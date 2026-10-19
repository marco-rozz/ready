// Package llm provides a factory for creating Eino ChatModel instances from configuration.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino-ext/components/model/claude"
	"github.com/cloudwego/eino-ext/components/model/gemini"
	"github.com/cloudwego/eino-ext/components/model/ollama"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino-ext/components/model/openrouter"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/codex"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
	"google.golang.org/genai"
)

// Compile-time interface assertions.
var (
	_ model.ToolCallingChatModel = (*ollama.ChatModel)(nil)
	_ model.ToolCallingChatModel = (*openrouter.ChatModel)(nil)
	_ model.ToolCallingChatModel = (*openai.ChatModel)(nil)
	_ model.ToolCallingChatModel = (*claude.ChatModel)(nil)
	_ model.ToolCallingChatModel = (*gemini.ChatModel)(nil)
	_ model.AgenticModel         = (*agenticopenai.ResponsesModel)(nil)
	_ model.AgenticModel         = (*codex.Model)(nil)
)

const (
	// defaultOllamaBaseURL is the default endpoint for a local Ollama instance.
	defaultOllamaBaseURL = "http://localhost:11434"

	// defaultOpenCodeBaseURL is the default base URL for the OpenCode Zen gateway.
	defaultOpenCodeBaseURL = "https://opencode.ai/zen/v1"

	// defaultOpenRouterBaseURL is the default endpoint for OpenRouter.
	defaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"

	// streaming transport deadlines (D3): connection establishment and
	// response-header arrival are bounded; the body read is NOT — the
	// request context (attempt budget + outer agent deadline) is the only
	// authority over stream duration.
	dialTimeout         = 30 * time.Second
	tlsHandshakeTimeout = 10 * time.Second
	headerTimeout       = 120 * time.Second

	// claudeMaxTokens is the fixed default MaxTokens for the claude component
	// (Anthropic API rejects zero), per D4 in the llm-model-registry-1 design.
	claudeMaxTokens = 8192
)

// ErrChainExhausted is returned by the failover orchestrator when a turn has
// attempted every eligible candidate without success (task 3.5). It wraps
// the last candidate's error so errors.Is matches the sentinel while the
// underlying cause stays inspectable via errors.Unwrap. llm-status-visibility
// maps it to a distinct unavailable error code; every other failure stays
// generic.
var ErrChainExhausted = errors.New("llm candidate chain exhausted")

// CandidateDescriptor is one immutable entry of the ordered candidate chain
// compiled from llm.default + llm.failover (task 1.6). Descriptor order is
// the declared order: default first, then failover entries in sequence.
// Descriptors never change after compilation; all mutable state (health,
// cooldown, verified) lives in the orchestrator.
type CandidateDescriptor struct {
	// Ref is the full `<provider>::<model>` reference.
	Ref string
	// Provider is the provider instance name (the map key under
	// llm.providers) the reference resolves to.
	Provider string
	// ModelID is the model-id portion of the reference, forwarded verbatim.
	ModelID string
	// API is the wire protocol of the provider instance.
	API string
	// Layout is the prompt-layout capability of the instance's wire protocol.
	Layout PromptLayout
}

// CompileCandidates validates global registry shape and compiles the
// immutable ordered candidate descriptor list `[default, failover...]`
// (tasks 1.5/1.6). The errors returned here are FATAL global registry
// errors — an unparseable reference, a reference naming an undeclared
// provider, an empty model-id, or a duplicate reference — and fail boot
// without probing anything. Per-candidate construction/credential/readiness
// failures are deliberately NOT evaluated here: they mark only the affected
// candidate unhealthy and are owned by the orchestrator's boot path.
// config.Validate checks the same global shape earlier; the redundancy is
// defensive so a directly-constructed LLMConfig (tests, SDK callers) that
// bypassed Load+Validate still cannot produce a half-valid chain.
func CompileCandidates(registry *config.LLMConfig) ([]CandidateDescriptor, error) {
	refs := make([]string, 0, 1+len(registry.Failover))
	refs = append(refs, registry.Default)
	refs = append(refs, registry.Failover...)

	descs := make([]CandidateDescriptor, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		providerName, modelID, err := config.ParseModelReference(ref)
		if err != nil {
			return nil, fmt.Errorf("config error for model reference %q: %w", ref, err)
		}

		providerCfg, ok := registry.Providers[providerName]
		if !ok {
			return nil, fmt.Errorf("config error: model reference %q names provider %q which is not declared under llm.providers", ref, providerName) // user-facing config error
		}

		if modelID == "" {
			return nil, fmt.Errorf("config error: model reference %q has an empty model-id", ref) // user-facing config error
		}

		if seen[ref] {
			return nil, fmt.Errorf("config error: duplicate llm reference %q across default/failover", ref) // user-facing config error
		}
		seen[ref] = true

		descs = append(descs, CandidateDescriptor{
			Ref:      ref,
			Provider: providerName,
			ModelID:  modelID,
			API:      providerCfg.API,
			Layout:   promptLayoutFor(&providerCfg),
		})
	}

	return descs, nil
}

// BuildForReference builds the ResolvedModel for a single reference without
// probing. A returned error is a PER-CANDIDATE failure (construction or
// credential: unreadable api_key_file, unsupported api, provider build
// error), never a fatal global registry error — the caller marks only this
// candidate unhealthy and continues with the rest of the chain. Identical
// to the boot construction path so a post-switch model is behaviourally
// identical to a boot-time model for that reference (including the
// OpenRouter stream-safe adapter).
func BuildForReference(ctx context.Context, registry *config.LLMConfig, ref string) (ResolvedModel, error) {
	providerName, modelID, err := config.ParseModelReference(ref)
	if err != nil {
		return ResolvedModel{}, fmt.Errorf("config error for model reference %q: %w", ref, err)
	}

	providerCfg, ok := registry.Providers[providerName]
	if !ok {
		return ResolvedModel{}, fmt.Errorf("config error: model reference %q names provider %q which is not declared under llm.providers", ref, providerName) // user-facing config error
	}
	// Config.Load records the map key on each provider, but callers that build
	// an in-memory registry (notably tests and embedders) may omit it. Codex
	// uses this stable instance key to select its namespaced OAuth credential.
	if providerCfg.Name == "" {
		providerCfg.Name = providerName
	}

	var key string
	if providerCfg.Provider != config.ProviderOpenAICodex {
		var keyErr error
		key, keyErr = providerCfg.ResolvedAPIKey(ctx)
		if keyErr != nil {
			return ResolvedModel{}, fmt.Errorf("config error for model reference %q: %w", ref, keyErr)
		}
	}

	resolved, buildErr := buildModel(ctx, &providerCfg, modelID, key, registry.OAuthPath)
	if buildErr != nil {
		return ResolvedModel{}, fmt.Errorf("config error for model reference %q: %w", ref, buildErr)
	}

	return resolved, nil
}

// PreflightCatalog runs the model-catalog preflight for one reference: an
// idempotent HTTP GET verifying connectivity, authentication, and that the
// configured model-id appears in the provider's catalog (task 4.6 — the
// catalog check is retained as a preflight; readiness itself is established
// by the orchestrator's synthetic tool-call probe). Returns nil when the
// entry may proceed (2xx catalog containing the model-id, or an inconclusive
// catalog shape), or the reason the entry should be considered not ready
// (transport error, auth rejected, model-id absent). Never returns an error
// for a merely unexpected HTTP status — inconclusive results proceed.
// criticalities-remediation (M12): the probe budget honours
// llm.recovery.probe_timeout — a cold local model (e.g. gemma4:31b loading
// from disk) is not spuriously failed by a hardcoded 10 s bound. The
// registry argument carries the configured value; <= 0 falls back to the
// built-in default.
func PreflightCatalog(ctx context.Context, registry *config.LLMConfig, ref string, logger zerolog.Logger) error {
	providerName, modelID, err := config.ParseModelReference(ref)
	if err != nil {
		return fmt.Errorf("config error for model reference %q: %w", ref, err)
	}

	providerCfg, ok := registry.Providers[providerName]
	if !ok {
		return fmt.Errorf("config error: model reference %q names provider %q which is not declared under llm.providers", ref, providerName) // user-facing config error
	}

	if providerCfg.Provider == config.ProviderOpenAICodex {
		return preflightCodexCredential(ctx, registry.OAuthPath, providerName)
	}

	key, keyErr := providerCfg.ResolvedAPIKey(ctx)
	if keyErr != nil {
		return fmt.Errorf("config error for model reference %q: %w", ref, keyErr)
	}

	probeTimeout := llmProbeTimeout(registry)

	outcome, probeErr := probeEntry(ctx, &providerCfg, ref, modelID, key, probeTimeout, logger)
	switch outcome {
	case probeOK, probeInconclusive:
		return nil
	default:
		return probeErr
	}
}

// defaultProbeTimeout is the built-in boot-probe budget used when
// llm.recovery.probe_timeout is not configured.
const defaultProbeTimeout = 10 * time.Second

// llmProbeTimeout resolves the effective probe timeout: the configured
// llm.recovery.probe_timeout when set, otherwise the built-in default.
func llmProbeTimeout(registry *config.LLMConfig) time.Duration {
	if registry != nil && registry.ResolvedRecovery().ProbeTimeout > 0 {
		return registry.ResolvedRecovery().ProbeTimeout
	}

	return defaultProbeTimeout
}

// ResolvedModel is a tagged union of the two ChatModel kinds Resolve can
// produce, one per model entry, dispatched on the entry's wire protocol:
//
//   - Chat is a model.ToolCallingChatModel, used by every protocol except
//     `responses` (chat_completions, anthropic, google, plus ollama and
//     openrouter). Consumers build the agent via adk.NewChatModelAgent.
//   - Agentic is a model.AgenticModel, used by the `responses` protocol
//     (eino-ext/components/model/agenticopenai). Consumers build the agent
//     via adk.NewTypedChatModelAgent[*schema.AgenticMessage].
//
// Exactly one field is non-nil. The registry returns a tagged union instead
// of two resolvers because a single failover loop must cover both model kinds.
type ResolvedModel struct {
	Chat    model.ToolCallingChatModel
	Agentic model.AgenticModel
	layout  PromptLayout
	// contextWindow is the operator-declared context-window size in tokens
	// (0 = not configured). Carried so the /context estimator and the
	// occupancy percentage use the real denominator instead of a guess.
	contextWindow int
}

// IsAgentic reports whether this ResolvedModel was built via the `responses`
// protocol (Agentic non-nil) rather than a ToolCallingChatModel protocol.
func (r ResolvedModel) IsAgentic() bool {
	return r.Agentic != nil
}

// ContextWindow reports the configured context-window size in tokens, or 0
// when the provider does not declare one (occupancy stays absent — never
// estimated).
func (r ResolvedModel) ContextWindow() int {
	return r.contextWindow
}

// Layout reports the prompt-layout capability of this ResolvedModel's wire
// protocol.
func (r ResolvedModel) Layout() PromptLayout {
	return r.layout
}

// PromptLayout is the KV-cache-prefix capability of a provider's wire
// protocol: whether per-turn dynamic content can be appended after the
// conversation history (LayoutTailDynamic) or must stay at the head
// (LayoutHeadSafe).
type PromptLayout int

const (
	// LayoutTailDynamic: the wire format carries positional system/user
	// messages, so per-turn dynamic content can be appended after history
	// without invalidating a prefix cache.
	LayoutTailDynamic PromptLayout = iota
	// LayoutHeadSafe: the wire format hoists system content into a
	// dedicated top-level field (Anthropic `system`, Gemini
	// `systemInstruction`) and/or rejects non-user/assistant roles in the
	// message array. Dynamic content stays at the head.
	LayoutHeadSafe
)

// String renders the layout name used in logs (`tail-dynamic` /
// `head-safe`).
func (l PromptLayout) String() string {
	if l == LayoutTailDynamic {
		return "tail-dynamic"
	}

	return "head-safe"
}

// promptLayoutFor derives the prompt-layout capability from a provider's
// wire protocol. Ollama and OpenRouter carry positional
// system messages (confirmed for the Gemma-family chat template in
// a KV-cache prompt-layout spike); OpenCode's `chat_completions` and
// `responses` APIs are likewise positional. `anthropic` and `google` hoist
// system content into a dedicated field and/or reject a trailing
// non-user/assistant role — an unrecognised provider or
// API kind fails safe to LayoutHeadSafe rather than risking an HTTP 400 on
// every turn.
func promptLayoutFor(providerCfg *config.LLMProviderConfig) PromptLayout {
	switch providerCfg.Provider {
	case config.ProviderOllama, config.ProviderOpenRouter:
		return LayoutTailDynamic
	case config.ProviderOpenCode:
		switch providerCfg.API {
		case config.APIChatCompletions, config.APIResponses:
			return LayoutTailDynamic
		default:
			return LayoutHeadSafe
		}
	case config.ProviderOpenAICodex:
		return LayoutTailDynamic
	default:
		return LayoutHeadSafe
	}
}

// buildModel constructs the appropriate model for a single reference — a
// ToolCallingChatModel for every protocol except `responses`, or an
// AgenticModel for `responses`. It returns a config error for invalid
// configurations. The caller is responsible for resolving the API key via
// LLMProviderConfig.ResolvedAPIKey(ctx) before calling buildModel (D7: read
// once at boot).
func buildModel(ctx context.Context, providerCfg *config.LLMProviderConfig, modelID, key string, oauthPaths ...string) (ResolvedModel, error) {
	// Re-validate provider-level config that config.Validate() also checks.
	// This ensures Resolve() from tests without Load+Validate also catches
	// config errors before any connection attempt.
	if providerCfg.Provider == "" {
		return ResolvedModel{}, fmt.Errorf("provider is required")
	}
	if modelID == "" {
		return ResolvedModel{}, fmt.Errorf("model-id is required")
	}

	hasKey := providerCfg.APIKey != ""
	hasFile := providerCfg.APIKeyFile != ""

	if hasKey && hasFile {
		return ResolvedModel{}, fmt.Errorf("api_key and api_key_file are mutually exclusive")
	}

	if key == "" && providerCfg.Provider != config.ProviderOllama && providerCfg.Provider != config.ProviderOpenAICodex {
		return ResolvedModel{}, fmt.Errorf("api_key or api_key_file is required for provider %q", providerCfg.Provider)
	}

	// The context window is provider-level (tui-usage-observability): every
	// construction path carries it on the resolved model for /context and
	// the occupancy denominator.
	withWindow := func(r ResolvedModel) ResolvedModel {
		r.contextWindow = providerCfg.ContextWindow

		return r
	}

	switch providerCfg.Provider {
	case config.ProviderOllama:
		chatModel, err := buildOllama(ctx, providerCfg, modelID, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return withWindow(ResolvedModel{Chat: chatModel, layout: promptLayoutFor(providerCfg)}), nil
	case config.ProviderOpenRouter:
		chatModel, err := buildOpenRouter(ctx, providerCfg, modelID, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return withWindow(ResolvedModel{Chat: chatModel, layout: promptLayoutFor(providerCfg)}), nil
	case config.ProviderOpenCode:
		r, err := buildOpenCode(ctx, providerCfg, modelID, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return withWindow(r), nil
	case config.ProviderOpenAICodex:
		var oauthPath string
		if len(oauthPaths) > 0 {
			oauthPath = oauthPaths[0]
		}
		agenticModel, err := buildOpenAICodex(ctx, providerCfg, modelID, oauthPath)
		if err != nil {
			return ResolvedModel{}, err
		}

		return withWindow(ResolvedModel{Agentic: agenticModel, layout: LayoutTailDynamic}), nil
	default:
		return ResolvedModel{}, fmt.Errorf("unsupported provider %q", providerCfg.Provider)
	}
}

func buildOpenAICodex(ctx context.Context, entry *config.LLMProviderConfig, modelID, oauthPath string) (model.AgenticModel, error) {
	if entry.API != config.APIResponses {
		return nil, fmt.Errorf("openai-codex requires api %q", config.APIResponses)
	}
	// One OAuth-store resolver: config.Load resolves the store (Install-home
	// default or explicit oauth.path); an empty path here means the caller
	// bypassed it and there is no silent fallback to a relative directory.
	if oauthPath == "" {
		return nil, fmt.Errorf("openai-codex provider %q: oauth credential store path is empty — load the config through config.Load or set oauth.path", entry.Name)
	}
	store, err := codexauth.NewFileStore(ctx, oauthPath)
	if err != nil {
		return nil, err
	}
	cred, err := store.Load(ctx, entry.Name)
	if err != nil {
		return nil, err
	}
	if cred == nil || !cred.Usable(entry.Name, time.Now(), 30*time.Second) {
		return nil, fmt.Errorf("%w for provider %q; run `ready login %s`", codexauth.ErrLoginRequired, entry.Name, entry.Name)
	}
	authClient := codexauth.NewClient(streamSafeHTTPClient(nil))
	tokenSource, err := codexauth.NewTokenSource(store, authClient, entry.Name, cred)
	if err != nil {
		return nil, err
	}

	return codex.NewModel(ctx, codex.Config{
		BaseURL: entry.BaseURL, Model: modelID, TokenSource: tokenSource,
		HTTPClient: streamSafeHTTPClient(nil),
	})
}

func preflightCodexCredential(ctx context.Context, oauthPath, providerName string) error {
	if oauthPath == "" {
		return fmt.Errorf("openai-codex provider %q: oauth credential store path is empty — load the config through config.Load or set oauth.path", providerName)
	}
	store, err := codexauth.NewFileStore(ctx, oauthPath)
	if err != nil {
		return err
	}
	cred, err := store.Load(ctx, providerName)
	if err != nil {
		return err
	}
	if cred == nil || !cred.Usable(providerName, time.Now(), 30*time.Second) {
		return fmt.Errorf("%w for provider %q; run `ready login %s`", codexauth.ErrLoginRequired, providerName, providerName)
	}

	return nil
}

// streamSafeTransport returns a copy of http.DefaultTransport with deadlines
// on connection establishment, TLS handshake and response-header arrival —
// and NO whole-request timeout: the body read is bounded only by the request
// context (D3, criticalities-remediation H5).
func streamSafeTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Defensive: DefaultTransport is always *http.Transport — fall back
		// to a plain transport with the same dial/header deadlines.
		base = &http.Transport{}
	}

	scoped := base.Clone()
	scoped.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	scoped.TLSHandshakeTimeout = tlsHandshakeTimeout
	scoped.ResponseHeaderTimeout = headerTimeout

	return scoped
}

// streamSafeHTTPClient returns an HTTP client whose transport carries the
// stream-safe deadlines and no whole-request timeout (D3). Callers that need
// injected headers/auth wrap the transport before handing it here — pass the
// already-wrapped RoundTripper as inner (it must chain into a scoped
// transport via streamSafeTransport()).
func streamSafeHTTPClient(inner http.RoundTripper) *http.Client {
	if inner == nil {
		inner = streamSafeTransport()
	}

	return &http.Client{Transport: inner}
}

// buildOllama creates an Ollama-backed ChatModel. keep_alive and options are
// read from the provider instance and applied to every model referenced
// through it (per-model Ollama tuning is not supported — the design doc).
func buildOllama(ctx context.Context, entry *config.LLMProviderConfig, modelID, key string) (model.ToolCallingChatModel, error) {
	baseURL := entry.BaseURL
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}

	ollamaCfg := &ollama.ChatModelConfig{
		BaseURL: baseURL,
		Model:   modelID,
	}

	// Ollama-only generation tuning. KeepAlive and Options are nil when the
	// operator omits them (validated to be ollama-only by config.Validate),
	// in which case Ollama applies its own defaults.
	if entry.KeepAlive != nil {
		ka := *entry.KeepAlive
		ollamaCfg.KeepAlive = &ka
	}
	if opts := ollamaAPIOptions(entry.Options); opts != nil {
		ollamaCfg.Options = opts
	}

	if key != "" {
		// H5 (D3): stream-safe client — no http.Client.Timeout; the auth header
		// wraps a scoped transport with dial/TLS/header deadlines only.
		ollamaCfg.HTTPClient = streamSafeHTTPClient(&authTransport{
			base:  streamSafeTransport(),
			token: key,
		})
	} else {
		ollamaCfg.HTTPClient = streamSafeHTTPClient(nil)
	}

	chatModel, err := ollama.NewChatModel(ctx, ollamaCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama model: %w", err)
	}

	return chatModel, nil
}

// buildOpenRouter creates an OpenRouter-backed ChatModel.
func buildOpenRouter(ctx context.Context, entry *config.LLMProviderConfig, modelID, key string) (model.ToolCallingChatModel, error) {
	if key == "" {
		return nil, fmt.Errorf("api_key is required for openrouter")
	}

	orConfig := &openrouter.Config{
		APIKey: key,
		Model:  modelID,
		// H5 (D3): stream-safe client instead of Config.Timeout (which maps to
		// http.Client.Timeout and would kill streams longer than the timeout).
		HTTPClient: streamSafeHTTPClient(nil),
	}

	if entry.BaseURL != "" {
		orConfig.BaseURL = entry.BaseURL
	}

	chatModel, err := openrouter.NewChatModel(ctx, orConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create OpenRouter model: %w", err)
	}

	// Wrap OpenRouter in a stream-safe adapter: see openrouterStreamSafe.
	return &openrouterStreamSafe{inner: chatModel}, nil
}

// openrouterStreamSafe wraps an OpenRouter ChatModel so that Stream is served
// from a single Generate call instead of the component's native streaming
// path. This is a contained workaround for an upstream nil-pointer panic in
// eino-ext openrouter: buildResponseChunkMessageModifier (unconditionally
// registered while streaming, chatmodel.go:432) dereferences
// msg.ResponseMeta, but the shared ACL stream loop
// (libs/acl/openai@v0.1.17/chat_model.go:873) invokes that modifier with
// msg=nil on EOF whenever the last content frame was non-empty — the common
// case when the gateway sends no separate trailing usage/empty frame.
//
// Routing Stream through Generate sidesteps the broken modifier path entirely
// while preserving the model.ToolCallingChatModel contract (tools, options,
// and the full Generate response are forwarded unchanged). Only OpenRouter
// loses incremental streaming; every other provider keeps real streaming.
// Remove once upstream guards nil msg in the chunk modifier.
type openrouterStreamSafe struct {
	inner model.ToolCallingChatModel
}

// Guarantees openrouterStreamSafe satisfies model.ToolCallingChatModel.
var _ model.ToolCallingChatModel = (*openrouterStreamSafe)(nil)

// Generate forwards to the inner OpenRouter model (Generate is unaffected by
// the streaming modifier bug).
func (w *openrouterStreamSafe) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return w.inner.Generate(ctx, input, opts...)
}

// Stream serves a single chunk produced by Generate. The chunk carries the
// complete response; the reader then yields EOF. This avoids the OpenRouter
// streaming modifier that panics on the EOF nil-message call.
func (w *openrouterStreamSafe) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := w.inner.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		if msg != nil {
			sw.Send(msg, nil)
		}
	}()

	return sr, nil
}

// WithTools re-wraps the tools-attached variant so the override survives the
// agent's WithTools().Stream() call chain.
func (w *openrouterStreamSafe) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := w.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}

	return &openrouterStreamSafe{inner: inner}, nil
}

// buildOpenCode creates a model targeting the OpenCode Zen gateway. It
// returns a ResolvedModel with Chat populated for every protocol except
// `responses`, which populates Agentic instead (see ResolvedModel).
func buildOpenCode(ctx context.Context, entry *config.LLMProviderConfig, modelID, key string) (ResolvedModel, error) {
	baseURL := entry.BaseURL
	if baseURL == "" {
		baseURL = defaultOpenCodeBaseURL
	}

	layout := promptLayoutFor(entry)

	switch entry.API {
	case config.APIChatCompletions:
		chatModel, err := buildOpenCodeChatCompletions(ctx, modelID, baseURL, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return ResolvedModel{Chat: chatModel, layout: layout}, nil
	case config.APIAnthropic:
		chatModel, err := buildOpenCodeAnthropic(ctx, modelID, baseURL, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return ResolvedModel{Chat: chatModel, layout: layout}, nil
	case config.APIGoogle:
		chatModel, err := buildOpenCodeGoogle(ctx, modelID, baseURL, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return ResolvedModel{Chat: chatModel, layout: layout}, nil
	case config.APIResponses:
		agenticModel, err := buildOpenCodeResponses(ctx, modelID, baseURL, key)
		if err != nil {
			return ResolvedModel{}, err
		}

		return ResolvedModel{Agentic: agenticModel, layout: layout}, nil
	default:
		return ResolvedModel{}, fmt.Errorf("unsupported api %q for opencode", entry.API)
	}
}

// buildOpenCodeChatCompletions creates an OpenAI-compatible ChatModel pointed
// at the OpenCode Zen gateway. The HTTP client carries the OpenCode session
// headers (x-opencode-session is REQUIRED by the Go subscription gateway,
// ignored by plain Zen — see opencode_session.go).
func buildOpenCodeChatCompletions(ctx context.Context, modelName, baseURL, key string) (model.ToolCallingChatModel, error) {
	cfg := &openai.ChatModelConfig{
		BaseURL:    baseURL,
		APIKey:     key,
		Model:      modelName,
		HTTPClient: zenSessionHTTPClient(),
	}

	chatModel, err := openai.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create opencode chat_completions model: %w", err)
	}

	return chatModel, nil
}

// buildOpenCodeAnthropic creates a Claude-compatible ChatModel pointed at the
// OpenCode Zen gateway.
func buildOpenCodeAnthropic(ctx context.Context, modelName, baseURL, key string) (model.ToolCallingChatModel, error) {
	baseURLPtr := &baseURL

	cfg := &claude.Config{
		BaseURL:    baseURLPtr,
		AuthToken:  key, // D4: AuthToken (Bearer), never APIKey
		Model:      modelName,
		MaxTokens:  claudeMaxTokens,
		HTTPClient: zenSessionHTTPClient(), // OpenCode session headers, see opencode_session.go
	}

	chatModel, err := claude.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create opencode anthropic model: %w", err)
	}

	return chatModel, nil
}

// buildOpenCodeGoogle creates a Gemini-native ChatModel pointed at the OpenCode
// Zen gateway. The eino-ext gemini component wraps google.golang.org/genai,// which talks Google's generateContent wire format and implements
// ToolCallingChatModel, so the agent wiring is unchanged.
//
// The genai client is built for the Gemini API backend with BaseURL pointed
// at the gateway and an Authorization: Bearer header injected via
// HTTPOptions.Headers (Zen authenticates with Bearer tokens, like its
// chat_completions and anthropic entry points). The APIKey is also set so the
// genai client does not reject the config at construction; it surfaces as an
// x-goog-api-key header, which Zen ignores in favour of Authorization.
func buildOpenCodeGoogle(ctx context.Context, modelName, baseURL, key string) (model.ToolCallingChatModel, error) {
	if key == "" {
		return nil, fmt.Errorf("api_key is required for opencode google")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  key,
		Backend: genai.BackendGeminiAPI,
		// H5 (D3): no HTTPOptions.Timeout (the genai SDK wraps it in a
		// per-request context deadline that would kill long streams) and a
		// stream-safe HTTP client instead.
		HTTPClient: streamSafeHTTPClient(nil),
		HTTPOptions: genai.HTTPOptions{
			BaseURL: baseURL,
			Headers: http.Header{
				"Authorization":  []string{"Bearer " + key},
				zenSessionHeader: []string{zenSessionID()}, // OpenCode session, see opencode_session.go
				"User-Agent":     []string{zenUserAgent},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create google genai client: %w", err)
	}

	cfg := &gemini.Config{
		Client: client,
		Model:  modelName,
	}

	chatModel, err := gemini.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create opencode google model: %w", err)
	}

	return chatModel, nil
}

// buildOpenCodeResponses creates an agentic model targeting the OpenAI
// Responses API surface of the OpenCode Zen gateway, via
// eino-ext/components/model/agenticopenai's ResponsesModel. Unlike the other
// opencode builders, this returns a model.AgenticModel (BaseModel[*schema.AgenticMessage]),
// not a model.ToolCallingChatModel — the caller (buildOpenCode) wraps it in
// ResolvedModel.Agentic. Auth follows the same Bearer-token convention as the
// other opencode protocols: ResponsesConfig.APIKey is forwarded to the
// OpenAI SDK's option.WithAPIKey, which sets Authorization: Bearer <key>.
func buildOpenCodeResponses(ctx context.Context, modelName, baseURL, key string) (model.AgenticModel, error) {
	if key == "" {
		return nil, fmt.Errorf("api_key is required for opencode responses")
	}

	agenticModel, err := agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{
		BaseURL: baseURL,
		APIKey:  key,
		Model:   modelName,
		// H5 (D3): stream-safe client instead of Timeout (same semantics as
		// the other builders).
		HTTPClient:    streamSafeHTTPClient(nil),
		CustomHeaders: zenSessionCustomHeaders(), // OpenCode session, see opencode_session.go
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create opencode responses model: %w", err)
	}

	return agenticModel, nil
}

// probeOutcome classifies the result of a single boot probe.
type probeOutcome int

const (
	probeOK           probeOutcome = iota // 2xx — accept the entry
	probeInconclusive                     // any other status — boot with this entry + WARN
	probeFailed                           // transport error or 401/403 — fail over
)

// probeEntry performs a lightweight, idempotent HTTP GET to detect
// connectivity, authentication, and model-id existence for a model
// reference. The caller provides the pre-resolved API key (D7: read once at
// boot), the label to use in log messages (the full `<provider>::<model>`
// reference), and the model-id whose presence in the provider's catalog must
// be verified.
//
// The probe URL is derived from the provider entry's base_url — unchanged
// logic: `{base}/api/tags` for ollama, `{base}/models` otherwise.
//
// Classification:
//   - transport error / 401/403 → probeFailed + logs a WARN
//   - 2xx with a parseable catalog containing the model-id → probeOK
//   - 2xx with a parseable catalog NOT containing the model-id → probeFailed
//   - logs a WARN naming the missing model-id
//   - 2xx with an unparseable body → probeInconclusive + logs a WARN (a flaky
//     catalog shape never gates boot harder than connectivity alone)
//   - any other status → probeInconclusive + logs a WARN
func probeEntry(ctx context.Context, entry *config.LLMProviderConfig, label, modelID, key string, probeTimeout time.Duration, logger zerolog.Logger) (probeOutcome, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var probeURL string
	baseURL := entry.BaseURL

	switch entry.Provider {
	case config.ProviderOllama:
		if baseURL == "" {
			baseURL = defaultOllamaBaseURL
		}
		probeURL = baseURL + "/api/tags"
	case config.ProviderOpenRouter:
		if baseURL == "" {
			baseURL = defaultOpenRouterBaseURL
		}
		probeURL = baseURL + "/models"
	case config.ProviderOpenAICodex:
		return probeInconclusive, nil
	default:
		// opencode (chat_completions, anthropic, google, or responses) all
		// probe at {base}/models
		if baseURL == "" {
			baseURL = defaultOpenCodeBaseURL
		}
		probeURL = baseURL + "/models"
	}

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		logger.Warn().Str("model", label).Err(err).Msg("LLM boot probe request error, skipping entry")

		return probeFailed, fmt.Errorf("entry %q probe request error: %w", label, err)
	}

	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// Transport error (DNS, refused, timeout).
		logger.Warn().Str("model", label).Err(err).Msg("LLM boot probe transport error, skipping entry")

		return probeFailed, fmt.Errorf("entry %q probe transport error: %w", label, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Inspect the catalog body to verify the configured model-id is
		// present. An unparseable body is inconclusive (boot through) so a
		// flaky catalog shape does not gate boot.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
		if readErr != nil {
			logger.Warn().
				Str("model", label).
				Err(readErr).
				Msg("LLM boot probe catalog read error, booting with this entry")

			return probeInconclusive, nil
		}

		catalog, parsed := parseCatalog(entry, body)
		if !parsed {
			logger.Warn().
				Str("model", label).
				Msg("LLM boot probe catalog unparseable, booting with this entry")

			return probeInconclusive, nil
		}

		if !catalogContains(catalog, modelID) {
			logger.Warn().
				Str("model", label).
				Str("model_id", modelID).
				Msg("LLM boot probe model-id absent from catalog, skipping entry")

			return probeFailed, fmt.Errorf("entry %q probe model-id %q not found in catalog", label, modelID)
		}

		return probeOK, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		logger.Warn().
			Str("model", label).
			Int("http_status", resp.StatusCode).
			Msg("LLM boot probe authentication rejected, skipping entry")

		return probeFailed, fmt.Errorf("entry %q probe authentication failed: HTTP %d", label, resp.StatusCode)
	default:
		// Inconclusive — boot with this entry (the probe is a failover
		// accelerator, never a gate).
		logger.Warn().
			Str("model", label).
			Int("http_status", resp.StatusCode).
			Msg("LLM boot probe inconclusive, booting with this entry")

		return probeInconclusive, nil
	}
}

// probeBodyLimit caps how much of the models-listing response body the
// probe reads and parses. Catalogs are small (tens to low-thousands of
// entries); 1 MiB is generous while preventing an unbounded read on a
// misbehaving endpoint.
const probeBodyLimit = 1 << 20 // 1 MiB

// parseCatalog extracts the list of model identifiers from a models-listing
// response body. For ollama the shape is {"models":[{"name":...}]}; for
// every other provider (openrouter and all opencode protocols) it is
// {"data":[{"id":...}]}. Returns the identifiers and parsed=true when the
// body matches the expected shape (JSON unmarshal succeeds); returns
// parsed=false when the body cannot be parsed, in which case the caller treats
// the probe as inconclusive.
func parseCatalog(entry *config.LLMProviderConfig, body []byte) (models []string, parsed bool) {
	switch entry.Provider {
	case config.ProviderOllama:
		var payload struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, false
		}
		for _, m := range payload.Models {
			if m.Name != "" {
				models = append(models, m.Name)
			}
		}

		return models, true
	default:
		// openrouter and every opencode protocol (chat_completions,
		// anthropic, google, responses) expose an OpenAI-shaped /models
		// listing with a "data" array of {"id":...} entries.
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, false
		}
		for _, m := range payload.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}

		return models, true
	}
}

// catalogContains reports whether the model-id is present in the catalog.
// The match is exact (case-sensitive), matching provider wire semantics:
// Ollama tags and OpenRouter/OpenCode model-ids are case-sensitive.
func catalogContains(catalog []string, modelID string) bool {
	for _, m := range catalog {
		if m == modelID {
			return true
		}
	}

	return false
}

// ollamaAPIOptions converts the config-layer OllamaOptions into the
// eino-ext Ollama component's Options type. It returns nil when cfg is nil or
// when no field is set, so the component preserves Ollama's defaults for every
// omitted knob. Only set fields are populated; the api.Options struct uses
// `omitempty` JSON tags so unset (zero) fields are not sent to Ollama.
func ollamaAPIOptions(cfg *config.OllamaOptions) *ollama.Options {
	if cfg == nil {
		return nil
	}
	if cfg.Temperature == nil && cfg.TopP == nil && cfg.TopK == nil &&
		cfg.NumPredict == nil && cfg.NumCtx == nil && cfg.Seed == nil &&
		cfg.RepeatPenalty == nil && cfg.NumThread == nil {
		return nil
	}

	opts := &ollama.Options{}
	if cfg.Temperature != nil {
		opts.Temperature = *cfg.Temperature
	}
	if cfg.TopP != nil {
		opts.TopP = *cfg.TopP
	}
	if cfg.TopK != nil {
		opts.TopK = *cfg.TopK
	}
	if cfg.NumPredict != nil {
		opts.NumPredict = *cfg.NumPredict
	}
	if cfg.NumCtx != nil {
		opts.NumCtx = *cfg.NumCtx
	}
	if cfg.Seed != nil {
		opts.Seed = *cfg.Seed
	}
	if cfg.RepeatPenalty != nil {
		opts.RepeatPenalty = *cfg.RepeatPenalty
	}
	if cfg.NumThread != nil {
		opts.NumThread = *cfg.NumThread
	}

	return opts
}

// authTransport is an http.RoundTripper that injects a Bearer token into every request.
type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqCopy := req.Clone(req.Context())
	reqCopy.Header.Set("Authorization", "Bearer "+t.token)

	return t.base.RoundTrip(reqCopy)
}

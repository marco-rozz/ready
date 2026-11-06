// Package initwizard implements the deterministic guided-init console
// wizard and bounded offline repair of the guided-init-and-direct-secret-entry
// change. It asks exactly one provider's questions, validates every input
// locally (no model, no AI authorship), generates a starter configuration
// with editable defaults, creates private agent-local credential files
// and publishes — config last — without ever writing a credential TODO.
//
// The wizard is deliberately free of runtime dependencies: cmd/init.go is a
// thin dispatcher over Run.
package initwizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/marco-rozz/ready/internal/config"
)

// Sentinel outcomes of the wizard.
var (
	// ErrNotTerminal reports stdin/stdout are not a supported terminal.
	ErrNotTerminal = errors.New("guided init requires an interactive terminal (run init in a real console)")
	// ErrEOF reports end of input during the interview.
	ErrEOF = errors.New("input ended before the interview completed")
	// ErrCancelled reports a Ctrl+C during the interview.
	ErrCancelled = errors.New("cancelled")
	// ErrDeclinedRepair reports the operator declined the repair menu.
	ErrDeclinedRepair = errors.New("repair declined")
)

// Options configures one init invocation.
type Options struct {
	// ExplicitAgentDir is the raw --agent-dir value ("" for the default).
	ExplicitAgentDir string
	// NewIdentity gives an existing agent a fresh independent identity (the
	// way to make a copied agent directory a separate agent).
	NewIdentity bool
	// WorkingDir anchors the relative paths in the printed examples. Both
	// terminals must use this directory; the command is ready from PATH.
	WorkingDir string
	// Stdin/Stdout/Stderr are the interview streams (tests inject buffers).
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// StdinTTY: stdin is an interactive terminal. When false before any
	// fresh publication, init fails (the interview cannot run).
	StdinTTY bool
	// RandomHex is the injected entropy for the ZMQ token (32 bytes →
	// 64 hex chars). Production uses crypto/rand; tests inject a fixed
	// sequence.
	RandomHex func(n int) (string, error)
	// RunCodexLogin runs the in-process Codex OAuth flow for one agent
	// (deviceCode selects the headless flow). Production wires cmd/login's
	// library entry; tests inject nil to exercise the decline paths.
	RunCodexLogin func(ctx context.Context, agentDir string, deviceCode bool) error
	// ReadyGuideCheck overrides the shipped-library readiness check (test
	// seam; production checks ~/.local/share/ready/library/skills).
	ReadyGuideCheck func() bool
	// ReaderOverride replaces the prompt reader binding entirely (test
	// seam: piped interview streams with the TTY reader contract).
	ReaderOverride promptReader
}

func (o Options) stdout() io.Writer {
	if o.Stdout == nil {
		return io.Discard
	}

	return o.Stdout
}

// ProviderChoice is one wizard-completable provider interview answer set.
type ProviderChoice struct {
	// Provider is the config provider type name.
	Provider string
	// Instance is the provider-derived instance key for llm.providers.
	Instance string
	// API is the fixed or explicitly-asked wire protocol.
	API string
	// BaseURL is the validated configurable endpoint ("" for the fixed
	// Codex traffic endpoint, which the provider factory supplies).
	BaseURL string
	// ModelID is the model reference payload after the :: separator.
	ModelID string
	// APIKey holds the typed API key (masked input). It never lands in
	// YAML: fresh init writes it to <agent-dir>/secrets/<instance>.key.
	APIKey string
	// OllamaCloud selects direct hosted access, which requires an API key.
	// This is an interview choice; the saved config uses base_url/api_key_file.
	OllamaCloud bool
	// DeviceCode is the operator's explicit request of the headless
	// ChatGPT device flow during the init offer (Codex only).
	DeviceCode bool
	// SkippedProbe: the operator declined the optional synthetic probe.
	SkippedProbe bool
}

// allowedAPIsByProvider mirrors internal/config's wire-protocol allow-list
// (kept as the interview's local vocabulary; final authority is
// ValidateCandidate, which rejects anything inconsistent).
//
//nolint:gochecknoglobals // immutable interview vocabulary (matches config's own table style)
var allowedAPIsByProvider = map[string][]string{
	config.ProviderOllama:      {"ollama"},
	config.ProviderOpenRouter:  {apiChatCompletions},
	config.ProviderOpenCode:    {apiChatCompletions, apiAnthropic, apiGoogle, apiResponses},
	config.ProviderOpenAICodex: {apiResponses},
}

// Interview API vocabulary (naming satisfies goconst; final authority —
// config's own allow-list per provider type).
const (
	apiChatCompletions         = "chat_completions"
	apiAnthropic               = "anthropic"
	apiGoogle                  = "google"
	apiResponses               = "responses"
	defaultOllamaCloudEndpoint = "https://ollama.com"
)

// apiDefaultsByProvider are the derived fixed APIs: operators never type
// those (only opencode must answer explicitly).
//
//nolint:gochecknoglobals // immutable derived-API table
var apiDefaultsByProvider = map[string]string{
	config.ProviderOllama:      "ollama",
	config.ProviderOpenRouter:  apiChatCompletions,
	config.ProviderOpenAICodex: apiResponses,
}

// defaultEndpoints are the wizard's suggested configurable endpoints for the
// providers that expose one.
//
//nolint:gochecknoglobals // immutable endpoint suggestions
var defaultEndpoints = map[string]string{
	config.ProviderOllama:     "http://127.0.0.1:11434",
	config.ProviderOpenRouter: "https://openrouter.ai/api/v1",
	config.ProviderOpenCode:   "https://opencode.ai/zen/v1",
}

// defaultModels are the wizard's suggested model IDs (Enter accepts).
//
//nolint:gochecknoglobals // immutable model suggestion table
var defaultModels = map[string]string{
	config.ProviderOllama:      "gemma4:31b",
	config.ProviderOpenRouter:  "google/gemma-3-27b-it",
	config.ProviderOpenCode:    "gpt-5.1",
	config.ProviderOpenAICodex: "gpt-5.2-codex",
}

// keyRequiredByProvider reports whether the provider needs an API key
// (written to a private file, never inline).
//
//nolint:gochecknoglobals // immutable key-requirement table
var keyRequiredByProvider = map[string]bool{
	config.ProviderOpenRouter: true,
	config.ProviderOpenCode:   true,
}

func (p ProviderChoice) needsAPIKey() bool {
	return keyRequiredByProvider[p.Provider] ||
		(p.Provider == config.ProviderOllama && (p.OllamaCloud || p.APIKey != ""))
}

// validateProviderChoice checks the interview result against the same rules
// candidate validation will apply: exact provider type, allowed wire API,
// parseable endpoint and model, non-empty key when required.
func validateProviderChoice(p ProviderChoice) error {
	allowed, ok := allowedAPIsByProvider[p.Provider]
	if !ok {
		return fmt.Errorf("provider %q is not supported (valid: %s)", p.Provider, joinNames(validProviderNames()))
	}
	if !contains(allowed, p.API) {
		return fmt.Errorf("llm.providers.%s.api %q is not valid for provider %q (valid: %s)", p.Instance, p.API, p.Provider, joinNames(allowed))
	}
	if _, _, err := config.ParseModelReference(p.Instance + "::" + p.ModelID); err != nil {
		return fmt.Errorf("model id: %w", err)
	}
	if p.Provider != config.ProviderOpenAICodex {
		if err := validateEndpoint(p.BaseURL); err != nil {
			return err
		}
	}
	if p.needsAPIKey() && strings.TrimSpace(p.APIKey) == "" {
		return fmt.Errorf("llm.providers.%s.api_key is required for provider %q", p.Instance, p.Provider)
	}

	return nil
}

// validateEndpoint accepts exactly one parseable absolute http(s) URL with a
// host — the same shape every provider acceptance expects.
func validateEndpoint(raw string) error {
	u, ok := parseURL(raw)
	if !ok {
		return fmt.Errorf("endpoint %q is not a valid http(s) URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q is missing a host", raw)
	}

	return nil
}

// runInterview walks the fixed question tree and returns the answers.
// Nothing in here touches disk; EOF at any question and Ctrl+C at any
// moment abort the whole wizard with no side effects (echo restoration
// included — see prompt.go).
func runInterview(ctx context.Context, o Options) (ProviderChoice, error) {
	var i interview
	i.in = o.Stdin
	i.out = o.stdout()
	i.read = appPromptReader(o.StdinTTY, o.Stdin, o.stdout(), o.ReaderOverride)

	out := o.stdout()
	_, _ = io.WriteString(out, `Welcome to READY setup.
This wizard connects READY to one AI provider and model, and creates a
private access token for your chat client. It does not start READY yet.
It also saves starter settings for logs, conversations and skills.
After saving, it prepares your skill files from the installed shared library.
You can change these later in the configuration file or through admin chat.

Press Enter to accept a value in [brackets]. For yes/no questions,
[y/N] means Enter skips the optional step. Press Ctrl+C to cancel.
Nothing is saved until all required questions have been answered.

Step 1 of 4 — Choose your AI provider
  ollama        Your Ollama server, or direct Ollama Cloud access
  openrouter    Models accessed with an OpenRouter API key
  opencode      Models accessed with an OpenCode API key
  openai-codex  Experimental ChatGPT sign-in (no API key)

`)

	provider, err := i.askProvider(ctx)
	if err != nil {
		return ProviderChoice{}, err
	}

	choice := ProviderChoice{Provider: provider, Instance: provider, API: apiDefaultsByProvider[provider]}
	_, _ = io.WriteString(out, "\nStep 2 of 4 — Choose where READY connects\n")

	if provider == config.ProviderOllama {
		cloud, cerr := i.askOllamaCloud(ctx)
		if cerr != nil {
			return ProviderChoice{}, cerr
		}
		choice.OllamaCloud = cloud
	}

	if ep, ok := defaultEndpoints[provider]; ok {
		if choice.OllamaCloud {
			// The native client appends /api/chat and /api/tags itself.
			ep = defaultOllamaCloudEndpoint
		}
		_, _ = io.WriteString(out, "The endpoint is your provider's server address. Keep the suggestion unless you use a different server.\n")
		if provider == config.ProviderOllama {
			_, _ = io.WriteString(out, "For Ollama, enter only the host and optional port; READY adds /api/... itself.\n")
		}
		if provider == config.ProviderOpenCode {
			_, _ = io.WriteString(out, "OpenCode plans: Go (subscription) uses https://opencode.ai/zen/go/v1 — Zen (pay-per-use) keeps the default below.\n")
		}
		_, _ = fmt.Fprintf(out, "Endpoint [%s]: ", ep)
		raw, lerr := i.read.line(ctx)
		if lerr != nil {
			return ProviderChoice{}, lerr
		}
		choice.BaseURL = firstNonEmpty(raw, ep)
		if err := validateEndpoint(choice.BaseURL); err != nil {
			return ProviderChoice{}, fmt.Errorf("endpoint: %w", err)
		}
	}

	if provider == config.ProviderOpenCode {
		_, _ = io.WriteString(out, "Choose the API format supported by your OpenCode endpoint; the model name does not select it.\n")
		_, _ = io.WriteString(out, "API format (chat_completions, anthropic, google, responses) [chat_completions]: ")
		raw, lerr := i.read.line(ctx)
		if lerr != nil {
			return ProviderChoice{}, lerr
		}
		choice.API = firstNonEmpty(raw, apiChatCompletions)
		_, _ = fmt.Fprintf(out, "Selected API format: %s\n", choice.API)
	}
	if provider == config.ProviderOpenAICodex {
		_, _ = io.WriteString(out, "READY uses the fixed ChatGPT endpoint. You can sign in after the configuration is saved.\n")
	}

	_, _ = io.WriteString(out, "\nStep 3 of 4 — Choose the model\nUse the exact model ID listed by your provider. The suggestion must be available on your account or server.\n")
	if provider == config.ProviderOllama && !choice.OllamaCloud {
		_, _ = io.WriteString(out, "Your local Ollama server must be running, with this model already downloaded.\n")
	}
	_, _ = fmt.Fprintf(out, "Model id%s: ", defaultModelHint(provider))
	model, lerr := i.read.line(ctx)
	if lerr != nil {
		return ProviderChoice{}, lerr
	}
	choice.ModelID = firstNonEmpty(model, defaultModels[provider])
	choice.ModelID = strings.TrimSpace(choice.ModelID)
	if choice.ModelID == "" {
		return ProviderChoice{}, errors.New("a model id is required")
	}

	_, _ = io.WriteString(out, "\nStep 4 of 4 — Set up private credentials\nREADY generates the chat access token automatically; you do not need to invent or type one.\n")
	switch {
	case choice.needsAPIKey():
		_, _ = io.WriteString(out, "Create an API key in your provider account's API keys settings.\nPaste it here, then press Enter. Nothing appears while you type or paste.\nThe key is stored encrypted in this agent's secret store; never paste it into chat.\n")
		_, _ = fmt.Fprint(out, "API key (input hidden): ")
		key, serr := i.read.secret(ctx)
		if serr != nil {
			return ProviderChoice{}, serr
		}
		choice.APIKey = strings.TrimRight(key, "\r\n")
		if strings.TrimSpace(choice.APIKey) == "" {
			return ProviderChoice{}, fmt.Errorf("an API key is required for provider %q", provider)
		}
		_, _ = io.WriteString(out, "\n")
	case provider == config.ProviderOllama:
		_, _ = io.WriteString(out, "Local Ollama does not require an API key in this setup.\n")
	default:
		_, _ = io.WriteString(out, "ChatGPT sign-in replaces an API key; the next step will offer to sign in.\n")
	}

	if err := validateProviderChoice(choice); err != nil {
		return ProviderChoice{}, err
	}

	return choice, nil
}

func (i *interview) askOllamaCloud(ctx context.Context) (bool, error) {
	_, _ = io.WriteString(i.out, "Local uses an Ollama server on your computer or network. Cloud connects directly to Ollama's hosted service and requires an API key.\n")
	for {
		_, _ = io.WriteString(i.out, "Ollama deployment (local, cloud) [local]: ")
		raw, err := i.read.line(ctx)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "", "local":
			return false, nil
		case "cloud":
			return true, nil
		default:
			_, _ = io.WriteString(i.out, "  choose local or cloud\n")
		}
	}
}

// askProvider reads and normalizes the provider type answer.
func (i *interview) askProvider(ctx context.Context) (string, error) {
	names := validProviderNames()
	for {
		_, _ = fmt.Fprintf(i.out, "Provider (%s): ", strings.Join(names, ", "))
		raw, err := i.read.line(ctx)
		if err != nil {
			return "", err
		}
		v := strings.TrimSpace(raw)
		if contains(names, v) {
			return v, nil
		}
		_, _ = fmt.Fprintf(i.out, "  unsupported provider %q — type one of: %s\n", v, strings.Join(names, ", "))
	}
}

func defaultModelHint(provider string) string {
	if d, ok := defaultModels[provider]; ok {
		return " [" + d + "]"
	}

	return ""
}

func validProviderNames() []string {
	return []string{config.ProviderOllama, config.ProviderOpenRouter, config.ProviderOpenCode, config.ProviderOpenAICodex}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}

	return false
}

func joinNames(list []string) string { return strings.Join(list, ", ") }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}

	return ""
}

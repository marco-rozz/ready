package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/agent/toolnames"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/marco-rozz/ready/internal/formatter"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/logging"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/profile"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/skillssync"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/marco-rozz/ready/internal/twin"
	"github.com/marco-rozz/ready/internal/usage"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
)

// Compile-time assertions: verify that the concrete types satisfy the
// narrow interfaces required by the Formatter, Bootstrapper, and Snapshotter
// at the wiring point.
var (
	_ formatter.PathResolver  = (*skills.Manager)(nil)
	_ formatter.ContentLoader = (*skillcache.SkillCache)(nil)
	_ bootstrap.ContentLoader = (*skillcache.SkillCache)(nil)
	_ twin.SkillInvalidator   = (*bootstrap.Bootstrapper)(nil)
)

// channelFormatterCfg holds the formatter configuration for a channel.
type channelFormatterCfg struct {
	skillName string
	postStep  bool // true = post-step LLM call; false = inject into system prompt
}

// formatterRegistry maps a channel name to its formatter configuration.
type formatterRegistry map[string]channelFormatterCfg

// turnExecutor is the per-turn seam between the dispatcher and whatever
// serves a turn: the orchestrator (llm-failover-orchestrator) in production,
// or a plain-runner adapter in tests. The executor receives the turn's
// UNASSEMBLED parts — the assemble closure builds the full conversation per
// attempt from the attempting candidate's prompt layout (task 5.1/5.2), so
// prompt assembly runs once per attempt inside the executor.
type turnExecutor interface {
	ExecuteTurn(
		ctx context.Context,
		sessionKey string,
		assemble func(llm.PromptLayout) []*schema.Message,
		resumeReady func() bool,
		onChunk func(string),
	) (agent.TurnOutcome, error)
}

// plainRunnerExec adapts a single agent.Runner (and a fixed layout) to the
// turnExecutor seam — the test wiring and the shape the orchestrator itself
// implements. There is no failover on this path.
type plainRunnerExec struct {
	runner agent.Runner
	layout llm.PromptLayout
}

// Guarantees plainRunnerExec satisfies turnExecutor.
var _ turnExecutor = (*plainRunnerExec)(nil)

// ExecuteTurn assembles once with the fixed layout and runs the turn.
func (p *plainRunnerExec) ExecuteTurn(
	ctx context.Context,
	sessionKey string,
	assemble func(llm.PromptLayout) []*schema.Message,
	_ func() bool,
	onChunk func(string),
) (agent.TurnOutcome, error) {
	return p.runner.RunMessagesStreaming(ctx, sessionKey, assemble(p.layout), onChunk)
}

// agentRegistry holds the turn executor shared by all channels. In
// production it is the orchestrator-backed executor: one agent.Runner per
// candidate is built at boot (see buildLLMCandidates) and the orchestrator
// selects between them per attempt, so the dispatcher, history,
// and formatter pipeline stay protocol- and candidate-blind. When orch is
// non-nil, PromptLayout tracks promotions (task 6.5).
type agentRegistry struct {
	exec   turnExecutor
	orch   *orchestrator.Orchestrator // nil in tests
	layout llm.PromptLayout           // fallback layout when orch == nil
}

// PromptLayout reports the prompt-layout capability of the model currently
// serving this registry: the orchestrator's active candidate when wired,
// the fixed fallback layout otherwise.
func (r *agentRegistry) PromptLayout() llm.PromptLayout {
	if r.orch != nil {
		return r.orch.ActiveLayout()
	}

	return r.layout
}

// ServeOptions carries the command-line settings parsed by the `serve`
// subcommand. The field set is intentionally small — it mirrors the flags
// declared on the urfave/cli command so a future flag addition only touches
// `cmd/main.go` and this struct.
type ServeOptions struct {
	// ConfigPath is an explicit config file path. Optional: when empty, the
	// resolution chain elects ./.ready/config.yaml then ~/.ready/config.yaml.
	ConfigPath string
	// MockMCP selects the in-process MCP mock instead of a real Home
	// Assistant connection. Useful for local development and CI.
	MockMCP bool
	// Profile is the active MCP profile name from the --profile flag. Empty
	// means "not set on the command line"; resolution falls back to the
	// READY_MCP_PROFILE env var, then the mcp_profile config key.
	Profile string
}

// Serve runs the home-agent daemon: it loads configuration, wires the
// channel manager, agent, MCP runtime and persistence stack, then blocks
// until the supplied context is cancelled (typically by SIGINT/SIGTERM).
//
// The body is the historical content of main() with two adjustments:
//   - early failures (config load/validate, logger init) return a wrapped
//     error instead of calling os.Exit, so the urfave dispatcher can render
//     a single human stderr line and pick the exit code;
//   - signal handling is rooted in the supplied ctx via NotifyContext, so
//     tests and the dispatcher can cancel the daemon deterministically.
func Serve(ctx context.Context, opts ServeOptions) (serveErr error) {
	// Config resolution chain (install-home): the explicit --config wins with
	// no fallback, else ./.ready/config.yaml, else ~/.ready/config.yaml.
	resolution, err := config.Elect(ctx, opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("failed to elect config: %w", err)
	}

	cfg, migrationReport, err := prepareServeConfig(ctx, resolution.Path, opts, nil)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	defer func() {
		if serveErr != nil && migrationReport.Committed {
			serveErr = fmt.Errorf("%w%s", serveErr, migrationReport.CommitContext())
		}
	}()
	cfg.Origin = resolution.Origin

	activeProfileName, activeProfile, err := cfg.ResolveProfile(opts.Profile, os.Getenv("READY_MCP_PROFILE"))
	if err != nil {
		return fmt.Errorf("failed to resolve MCP profile: %w", err)
	}

	logRes, err := buildLogger(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialise logger: %w", err)
	}

	rootLogger, logCloser := logRes.Logger, logRes.Closer

	logger := rootLogger.With().Str("component", "main").Logger()
	logConfigMigration(logger, migrationReport)

	// Elected config visibility (install-home): the chain can pick the
	// cwd's ./.ready/config.yaml without the operator noticing — the boot
	// always names the file and how it was elected, and warns when a local
	// file is readable by group or others (never fatal, unix only).
	logger.Info().
		Str("config", cfg.ElectedPath).
		Str("origin", string(cfg.Origin)).
		Str("runtime_home", cfg.RuntimeHome).
		Msg("elected config")
	if w := warnConfigPermissions(cfg.ElectedPath); w != "" {
		logger.Warn().Msg(w)
	}

	logger.Info().
		Str("mcp_profile", activeProfileName).
		Strs("inject_skills", activeProfile.InjectSkills).
		Msg("active MCP profile resolved")

	// Warn when thoughts is enabled but the effective log level is too high.
	warnThoughtsLevelMismatch(rootLogger, cfg, logRes)

	// Secret files read through *_file configuration keys are permission-checked at
	// boot — a file readable by group or others WARNs naming the path.
	warnSecretFilePermissions(cfg, rootLogger)

	// Privacy guardrail: redact the user's home directory prefix (the
	// privacy-sensitive part of any absolute path, e.g. the username) to "~"
	// and the resolved runtime home (the elected config's directory) to a
	// neutral placeholder wherever filesystem paths cross into the LLM
	// context or out to the user. Deterministic prefix replacement — never
	// touches strings that merely resemble paths (MCP tool names, URLs,
	// dates).
	sanitizer := privacy.FromRun(cfg.RuntimeHome, nil)

	// Skill library sync (install-home D14): bring the runtime skill copy up
	// to date with the shipped library before the skills manager starts —
	// per file, three-way against the recorded state, so untouched files
	// receive shipped fixes and operator edits are never overwritten.
	if err = skillssync.Run(ctx, cfg.Skills, cfg.RuntimeHome, rootLogger); err != nil {
		return fmt.Errorf("failed to sync the skill library: %w", err)
	}

	skillsMgr, skillCache := mustInitInfra(ctx, cfg, rootLogger)

	// Start the HA digital twin (non-blocking; nil when home_assistant.enabled: false).
	// criticalities-remediation (LOW): twin.Init returns construction errors
	// instead of exiting the process from inside the library.
	haTwin, twinErr := twin.Init(ctx, cfg.HomeAssistant, rootLogger)
	if twinErr != nil {
		return fmt.Errorf("failed to start HA digital twin: %w", twinErr)
	}

	agentTools, mcpRuntime, blockedTools := buildTools(ctx, cfg, skillsMgr, skillCache, opts.MockMCP, activeProfileName, haTwin, rootLogger)

	// denied-tool-disclosure: when the active profile opts in,
	// the sentinel tool joins the agent's tool configuration (and is therefore
	// advertised) and the blocked set is loaded into the tool-name resolver's
	// ghost index. With the knob off, none of this happens — behavior is
	// byte-identical to the pre-disclosure daemon.
	discloseDenied := activeProfile.DiscloseDeniedTools
	if discloseDenied {
		agentTools = append(agentTools, toolnames.NewPolicyBlockTool())
		logger.Info().
			Str("mcp_profile", activeProfileName).
			Int("blocked_tools", len(blockedTools)).
			Msg("disclose_denied_tools enabled — blocked-call disclosure active")
	}

	resolver := buildToolNameResolver(ctx, agentTools, rootLogger, blockedTools, discloseDenied)

	// LLM orchestrator (change llm-failover-orchestrator): compile the
	// immutable ordered candidate chain, build one runner per candidate at
	// boot (both construction paths), then lazily evaluate readiness until
	// the first ready candidate. Fatal global registry errors (reference
	// shape, undeclared providers, duplicates) fail boot; per-candidate
	// construction/credential/readiness failures mark only that candidate
	// unhealthy — and when no candidate is ready the daemon still starts in
	// the orchestrator's unavailable state instead of failing boot.
	llmCands, fatalLLMErr := buildLLMCandidates(ctx, cfg, agentTools, rootLogger, resolver, activeProfileName, discloseDenied)
	if fatalLLMErr != nil {
		return fmt.Errorf("invalid LLM registry: %w", fatalLLMErr)
	}

	orch := orchestrator.New(orchestrator.Config{
		Candidates:      llmCands,
		Registry:        &cfg.LLM,
		Recovery:        cfg.LLM.ResolvedRecovery(),
		AttemptTimeout:  cfg.LLM.ResolvedFailoverAttemptTimeout(),
		Retries:         cfg.LLM.ResolvedFailoverRetries(),
		RetryBackoff:    cfg.LLM.ResolvedFailoverRetryBackoff(),
		RetryBackoffCap: 30 * time.Second,
	}, rootLogger)

	orch.Boot(ctx)

	// The background recovery worker runs until the daemon context is
	// cancelled; it is joined by the deferred shutdown (orch.Wait).
	sigCtx := ctx
	orch.Start(sigCtx)
	defer orch.Wait()

	llmSnap := orch.Snapshot()
	if llmSnap.State == orchestrator.StateUnavailable {
		logger.Warn().
			Strs("candidates", candidateRefs(llmCands)).
			Msg("no LLM candidate ready at boot: daemon starts unavailable; recovery probing begins with agent traffic — check llm provider endpoints/credentials")
	} else {
		logger.Info().
			Str("active_model", llmSnap.ActiveRef).
			Str("api", llmSnap.ActiveAPI).
			Msg("LLM model resolved")

		if llmSnap.ActiveRef != cfg.LLM.Default {
			logger.Warn().
				Str("default_model", cfg.LLM.Default).
				Str("active_model", llmSnap.ActiveRef).
				Msg("LLM failover activated a different model than the default; session history is retained but may reference a different provider/protocol")
		}
	}

	agentReg := &agentRegistry{exec: orch, orch: orch, layout: orch.ActiveLayout()}

	// auxChatModel is handed to the auxiliary consumers (formatter post-step,
	// compaction summariser) — none needs tool calling, just plain Generate.
	// They are routed through the orchestrator (tasks 6.3/6.4): it walks the
	// candidate chain with the shared health policy and the proportional-share
	// attempt budget, and each consumer degrades on a failed or exhausted call
	// (deliver unformatted / leave the Session unchanged).
	auxChatModel := orch.AuxModel()

	outputFormatter := formatter.New(auxChatModel, skillsMgr, skillCache)
	fmtRegistry := buildFormatterRegistry(cfg)

	bootstrapCfg := buildBootstrapCfg(cfg, activeProfile.InjectSkills)
	bootstrapper := bootstrap.New(skillsMgr, skillCache, bootstrapCfg, rootLogger)

	// Register hot-reload callback: evict file cache and invalidate bootstrap
	// static shell so the next turn rebuilds from updated files.
	skillsMgr.OnReload(func(_ context.Context, skillName string) {
		if path, pathErr := skillsMgr.SkillFilePath(skillName); pathErr == nil {
			skillCache.Evict(path)
		}

		bootstrapper.Invalidate(skillName)
	})

	// Start the snapshot generator for the home-topology skill. It waits for
	// the twin ready internally and regenerates on every topology-change signal.
	if haTwin != nil {
		if skillDir, skillDirErr := skillsMgr.GetSkillLocation("home-topology"); skillDirErr == nil {
			snapshotter := twin.NewSnapshotter(haTwin, skillDir, bootstrapper, rootLogger)
			snapshotter.Start(ctx)
		} else {
			logger.Warn().Err(skillDirErr).Msg("home-topology skill not found — digital twin snapshots disabled")
		}
	}

	mgr := buildChannelManager(ctx, cfg, rootLogger)

	// llm-status-visibility: wire the orchestrator's model status into the
	// ZMQ channel — the authenticated handshake reports the active candidate,
	// and every promotion / transition to unavailable is broadcast to the
	// connected session as an unsolicited control event.
	if zmqCh := getZMQChannel(mgr, zmqcli.ChannelName); zmqCh != nil {
		zmqCh.WatchModelStatus(orchestratorStatusSource{orch: orch})
	}

	logger.Info().Msg("READY. (Agentic Home System) started. Type a message (Ctrl+C to quit).")

	// sigCtx was already bound to the caller-supplied ctx earlier (it is
	// bound to SIGINT/SIGTERM via signal.NotifyContext in main()).

	// tui-usage-observability + session-lifecycle D7: the daemon-global
	// per-Session usage registry, keyed by Session ID and re-seeded at
	// restore from each Session file's turn_usage records.
	usageStore := usage.NewStore()
	histStore := newHistoryStore(usageStore, rootLogger)

	sessStore, err := session.NewStore(cfg.Sessions.Path, rootLogger.With().Str("component", "sessions").Logger())
	if err != nil {
		return fmt.Errorf("failed to initialise the session store: %w", err)
	}

	logger.Info().Str("path", sessStore.Root()).Msg("session store ready")

	sessMgr := session.NewManager(sessStore, histStore, func(id string) persistence.UsageRecord {
		return recordFromTotals(usageStore.Snapshot(id))
	}, rootLogger.With().Str("component", "sessions").Logger())

	rt := &sessionRuntime{
		mgr:          mgr,
		agentReg:     agentReg,
		fmtr:         outputFormatter,
		reg:          fmtRegistry,
		bootstrapper: bootstrapper,
		bootstrapCfg: bootstrapCfg,
		skillsMgr:    skillsMgr,
		skillCache:   skillCache,
		agentTools:   agentTools,
		hist:         histStore,
		sessions:     sessMgr,
		usageStore:   usageStore,
		sanitizer:    sanitizer,
		logger:       rootLogger.With().Str("component", "dispatcher").Logger(),
	}
	configureSessionRuntime(cfg, rt, auxChatModel)

	// Session end: release the Session-scoped state, then a background
	// retention pass (also run once now, at boot).
	triggerRetention := sessStore.RunRetention(sigCtx, session.RetentionPolicy{
		Days:         cfg.Sessions.Retention.Days(),
		MaxTotalSize: int64(cfg.Sessions.Retention.MaxTotalSize),
	})
	sessMgr.OnEnd(func(ctx context.Context, ended *session.Session) {
		rt.onSessionEnd(ctx, ended)
		triggerRetention()
	})

	if zmqCh := getZMQChannel(mgr, zmqcli.ChannelName); zmqCh != nil {
		wireZMQSessions(sigCtx, zmqCh, rt, orch)
	}

	// Start health monitors for all connected MCP servers (managed internally
	// by CompositeRuntime). Monitors are stopped via mcpRuntime.Close() later.
	if cr, ok := mcpRuntime.(*homemcp.CompositeRuntime); ok {
		cr.StartMonitors(sigCtx)
		logger.Info().Msg("MCP health monitors started")
	}

	disp := newDispatcher(sigCtx, mgr, rt, rootLogger)
	go disp.run()

	<-sigCtx.Done()
	logger.Info().Msg("shutting down...")

	// Stop the channel manager first — this closes the incoming channel,
	// causing disp.run() to exit its range loop and close all worker queues.
	if err = mgr.Stop(); err != nil {
		logger.Error().Err(err).Msg("error during channel manager shutdown")
	}

	// Wait for all in-flight session workers to finish processing before
	// tearing down the resources they depend on (MCP, skills, etc.).
	// criticalities-remediation (M15): the wait is deadline-bounded — a
	// worker that ignores its context cannot extend daemon shutdown forever.
	waitDone := make(chan struct{})
	go func() {
		disp.wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
	case <-time.After(dispatcherWaitGrace):
		logger.Warn().Dur("grace", dispatcherWaitGrace).Msg("dispatcher drain deadline exceeded — proceeding with shutdown")
	}

	// Use a fresh context with a short deadline for graceful cleanup — the
	// original ctx is already cancelled by the signal handler.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err = bootstrapper.CloseAll(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("error closing bootstrapper sessions")
	}

	if haTwin != nil {
		if err = haTwin.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing HA digital twin")
		}
	}

	shutdownResources(mcpRuntime, skillCache, skillsMgr, rootLogger)

	// The final record precedes Close: records logged after it never reach
	// the log file (they still reach the console).
	logger.Info().Msg("goodbye")

	if err = logCloser.Close(); err != nil {
		logger.Error().Err(err).Msg("error closing log sink")
	}

	return nil
}

// usageTotalsToProtocol maps a usage snapshot to the wire's UsageInfo block
// (nil stays nil — a fresh session's payloads stay pre-usage shaped).
func usageTotalsToProtocol(t *usage.Totals) *protocol.UsageInfo {
	if t == nil {
		return nil
	}

	return &protocol.UsageInfo{
		ReadTokens:    t.ReadTokens,
		WrittenTokens: t.WrittenTokens,
		CachedTokens:  t.CachedTokens,
		TurnCount:     t.TurnCount,
		LLMCalls:      t.LLMCalls,
		AuxCalls:      t.AuxCalls,
	}
}

// configureSessionRuntime completes the worker runtime with the per-channel
// reset rules and the compaction wiring.
func configureSessionRuntime(cfg *config.Config, rt *sessionRuntime, aux model.BaseChatModel) {
	rt.resets = map[string]session.Rule{}
	if cfg.Channels.CLI.Enabled {
		rt.resets[cli.ChannelName] = session.RuleFrom(cfg.SessionResetFor("cli"))
	}

	if cfg.Channels.ZMQCLI.Enabled {
		rt.resets[zmqcli.ChannelName] = session.RuleFrom(cfg.SessionResetFor("zmqcli"))
	}

	if cfg.Channels.Telegram.Enabled {
		rt.resets[telegram.ChannelName] = session.RuleFrom(cfg.SessionResetFor("telegram"))
	}

	rt.compactCfg = cfg.Compaction
	rt.trigger = compaction.Trigger{
		Window:   smallestContextWindow(cfg.LLM),
		Reserve:  cfg.Compaction.ReserveTokens,
		MaxTurns: cfg.Compaction.MaxTurns,
	}

	rt.summariser = compaction.NewSummariser(aux, rt.compactionSkillPrompt, cfg.Compaction.Timeout,
		rt.logger.With().Str("consumer", "compaction").Logger())
}

// wireZMQSessions wires the ZMQ channel to the Session lifecycle: the
// Connection start hook evaluates the reset rule of the `cli` address at
// every successful handshake (never waiting for its worker), the usage seed
// and final replies read the current Session's totals, and the TUI-reserved
// /session and /context requests are answered from live state.
func wireZMQSessions(ctx context.Context, zmqCh *zmqcli.Channel, rt *sessionRuntime, orch *orchestrator.Orchestrator) {
	zmqCh.OnConnectionStart(func(hctx context.Context, msg channel.Message) channel.ConnectionInfo {
		id, pending := rt.sessions.RequestReset(hctx, session.AddressOf(msg), rt.resetRule(zmqcli.ChannelName), time.Now())

		return channel.ConnectionInfo{SessionID: id, ResetPending: pending}
	})

	zmqCh.SetUsageSource(func() *protocol.UsageInfo {
		totals := rt.usageStore.Snapshot(rt.sessions.Current(ctx, zmqcli.ChannelName).ID)

		info := usageTotalsToProtocol(totals)
		if info == nil {
			return nil
		}

		// Occupancy is computed daemon-side: the last main-agent
		// call's real prompt tokens ÷ the active candidate's configured
		// window. Absent without either input — never estimated.
		if totals.LastMainPromptTokens > 0 {
			if window := orch.Snapshot().ActiveContextWindow; window > 0 {
				info.ContextOccupancyPct = 100 * float64(totals.LastMainPromptTokens) / float64(window)
			}
		}

		return info
	})

	zmqCh.SetControlRequestHandler(buildControlRequestHandler(ctx, rt))
}

// smallestContextWindow returns the smallest context_window declared by a
// provider referenced in the candidate chain (default + failover), or 0 when
// none declares one. A prompt that fits it fits every candidate a failover
// may select; Ollama silently truncates an oversized prompt from the head.
func smallestContextWindow(cfg config.LLMConfig) int {
	smallest := 0
	for _, ref := range append([]string{cfg.Default}, cfg.Failover...) {
		providerName, _, err := config.ParseModelReference(ref)
		if err != nil {
			continue
		}

		if cw := cfg.Providers[providerName].ContextWindow; cw > 0 && (smallest == 0 || cw < smallest) {
			smallest = cw
		}
	}

	return smallest
}

// buildLogger constructs the root zerolog.Logger from the application config
// and logs the collected boot warnings (config, env override, legacy file).
func buildLogger(cfg *config.Config) (logging.Result, error) {
	// Invalid format or file-rotation values are fatal; unrecognised levels
	// and a too-small total cap are boot warnings.
	configWarnings, err := cfg.ValidateLogging()
	if err != nil {
		return logging.Result{}, err
	}

	res, err := logging.New(cfg.Logging)
	if err != nil {
		return logging.Result{}, fmt.Errorf("failed to build logger: %w", err)
	}

	for _, w := range append(configWarnings, res.Warnings...) {
		res.Logger.Warn().Msg(w)
	}

	return res, nil
}

// mustInitInfra creates the skills manager and the skill cache. It
// terminates the process on failure.
func mustInitInfra(ctx context.Context, cfg *config.Config, rootLogger zerolog.Logger) (
	*skills.Manager,
	*skillcache.SkillCache,
) {
	logger := rootLogger.With().Str("component", "infra").Logger()

	skillsMgr, err := skills.NewManager(ctx, &cfg.Skills, rootLogger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create skills manager")
	}

	sc, err := skillcache.New()
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to create skill cache")
	}
	sc.WithLogger(logger.With().Str("component", "skillcache").Logger())

	return skillsMgr, sc
}

// buildLLMCandidates compiles the immutable ordered candidate chain and
// builds one production runner per candidate (task 4.7), covering both
// agent-construction paths: *agent.Agent for every chat protocol
// (chat_completions, anthropic, google, ollama, openrouter) and
// *agent.AgenticAgent for `responses`. A fatal error is a GLOBAL registry
// error (llm.CompileCandidates: reference shape, undeclared providers,
// duplicates) — the daemon refuses to boot. Per-candidate failures (build,
// credential, agent construction) mark only that candidate unhealthy with a
// WARN and never fail boot; the readiness evaluation in orchestrator.Boot
// handles the rest.
func buildLLMCandidates(
	ctx context.Context,
	cfg *config.Config,
	agentTools []tool.BaseTool,
	rootLogger zerolog.Logger,
	resolver *toolnames.Resolver,
	activeProfileName string,
	discloseDenied bool,
) ([]orchestrator.Candidate, error) {
	logger := rootLogger.With().Str("component", "llm-registry").Logger()

	warnOllamaContextWindowMismatch(logger, cfg)

	descs, err := llm.CompileCandidates(&cfg.LLM)
	if err != nil {
		return nil, err // fatal global registry error
	}

	// One shared thought tap: only one candidate serves a turn at a time, so
	// a single tap never interleaves turns.
	tap := agent.NewTap(rootLogger, cfg.Logging.Thoughts)

	cands := make([]orchestrator.Candidate, 0, len(descs))
	for _, desc := range descs {
		cand := orchestrator.Candidate{CandidateDescriptor: desc}

		resolved, buildErr := llm.BuildForReference(ctx, &cfg.LLM, desc.Ref)
		if buildErr != nil {
			cand.ConstructErr = buildErr
			cands = append(cands, cand)

			continue // per-candidate failure: WARN at boot, candidate unhealthy
		}

		cand.Model = resolved

		if resolved.IsAgentic() {
			// tui-usage-observability: the recorder sits at the model layer,
			// below the ADK, so usage extraction is uniform for chat and
			// agentic kinds. Attribution rides the call context;
			// the same wrapped instance serves both the runner and the aux
			// consumers so formatter/classifier calls are counted too.
			recordedAgentic := usage.NewRecordedAgenticModel(resolved.Agentic)

			// denied-tool-disclosure: the agentic path gets the same
			// model-layer blocked-call rewrite as the chat path so a blocked
			// call reaches the sentinel identically on both construction
			// paths (the design doc; agentic wrapper is Generate/Stream only).
			var runnerModel model.AgenticModel = recordedAgentic
			if discloseDenied {
				runnerModel = toolnames.NewDisclosingAgenticModel(recordedAgentic, resolver, rootLogger, activeProfileName)
			}

			runner, agErr := agent.NewAgentic(ctx, agent.AgenticConfig{
				Model:         runnerModel,
				Tools:         agentTools,
				SystemPrompt:  agent.BuildSystemPrompt(),
				MaxIterations: 25,
			}, tap)
			if agErr != nil {
				cand.ConstructErr = fmt.Errorf("failed to build agentic runner: %w", agErr)
				cands = append(cands, cand)

				continue
			}

			cand.Runner = runner
			cand.Aux = agent.AsBaseChatModel(recordedAgentic)
		} else {
			// Defensive layer: rewrite hallucinated tool-call names before they
			// hit Eino's toolsNode. See internal/agent/toolnames for the
			// resolution rules. Not wired for the agentic (`responses`) path.
			// The usage recorder sits below the normaliser so both the ReAct
			// runner and the raw aux model attribute through the same seam.
			recorded := usage.NewRecordedChatModel(resolved.Chat)
			var chatModel *toolnames.NormalizingChatModel
			if discloseDenied {
				chatModel = toolnames.NewDisclosingChatModel(recorded, resolver, rootLogger, activeProfileName)
			} else {
				chatModel = toolnames.NewNormalizingChatModel(recorded, resolver, rootLogger)
			}

			runner, chatErr := agent.New(ctx, agent.Config{
				ChatModel:     chatModel,
				Tools:         agentTools,
				SystemPrompt:  agent.BuildSystemPrompt(),
				MaxIterations: 25,
			}, tap)
			if chatErr != nil {
				cand.ConstructErr = fmt.Errorf("failed to build chat runner: %w", chatErr)
				cands = append(cands, cand)

				continue
			}

			cand.Runner = runner
			// The auxiliary consumers never attach tools, so they take the raw
			// recorded model (the tool-name normalizer is a tool-calling
			// defence); its usage recording matches the runner's.
			cand.Aux = recorded
		}

		cands = append(cands, cand)
	}

	// Per-candidate construction failures are logged here (task 4.7): they
	// mark only the affected candidate unhealthy and never fail boot.
	for _, cand := range cands {
		if cand.ConstructErr != nil {
			logger.Warn().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Err(cand.ConstructErr).
				Msg("LLM candidate construction failed; candidate unhealthy at boot")
		}
	}

	return cands, nil
}

// candidateRefs renders the candidate chain's references for logs.
func candidateRefs(cands []orchestrator.Candidate) []string {
	refs := make([]string, len(cands))
	for i, c := range cands {
		refs[i] = c.Ref
	}

	return refs
}

// warnOllamaContextWindowMismatch logs one WARN per ollama provider instance
// declaring both options.num_ctx and context_window with different values
// (tui-usage-observability task 4.3): Ollama would silently truncate the
// prompt to num_ctx, invalidating the occupancy percentage computed from
// context_window. Doc-only mitigation — the daemon still boots.

// buildTools assembles the agent tool list (skill tools + builtin tools +
// optional MCP tools), every invokable one wrapped by the error-digestion
// layer. It returns the MCP runtime (so the caller can close it on shutdown).
// Health monitors are managed internally by CompositeRuntime; use
// StartMonitors on the returned runtime to activate them.
func buildTools(
	ctx context.Context,
	cfg *config.Config,
	skillsMgr *skills.Manager,
	skillCache *skillcache.SkillCache,
	useMockMCP bool,
	activeProfileName string,
	haTwin *hatwin.Twin,
	rootLogger zerolog.Logger,
) ([]tool.BaseTool, homemcp.Runtime, []profile.BlockedTool) {
	logger := rootLogger.With().Str("component", "tools").Logger()
	decisionLog := cfg.Logging.ProfileDecisions

	builtinTools := []tool.BaseTool{
		skills.NewSkillsLoadTool(skillsMgr),
		skills.NewSkillsUnloadTool(skillsMgr),
		skills.NewReadSkillAssetTool(skillsMgr, skillCache),
		skills.NewListSkillAssetsTool(skillsMgr),
		tools.NewWaitTool(),
	}

	var blocked []profile.BlockedTool

	agentTools, ownerBlocked := profile.GateOwner(ctx, "builtin_tools", builtinTools, cfg.BuiltinTools.Profiles, activeProfileName, decisionLog, logger)
	blocked = append(blocked, ownerBlocked...)

	twinTools, twinBlocked := twin.BuildTools(ctx, cfg.HomeAssistant, haTwin, activeProfileName, decisionLog, logger)
	agentTools = append(agentTools, twinTools...)
	blocked = append(blocked, twinBlocked...)

	runtime, mcpBlocked, err := connectAllMCP(ctx, cfg, useMockMCP, activeProfileName, decisionLog, rootLogger)
	if err != nil {
		logger.Warn().Err(err).Msg("MCP connection failed — device control will be unavailable")

		return wrapWithErrorDigest(agentTools, rootLogger), nil, blocked
	}
	blocked = append(blocked, mcpBlocked...)

	if runtime != nil {
		mcpTools := runtime.Tools()
		agentTools = append(agentTools, mcpTools...)
		logger.Info().
			Str("mcp_profile", activeProfileName).
			Int("count", len(mcpTools)).
			Msg("MCP tools loaded (gated per-server against each server's own profiles block)")
	}

	return wrapWithErrorDigest(agentTools, rootLogger), runtime, blocked
}

// wrapWithErrorDigest wraps all tools with the error-digestion layer
// (errdigest.TrackedTool). The classifier is wired here with the MCP and
// wait-tool sentinels.
func wrapWithErrorDigest(agentTools []tool.BaseTool, rootLogger zerolog.Logger) []tool.BaseTool {
	classifier := errdigest.NewClassifier().
		Register(tools.ErrWaitExceedsTurnBudget, errdigest.Transient).
		Register(homemcp.ErrMCPTimeout, errdigest.Transient).
		Register(homemcp.ErrMCPConnection, errdigest.Transient).
		Register(homemcp.ErrMCPAuth, errdigest.Permanent).
		Register(homemcp.ErrMCPOAuthLoginRequired, errdigest.Permanent).
		Register(homemcp.ErrMCPToolNotFound, errdigest.Permanent).
		Register(homemcp.ErrMCPToolExecution, errdigest.Unknown).
		// Twin write path (twin-native-actuation D7): barrier refusals are
		// transient; vocabulary/target/parameter refusals are permanent.
		Register(hatwin.ErrNotConnected, errdigest.Transient).
		Register(hatwin.ErrSessionNotReady, errdigest.Transient).
		Register(hatwin.ErrStale, errdigest.Transient).
		Register(hatwin.ErrSessionClosed, errdigest.Transient).
		// ha_state_query's hard-stale refusal wraps twin.ErrTwinStale (a
		// separate sentinel from pkg/hatwin's ErrStale); without this the
		// observation reaches the model as [UNKNOWN] instead of [TRANSIENT].
		Register(twin.ErrTwinStale, errdigest.Transient).
		Register(hatwin.ErrOutOfVocabulary, errdigest.Permanent).
		Register(hatwin.ErrInvalidParams, errdigest.Permanent).
		Register(hatwin.ErrDomainRequired, errdigest.Permanent).
		Register(hatwin.ErrNoMatch, errdigest.Permanent).
		Register(hatwin.ErrUnknownEntity, errdigest.Permanent).
		Register(hatwin.ErrOverCap, errdigest.Permanent).
		Register(hatwin.ErrMultiTargetToggle, errdigest.Permanent).
		Register(hatwin.ErrUnknownCallID, errdigest.Permanent)

	return errdigest.WrapTools(agentTools, classifier, toolSessionKey,
		rootLogger.With().Str("component", "errdigest").Logger())
}

// buildToolNameResolver enumerates every tool's Info() name and returns a
// Resolver used by the NormalizingChatModel / NormalizingAgenticModel to
// correct hallucinated tool names before they reach Eino's toolsNode. Tools
// whose Info call fails contribute nothing (they will be surfaced by Eino at
// binding time). When disclose is true the blocked set is loaded into the
// resolver's ghost index (disclose_denied_tools knob on); otherwise it is
// dropped and no blocked name is ever resolvable.
// ghostTools converts the profile layer's blocked set into the toolnames
// ghost-index shape. Shared by buildToolNameResolver's ghost index (call
// interception: the rewritten policy_block call + WARN).
func ghostTools(blocked []profile.BlockedTool) []toolnames.BlockedTool {
	ghosts := make([]toolnames.BlockedTool, 0, len(blocked))
	for _, b := range blocked {
		ghosts = append(ghosts, toolnames.BlockedTool{Name: b.Name, Owner: b.Owner})
	}

	return ghosts
}

func buildToolNameResolver(ctx context.Context, agentTools []tool.BaseTool, rootLogger zerolog.Logger, blocked []profile.BlockedTool, disclose bool) *toolnames.Resolver {
	logger := rootLogger.With().Str("component", "toolnames").Logger()

	names := make([]string, 0, len(agentTools))
	for _, t := range agentTools {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			logger.Warn().Err(err).Msg("tool Info() failed — skipping in name resolver index")

			continue
		}
		names = append(names, info.Name)
	}

	logger.Debug().Int("count", len(names)).Strs("tools", names).Msg("tool-name resolver built")

	resolver := toolnames.NewResolver(names)
	if disclose {
		ghosts := ghostTools(blocked)
		ghostNames := make([]string, 0, len(ghosts))
		for _, g := range ghosts {
			ghostNames = append(ghostNames, g.Name)
		}
		resolver.LoadBlocked(ghosts)
		logger.Info().
			Int("blocked", len(ghosts)).
			Strs("ghosts", ghostNames).
			Msg("blocked-tool ghost index loaded (disclose_denied_tools on)")
	}

	return resolver
}

// toolSessionKey extracts the session identifier stored in context by
// processMessage, logged by the error-digestion layer.
func toolSessionKey(ctx context.Context) string {
	if key, ok := ctx.Value(toolSessionKeyCtxKey{}).(string); ok {
		return key
	}

	return "unknown"
}

// toolSessionKeyCtxKey is the context key used to carry the session key into tools.
type toolSessionKeyCtxKey struct{}

// connectAllMCP connects to every server declared in cfg.MCPServers and
// aggregates them under a single CompositeRuntime. Failure of an individual
// server is logged as Warn and skipped (best-effort boot). When --mock-mcp is
// active, all configured servers are ignored and a single in-process mock is
// returned instead.
func connectAllMCP(
	ctx context.Context,
	cfg *config.Config,
	useMock bool,
	activeProfileName string,
	decisionLog bool,
	rootLogger zerolog.Logger,
) (homemcp.Runtime, []profile.BlockedTool, error) {
	logger := rootLogger.With().Str("component", "mcp").Logger()

	if useMock {
		logger.Info().Msg("using mock MCP server")

		mockServer := homemcp.NewMockServer(nil)
		client, err := mockServer.NewClient(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create mock MCP client: %w", err)
		}

		// The mock simulates the "home_assistant" server: gate it against
		// mcp_servers.home_assistant.profiles like any real server, so
		// --mock-mcp exercises the same owner-gating path.
		gatedTools, mockBlocked := profile.GateOwner(ctx, "home_assistant", client.Tools(), cfg.MCPServers["home_assistant"].Profiles, activeProfileName, decisionLog, logger)

		// Wrap the single mock in a CompositeRuntime with no prefix.
		entry := homemcp.CompositeEntry{
			Runtime: client,
			Tools:   gatedTools,
		}

		return homemcp.NewCompositeRuntime(ctx, []homemcp.CompositeEntry{entry}, logger), mockBlocked, nil
	}

	if len(cfg.MCPServers) == 0 {
		return nil, nil, nil
	}

	var blocked []profile.BlockedTool

	var oauthStore oauth.Store

	if hasOAuthServer(cfg.MCPServers) {
		var err error

		oauthStore, err = oauth.NewStore(ctx, cfg.OAuth)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to initialise oauth credential store: %w", err)
		}
	}

	var entries []homemcp.CompositeEntry

	for serverKey, serverCfg := range cfg.MCPServers {
		if serverCfg.URL == "" {
			logger.Warn().Str("server", serverKey).Msg("MCP server entry has no URL — skipping")

			continue
		}

		effectivePrefix := serverCfg.EffectiveToolPrefix(serverKey)

		logger.Info().
			Str("server", serverKey).
			Str("url", serverCfg.URL).
			Str("transport", serverCfg.Transport).
			Str("tool_prefix", effectivePrefix).
			Msg("connecting to MCP server")

		clientCfg := homemcp.ClientConfig{
			MaxRetries:    3,
			RetryBackoff:  time.Second,
			Timeout:       serverCfg.Timeout,
			ExpectedTools: serverCfg.ExpectedTools,
			Logger:        rootLogger,
		}
		reconnectCfg := homemcp.ReconnectConfigFromConfig(serverCfg.Reconnect)

		serverCfgCopy := serverCfg // capture loop var for pointer stability

		if serverCfgCopy.OAuth != nil {
			// One scoped *http.Client per server, shared by the transport and
			// the OAuth handler — one policy, one construction site.
			httpClient := homemcp.NewScopedHTTPClient(&serverCfgCopy)

			handler, err := buildDaemonOAuthHandler(ctx, oauthStore, serverKey, serverCfgCopy, httpClient)
			if err != nil {
				logger.Warn().
					Str("server", serverKey).
					Err(err).
					Msg("failed to build oauth handler — skipping (best-effort boot)")

				continue
			}

			clientCfg.HTTPClient = httpClient
			clientCfg.OAuthHandler = handler
		}

		rc, err := homemcp.NewReconnectingClient(ctx, &serverCfgCopy, clientCfg, reconnectCfg, serverKey, effectivePrefix)
		if err != nil {
			if errors.Is(err, homemcp.ErrMCPOAuthLoginRequired) {
				logger.Warn().
					Str("server", serverKey).
					Msg("no usable oauth credential for this server — run `ready login " + serverKey + "`; skipping (best-effort boot)")
			} else {
				logger.Warn().
					Str("server", serverKey).
					Err(err).
					Msg("MCP server connection failed — skipping (best-effort boot)")
			}

			continue
		}

		var monitor *homemcp.HealthMonitor
		if serverCfg.HealthCheck.Enabled {
			monitor = homemcp.NewHealthMonitorFromConfig(rc, rc, serverCfg.HealthCheck)
			if monitor != nil {
				monitor.WithLogger(rootLogger)
			}
		}

		if rules, ok := serverCfg.Profiles[activeProfileName]; ok {
			serverKeyCopy, rulesCopy := serverKey, rules
			rc.OnReconnect(func() {
				profile.ValidateOwnerPatterns(ctx, serverKeyCopy, rc.Tools(), rulesCopy, zerolog.DebugLevel, logger)
			})
		}

		gatedTools, ownerBlocked := profile.GateOwner(ctx, serverKey, rc.Tools(), serverCfg.Profiles, activeProfileName, decisionLog, logger)
		blocked = append(blocked, ownerBlocked...)

		entries = append(entries, homemcp.CompositeEntry{
			Runtime: rc,
			Monitor: monitor,
			Tools:   gatedTools,
		})
	}

	if len(entries) == 0 {
		return nil, nil, nil
	}

	return homemcp.NewCompositeRuntime(ctx, entries, logger), blocked, nil
}

// hasOAuthServer reports whether any server in servers declares an oauth
// block, so connectAllMCP only touches the credential store (and its
// directory) when at least one server actually needs it.
func hasOAuthServer(servers map[string]config.MCPServerConfig) bool {
	for _, srv := range servers {
		if srv.OAuth != nil {
			return true
		}
	}

	return false
}

// buildDaemonOAuthHandler builds the auth.OAuthHandler for a single OAuth
// server per the design section's table (task 8.2). Unlike `ready login`, the daemon
// performs no discovery and no client registration: it only
// reads the store and, for authorization_code, mounts loginRequiredHandler
// automatically (via oauth.BuildHandler) when no usable registration is on
// disk yet. Servers using client_credentials are never gated on the store —
// they authenticate autonomously on first use.
func buildDaemonOAuthHandler(
	ctx context.Context,
	store oauth.Store,
	serverKey string,
	srv config.MCPServerConfig,
	httpClient *http.Client,
) (auth.OAuthHandler, error) {
	cred, err := store.Load(ctx, serverKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load oauth credential for %q: %w", serverKey, err)
	}

	grant, err := resolveDaemonGrant(srv.OAuth, cred != nil && cred.ClientID != "")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve oauth grant for %q: %w", serverKey, err)
	}

	handlerCfg := oauth.HandlerConfig{
		ServerKey:  serverKey,
		Grant:      grant,
		HTTPClient: httpClient,
	}

	switch grant {
	case oauth.GrantClientCredentials:
		secret, serr := srv.OAuth.ResolveClientSecret()
		if serr != nil {
			return nil, fmt.Errorf("failed to resolve oauth client secret for %q: %w", serverKey, serr)
		}

		handlerCfg.ClientID = srv.OAuth.ClientID
		handlerCfg.ClientSecret = secret

	case oauth.GrantAuthorizationCode:
		if cred != nil && cred.ClientID != "" {
			registeredClient := &oauthex.ClientCredentials{ClientID: cred.ClientID}
			if cred.ClientSecret != "" {
				registeredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: cred.ClientSecret}
			}

			handlerCfg.RegisteredClient = registeredClient
			handlerCfg.RedirectURL = cred.RedirectURI
			handlerCfg.Fetcher = oauth.NonInteractiveFetcher
			handlerCfg.InitialTokenSource = oauth.NewTokenSourceFromCredential(store, httpClient, cred)
		}
		// No stored credential: oauth.BuildHandler mounts loginRequiredHandler.
	}

	return oauth.BuildHandler(handlerCfg)
}

// resolveDaemonGrant resolves srv.Grant to one of oauth.GrantAuthorizationCode
// or oauth.GrantClientCredentials without any network I/O: under
// "auto", a stored registration (hasStoredCredential) always means
// authorization_code, since a client_credentials server holds no stored
// Credential by design (the design doc context); absent one, a configured
// client_id and secret resolve to client_credentials, mirroring the design doc's
// rule without the authorization-server round trip — a wrong guess simply
// fails Authorize() and is skipped by the best-effort boot loop like any
// other connection failure.
func resolveDaemonGrant(oa *config.MCPOAuthConfig, hasStoredCredential bool) (string, error) {
	grant := oa.Grant
	if grant == "" || grant == grantAuto {
		if hasStoredCredential {
			return oauth.GrantAuthorizationCode, nil
		}

		secret, err := oa.ResolveClientSecret()
		if err != nil {
			return "", err
		}

		if oa.ClientID != "" && secret != "" {
			return oauth.GrantClientCredentials, nil
		}

		return oauth.GrantAuthorizationCode, nil
	}

	return grant, nil
}

// buildFormatterRegistry walks the channels configuration and returns the
// formatter skill name for every enabled channel.  Disabled channels are
// omitted.  Both cli and zmqcli share the logical channel name "cli".
func buildFormatterRegistry(cfg *config.Config) formatterRegistry {
	reg := formatterRegistry{}

	if cfg.Channels.CLI.Enabled {
		reg[cli.ChannelName] = channelFormatterCfg{
			skillName: cfg.Channels.CLI.FormatterSkill,
			postStep:  cfg.Channels.CLI.FormatterPostStep,
		}
	}

	if cfg.Channels.ZMQCLI.Enabled {
		reg[zmqcli.ChannelName] = channelFormatterCfg{
			skillName: cfg.Channels.ZMQCLI.FormatterSkill,
			postStep:  cfg.Channels.ZMQCLI.FormatterPostStep,
		}
	}

	if cfg.Channels.Telegram.Enabled {
		reg[telegram.ChannelName] = channelFormatterCfg{
			skillName: cfg.Channels.Telegram.FormatterSkill,
			postStep:  cfg.Channels.Telegram.FormatterPostStep,
		}
	}

	return reg
}

// homeIntegrationSkill is the bootstrap-injected skill carrying the twin
// write/read discipline (twin-native-actuation D11). Injected into every
// enabled channel when home_assistant.enabled.
const homeIntegrationSkill = "home-integration"

// buildBootstrapCfg extracts the per-channel bootstrap skill lists from config,
// prepending the active profile's inject_skills to every enabled channel so the
// profile's skills (e.g. home-provisioning under `edit`) are injected into the
// system prompt regardless of per-channel bootstrap configuration. Profile
// skills come first and duplicates are removed (a profile skill that also
// appears in a channel's bootstrap is loaded once).
//
// When home_assistant.enabled, the home-integration skill (twin-native-actuation
// D11) is merged into the injection for every enabled channel — marked Active
// and absent from the catalog, like any other injected skill.
func buildBootstrapCfg(cfg *config.Config, injectSkills []string) map[string][]string {
	if cfg.HomeAssistant.Enabled {
		injectSkills = mergeSkills([]string{homeIntegrationSkill}, injectSkills)
	}

	m := make(map[string][]string)

	add := func(name string, channelBootstrap []string) {
		combined := mergeSkills(injectSkills, channelBootstrap)
		if len(combined) > 0 {
			m[name] = combined
		}
	}

	if cfg.Channels.CLI.Enabled {
		add(cli.ChannelName, cfg.Channels.CLI.Bootstrap)
	}

	if cfg.Channels.ZMQCLI.Enabled {
		add(zmqcli.ChannelName, cfg.Channels.ZMQCLI.Bootstrap)
	}

	if cfg.Channels.Telegram.Enabled {
		add(telegram.ChannelName, cfg.Channels.Telegram.Bootstrap)
	}

	return m
}

// mergeSkills concatenates profile-injected skills (first) with a channel's
// bootstrap list, preserving order and dropping duplicates.
func mergeSkills(profileSkills, channelSkills []string) []string {
	seen := make(map[string]struct{}, len(profileSkills)+len(channelSkills))
	out := make([]string, 0, len(profileSkills)+len(channelSkills))

	for _, s := range profileSkills {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}

	for _, s := range channelSkills {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}

	return out
}

// buildChannelManager creates and starts the channel manager with all enabled channels.
//
// cli and zmqcli are mutually exclusive: both use the logical channel name
// "cli", and config.Validate ensures at most one is enabled at a time.
// When zmqcli is enabled the legacy stdin cli is NOT registered.
func buildChannelManager(ctx context.Context, cfg *config.Config, rootLogger zerolog.Logger) *channel.Manager {
	logger := rootLogger.With().Str("component", "channel-manager").Logger()

	var channels []channel.Channel

	if cfg.Channels.CLI.Enabled {
		channels = append(channels, cli.New())
	}

	if cfg.Channels.ZMQCLI.Enabled {
		secret, err := cfg.Channels.ZMQCLI.ResolveAuthToken()
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to resolve zmqcli auth token")
		}

		zmqCfg := cfg.Channels.ZMQCLI

		// Guard against the reply_timeout watchdog firing before the agent's
		// own run budget is exhausted (it must never be shorter than the sum
		// of everything the daemon may spend producing a reply, plus a
		// buffer) — otherwise a legitimately long-running request gets its
		// session killed mid-flight and the eventual reply is dropped.
		minReplyTimeout := agentRunTimeout + formatterTimeout + 30*time.Second
		if zmqCfg.ReplyTimeout < minReplyTimeout {
			logger.Warn().
				Dur("configured_reply_timeout", zmqCfg.ReplyTimeout).
				Dur("min_reply_timeout", minReplyTimeout).
				Msg("channels.zmqcli.reply_timeout is too low for the agent run budget — raising it")
			zmqCfg.ReplyTimeout = minReplyTimeout
		}

		zch, err := zmqcli.NewChannel(ctx, zmqCfg, secret, cfg.Logging.ZMQPayloadsEnabled, logger)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to create zmqcli channel")
		}

		channels = append(channels, zch)

		logger.Info().
			Str("bind", cfg.Channels.ZMQCLI.Bind).
			Dur("connection_idle_timeout", cfg.Channels.ZMQCLI.ConnectionIdleTimeout).
			Msg("zmqcli channel enabled")
	}

	if cfg.Channels.Telegram.Enabled {
		tch, err := telegram.New(cfg.Channels.Telegram, rootLogger)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to create Telegram channel")
		}

		channels = append(channels, tch)

		logger.Info().
			Int("whitelist_entries", len(cfg.Channels.Telegram.AllowFrom)).
			Msg("Telegram channel enabled")
	}

	mgr := channel.NewManager(rootLogger, channels...)

	if err := mgr.Start(); err != nil {
		logger.Fatal().Err(err).Msg("failed to start channel manager")
	}

	return mgr
}

// agentRunTimeout caps the time a single agent invocation may take.  If the
// LLM is slow or unreachable the message loop must not block forever — after
// this deadline the context is cancelled and the user receives a terminal
// failure message (terminalMessageFor). It is a var (not a const) solely so
// tests can shorten it to exercise the agent-run-deadline failure path
// without waiting five minutes.
var agentRunTimeout = 300 * time.Second //nolint:gochecknoglobals // var (not const) solely so tests can shorten the agent-run deadline for the terminal-delivery deadline test

// Terminal failure messages (llm-outage-delivery). Fixed English product
// strings — deliberately NOT configurable, because a configurable terminal
// message can be configured to the empty string, which would reintroduce the
// silent-failure defect this change exists to fix (design A6). Both stay
// short enough to wrap within three lines at 40 columns so the serial/PETSCII
// channel needs no separate short form.
const (
	// msgLLMUnavailable is delivered when nothing ran: no tool call completed
	// and no content was delivered. Repeating the request later is safe.
	msgLLMUnavailable = "The AI service is temporarily unavailable. Please try again in a few minutes."
	// msgIncompleteResponse is delivered when the turn may have changed the
	// world (a tool call completed) or already showed output (content was
	// delivered). The user must check state before repeating the request —
	// repeating a partially-executed home-automation action runs it twice.
	msgIncompleteResponse = "The response could not be completed. Some actions may already have run; check the current state before repeating the request."
)

// terminalMessageFor selects the terminal failure message for a failed turn
// from the outcome metadata (design A2). The discriminator is observable
// side effects, not the error class: a user who may have had a light switched
// on must not be told "try again in a few minutes", because repeating the
// request could run the action twice.
func terminalMessageFor(outcome agent.TurnOutcome) string {
	if outcome.ToolCallStarted || outcome.ToolCallCompleted || outcome.ContentDelivered {
		return msgIncompleteResponse
	}

	return msgLLMUnavailable
}

// formatterTimeout caps the post-step formatter LLM call (formatter_post_step: true).
// It is kept shorter than agentRunTimeout so the total processing time
// (agent + formatter) stays within the client-side reply-timeout window.
const formatterTimeout = 60 * time.Second

// sessionWorkerBuffer is the queue depth per session worker.  If a single
// session sends more messages than this while its agent run is in flight, the
// dispatcher applies back-pressure on Manager.Incoming.
const sessionWorkerBuffer = 16

// dispatcherWaitGrace bounds the dispatcher drain at shutdown
// (criticalities-remediation M15).
const dispatcherWaitGrace = 10 * time.Second

// deliveryGrace bounds the post-turn delivery path (criticalities-remediation
// M15): the delivery runs on a WithoutCancel context, so an explicit deadline
// replaces the dispatcher's cancellation. Generous enough for a final Telegram
// edit or ZMQ send over a slow network.
const deliveryGrace = 30 * time.Second

// dispatcher routes incoming messages to a per-address worker goroutine so
// different Conversation addresses (CLI, distinct Telegram chats) run in
// parallel while preserving FIFO ordering within each: one item — a message
// or a Session command — at a time, so every Session transition of an
// address is executed by its worker only (session-lifecycle D5/D6).
//
// The per-item handler is injected as processFn so the dispatcher can be
// unit-tested without standing up the agent, formatter, MCP and persistence
// stack. Production wiring uses sessionRuntime.handleItem.
type dispatcher struct {
	ctx       context.Context
	incoming  <-chan channel.Message
	processFn func(ctx context.Context, msg channel.Message)
	keyFn     func(msg channel.Message) string
	logger    zerolog.Logger

	mu      sync.Mutex
	workers map[string]chan channel.Message
	wg      sync.WaitGroup
	// dropped tracks per-address message drops when an address's inbox is
	// saturated (criticalities-remediation M2). Guarded by d.mu.
	dropped map[string]int
}

func newDispatcher(ctx context.Context, mgr *channel.Manager, rt *sessionRuntime, rootLogger zerolog.Logger) *dispatcher {
	return &dispatcher{
		ctx:       ctx,
		logger:    rootLogger.With().Str("component", "dispatcher").Logger(),
		incoming:  mgr.Incoming(),
		processFn: rt.handleItem,
		keyFn:     func(m channel.Message) string { return string(session.AddressOf(m)) },
		workers:   make(map[string]chan channel.Message),
		dropped:   make(map[string]int),
	}
}

// run reads from the incoming channel and enqueues each message onto the
// worker for its session key.  On incoming close it flushes every worker
// queue and waits for them to drain.
func (d *dispatcher) run() {
	for msg := range d.incoming {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}

		d.enqueue(msg)
	}

	d.mu.Lock()
	for _, in := range d.workers {
		close(in)
	}
	d.workers = nil
	d.mu.Unlock()

	d.wg.Wait()
}

// wait blocks until all session workers have finished processing. This should
// be called after the channel manager has been stopped (which closes the
// incoming channel and causes run() to drain).
func (d *dispatcher) wait() {
	d.wg.Wait()
}

// enqueue pushes msg onto the worker for sessionKey(msg), creating the worker
// on first sight.  If the dispatcher context is done it drops the message.
func (d *dispatcher) enqueue(msg channel.Message) {
	key := d.keyFn(msg)

	d.mu.Lock()
	in, ok := d.workers[key]
	if !ok {
		in = make(chan channel.Message, sessionWorkerBuffer)
		d.workers[key] = in
		d.wg.Add(1)

		go d.serve(key, in)
	}
	d.mu.Unlock()

	// The enqueue is NON-BLOCKING — one saturated session's inbox must not block enqueue,
	// which would block m.incoming and wedge the intake of every other
	// channel (head-of-line blocking). The message is dropped with a
	// structured WARN naming the session; the drop counter keeps the
	// saturation observable.
	select {
	case in <- msg:
	case <-d.ctx.Done():
	default:
		d.mu.Lock()
		d.dropped[key]++
		count := d.dropped[key]
		d.mu.Unlock()

		d.logger.Warn().
			Str("session", key).
			Str("channel", msg.ChannelName).
			Int("inbox", sessionWorkerBuffer).
			Int("dropped_total", count).
			Msg("session inbox saturated — message dropped")
	}
}

// serve drains the worker's inbox sequentially, isolating each message with a
// panic recovery so one bad message cannot tear down the session worker.
func (d *dispatcher) serve(key string, in <-chan channel.Message) {
	defer d.wg.Done()

	for msg := range in {
		func() {
			defer func() {
				if r := recover(); r != nil {
					d.logger.Error().
						Str("session", key).
						Str("channel", msg.ChannelName).
						Str("stack", string(debug.Stack())).
						Msgf("PANIC in session worker: %v", r)
				}
			}()

			d.processFn(d.ctx, msg)
		}()
	}
}

// processMessage runs one Turn of the address's current Session: sends a
// Telegram typing indicator, runs the agent (with a timeout), applies the
// formatter post-step, records the transcript and the Turn's usage delta,
// and sends the reply. It returns what the post-reply compaction evaluation
// needs.
func (rt *sessionRuntime) processMessage(ctx context.Context, addr session.Address, msg channel.Message) turnInfo {
	logger := rt.logger

	logger.Info().
		Str("channel", msg.ChannelName).
		Str("address", string(addr)).
		Str("message", truncateLog(msg.Content, 100)).
		Msg("processing message")

	sKey := string(addr)

	// session-lifecycle 4.1: the address resolves to its current Session
	// before the history is loaded; everything conversational below (history,
	// agentskills session, usage) is keyed by the Session ID.
	cur, history := rt.sessions.History(ctx, addr)

	// criticalities-remediation (M14): dispose the streaming support loops
	// even when the turn panics — cleanup/terminal may never run, and the
	// Telegram flusher goroutine would leak its ticker until process exit.
	// scb is declared up-front so the deferred closure sees the value the
	// turn actually created.
	var scb *streamingCallbacks
	defer func() {
		if scb != nil && scb.dispose != nil {
			scb.dispose()
		}
	}()

	// Bootstrap: get (or create) the Session's agentskills session, with the
	// formatter/Compaction skills hidden from the catalog and pinned skills
	// re-marked Active after a restart.
	agentSkillsSID := rt.agentSkillsSession(ctx, cur, history, msg.ChannelName)

	stopTyping := startTypingLoop(ctx, rt.mgr, msg)
	// criticalities-remediation (M14): the stop is idempotent (sync.Once) —
	// the deferred call is the panic-path safety net so a panic in the turn
	// cannot leave the typing ticker running until process exit.
	defer stopTyping()

	msgCtx := skills.WithSessionID(ctx, agentSkillsSID)
	// Carry the Session ID into tools (error-digestion logging).
	msgCtx = context.WithValue(msgCtx, toolSessionKeyCtxKey{}, cur.ID)
	// tui-usage-observability: every LLM call under this turn context —
	// ReAct iterations, failover attempts, formatter post-step — attributes
	// to this Session's accumulator. Probes run without this value and record
	// nothing by construction.
	usageAcc := rt.usageStore.Accumulator(cur.ID)
	usageBefore := usageAcc.Snapshot()
	msgCtx = usage.WithAccumulator(msgCtx, usageAcc)

	history.Add(persistence.HistoryMessage{
		Role:      persistence.RoleUser,
		Content:   msg.Content,
		Timestamp: time.Now(),
	})

	// Wrap the agent call with a timeout so a hanging LLM cannot block the loop.
	runCtx, runCancel := context.WithTimeout(msgCtx, agentRunTimeout)
	defer runCancel()

	// The turn context (turnID,
	// timestamp, reminder/directive) is rendered ONCE per turn and recorded
	// right after the user message, so every attempt of the turn — and every
	// later turn's prompt — carries the same bytes.
	fmtCfg := rt.reg[msg.ChannelName]
	turnID := formatter.NewTurnID()
	turnStart := time.Now()
	tailIn, headIn := freezeTurnContextInput(fmtCfg, turnStart)
	history.Add(persistence.HistoryMessage{
		Role:      persistence.RoleUser,
		Kind:      persistence.KindTurnContext,
		Content:   formatter.BuildTurnContext(turnID, tailIn),
		Timestamp: turnStart,
	})
	headTurnContext := formatter.BuildTurnContext(turnID, headIn)

	// The stable prefix (formatter wrap, static shell, dynamic assets, skills
	// metadata) is byte-identical across turns and independent of the prompt
	// layout — the "Stable prefix ordering" requirement.
	layout := rt.agentReg.PromptLayout()
	parts := rt.collectPromptParts(runCtx, cur.ID, agentSkillsSID, msg.ChannelName)
	prefix := rt.prefixMessages(runCtx, parts, msg.ChannelName)

	// session-lifecycle D8a/D8c: the compaction summary (with the pinned
	// skills) is one user message right after the stable prefix; the
	// Turn-start guard may omit the oldest whole Turns from this Turn's view
	// when the estimate is over the threshold — never a synchronous
	// compaction.
	omit, extraSkills := rt.turnStartGuard(runCtx, cur, history, parts, agentSkillsSID, msg.ChannelName)
	lead := rt.leadMessage(runCtx, history, extraSkills)
	prefix = withLead(prefix, lead)

	// Per-attempt assembly:
	// the closure re-reads the LIVE history each attempt, so a same-candidate
	// retry or a failover continues the ReAct loop from the last complete
	// tool-call group recorded by the failed attempt — no tool re-executes.
	assemble := func(candidateLayout llm.PromptLayout) []*schema.Message {
		view := persistence.OmitOldestTurns(history.Messages(), omit)
		if candidateLayout == llm.LayoutTailDynamic {
			return assembleTailDynamic(prefix, view)
		}

		return assembleHeadSafe(prefix, view, headTurnContext)
	}

	// Transcript sink: every materialized assistant tool-call message
	// and tool result enters the history as it happens; each completed
	// tool-call group is delta-appended so a crash mid-turn leaves a
	// resumable transcript. resumeReady feeds the orchestrator's restart gate
	// from the in-memory history.
	runCtx = agent.WithTranscriptSink(runCtx, turnID, transcriptRecorder(ctx, history, logger))
	resumeReady := func() bool { return !history.HasOpenToolCallGroup() }

	logger.Debug().
		Str("event_type", "turn_context").
		Str("turn_id", turnID).
		Str("session_key", sKey).
		Str("session_id", cur.ID).
		Str("channel", msg.ChannelName).
		Str("layout", layout.String()).
		Msg("turn context assembled")

	// Streaming: when formatter_post_step is false (inline mode), deliver
	// chunks incrementally to the channel via an onChunk callback. When
	// formatter_post_step is true, pass nil — the post-step formatter needs
	// the complete response, so streaming delivery is not possible (P0).
	// (scb is declared above so the panic-path dispose defer can observe it.)
	if fmtCfg.skillName == "" || !fmtCfg.postStep {
		scb = buildStreamingCallback(runCtx, rt.mgr, msg, logger)
	}

	var onChunk func(string)
	if scb != nil {
		onChunk = scb.onChunk
	}

	// The turn runs through the registry's executor: the orchestrator in
	// production (per-attempt candidate selection, attempt budget, chain
	// walking), or the plain-runner adapter in tests.
	outcome, runErr := rt.agentReg.exec.ExecuteTurn(runCtx, sKey, assemble, resumeReady, onChunk)
	stopTyping()

	// Everything after the run completes is delivery bookkeeping — extracted
	// into its own function to keep processMessage within the complexity
	// budget (criticalities-remediation M14/M15 wiring included).
	deliverTurnResult(deliverArgs{
		ctx:       ctx,
		msgCtx:    msgCtx,
		runErr:    runErr,
		outcome:   outcome,
		sc:        scb,
		fmtr:      rt.fmtr,
		fmtCfg:    fmtCfg,
		msg:       msg,
		sKey:      sKey,
		mgr:       rt.mgr,
		logger:    logger,
		sanitizer: rt.sanitizer,
		history:   history,
		usageAcc:  usageAcc,
	})

	// session-lifecycle D7: the Turn's usage delta (main calls plus the
	// post-step formatter) is persisted as one turn_usage record at the end
	// of every Turn — success, terminal failure and llm_unavailable alike —
	// so totals survive a restart.
	delta := usageDelta(usageBefore, usageAcc.Snapshot())
	if err := history.AppendRecord(ctx, persistence.HistoryMessage{
		Kind:      persistence.KindTurnUsage,
		Usage:     &delta,
		Timestamp: time.Now(),
	}); err != nil {
		logger.Error().Err(err).Str("session_id", cur.ID).Msg("failed to append the turn usage record")
	}

	return turnInfo{
		sess:           cur,
		history:        history,
		channel:        msg.ChannelName,
		sid:            agentSkillsSID,
		measuredValid:  delta.LLMCalls > 0 && !rt.state(cur.ID).projected.Load(),
		appendedTokens: processTurnAppended(history.Messages()),
	}
}

// deliverArgs bundles the delivery-phase dependencies of deliverTurnResult.
type deliverArgs struct {
	ctx       context.Context
	mgr       *channel.Manager
	msgCtx    context.Context
	runErr    error
	outcome   agent.TurnOutcome
	sc        *streamingCallbacks
	fmtr      *formatter.Formatter
	fmtCfg    channelFormatterCfg
	msg       channel.Message
	sKey      string
	logger    zerolog.Logger
	sanitizer *privacy.Sanitizer
	history   *persistence.ConversationHistory
	usageAcc  *usage.Accumulator
}

// deliverTurnResult formats the successful response, persists it, performs
// streaming cleanup / terminal delivery (M15: on a context that outlives the
// dispatcher's cancellation), records the usage turn, and sends the final
// reply through the channel.
func deliverTurnResult(d deliverArgs) {
	runErr := d.runErr
	ctx := d.ctx
	msg := d.msg
	sKey := d.sKey
	logger := d.logger
	outcome := d.outcome

	// Terminal failure delivery (llm-outage-delivery): a failed turn must
	// never be silent. The terminal message is selected from the outcome
	// metadata — not from the error value — so a user who may have had a
	// device action run is told to check state instead of being invited to
	// blindly repeat the request.
	var terminal string
	var response string
	// llmExhausted marks a failure that satisfies errors.Is(err,
	// llm.ErrChainExhausted) — the orchestrator walked the whole candidate
	// chain. The final reply then carries the structured llm_unavailable
	// error code (llm-status-visibility) so a script can distinguish a
	// completed-but-failed request from a dead session. Any other failure
	// keeps the generic path and MUST NOT use the code.
	llmExhausted := runErr != nil && errors.Is(runErr, llm.ErrChainExhausted)

	if runErr != nil {
		logger.Error().
			Err(runErr).
			Str("channel", msg.ChannelName).
			Bool("tool_call_completed", outcome.ToolCallCompleted).
			Bool("content_delivered", outcome.ContentDelivered).
			Msg("agent error")

		terminal = terminalMessageFor(outcome)
		response = terminal
	} else {
		response = outcome.Reply
	}

	// The post-step formatter is an LLM call on the same health domain that
	// may just have failed — running it for a terminal message would compound
	// the outage with silence (design A1). Format only successful turns.
	if runErr == nil {
		fmtCtx, fmtCancel := context.WithTimeout(d.msgCtx, formatterTimeout)
		response = applyFormatterPostStep(fmtCtx, d.fmtr, d.fmtCfg, msg.ChannelName, response, logger)
		fmtCancel()
	}

	// Level 2 (universal net): the final response is the single choke point for
	// everything the user sees, so redact the home prefix here regardless of
	// where a path leaked from (tool result, reasoning, or prompt).
	response = d.sanitizer.Redact(response)

	// The final reply closes a successful
	// turn's transcript. A failed turn records no assistant line — the
	// terminal product string is a delivery artifact, not model output — and
	// its transcript is closed at the last complete tool-call group so the
	// next prompt never carries a dangling tool call.
	if runErr == nil {
		d.history.Add(persistence.HistoryMessage{
			Role:      persistence.RoleAssistant,
			Content:   response,
			Timestamp: time.Now(),
		})
	} else if n := d.history.CloseOpenToolCallGroup(); n > 0 {
		// session-lifecycle D9b: the executor has returned, so no further
		// attempt runs — unanswered calls get interrupted results (the calls
		// and the results that did complete stay visible to the model).
		logger.Warn().Str("session", sKey).Int("interrupted_results", n).Msg("failed turn left tool calls without results — interrupted results recorded")
	}

	if saveErr := d.history.Save(ctx); saveErr != nil {
		logger.Error().Err(saveErr).Msg("failed to save the session transcript")
	}

	// Streaming cleanup: flush remaining buffered content and perform final
	// edits (Telegram) or trailing newline (CLI) using the sanitized response.
	// Terminal delivery deliberately runs on the message-scoped ctx — NOT the
	// agent-run runCtx: when the failure IS the agent-run deadline, runCtx is
	// already cancelled and a send on it would be lost, reproducing the exact
	// silence this change exists to remove (design A4). The same applies to
	// cleanup: the agent-run timer stays armed until processMessage returns,
	// so a long turn's final Telegram edit could race the deadline.
	// criticalities-remediation (M15): the delivery path runs on a context
	// that OUTLIVES the dispatcher's cancellation — at SIGINT the dispatcher
	// ctx is already cancelled, and delivering the terminal message on it
	// would drop the very notice the delivery contract forbids dropping.
	// WithoutCancel keeps the message-scoped values (session, usage) while
	// discarding cancellation; a bounded deadline replaces it.
	deliveryCtx, deliveryCancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryGrace)
	defer deliveryCancel()

	if d.sc != nil {
		if terminal != "" {
			d.sc.terminal(deliveryCtx, terminal)
		} else {
			d.sc.cleanup(deliveryCtx, response)
		}
	}

	// tui-usage-observability (task 2.4): one delivered reply = one turn,
	// regardless of success, terminal failure, or llm_unavailable — the
	// attempt(s) still consumed tokens and the count drives the footer's
	// [N].
	d.usageAcc.AddTurn()

	// Send the final reply to the channel. When streaming delivered content
	// incrementally and the channel consumed it (CLI, Telegram), skip Send to
	// avoid duplicating output — the terminal message above was delivered by
	// scb.terminal for those channels. For ZMQ the final Send is still needed
	// to consume the pending reply slot exactly once (scb.terminal is a no-op
	// there), and for post-step / callback-less channels Send is the only
	// delivery path for the terminal message.
	if d.sc == nil || !d.sc.streamedDelivery {
		sendFinalReply(deliveryCtx, d.mgr, msg, response, llmExhausted, logger)
	} else {
		logger.Info().Str("channel", msg.ChannelName).Int("chars", len(response)).Msg("reply streamed")
	}
}

// sendFinalReply delivers the final reply through the channel's Send path,
// attaching the structured llm_unavailable error code (llm-status-visibility)
// when the turn's failure exhausted the LLM candidate chain — the ZMQ
// channel translates it to Status=error + ErrorCode on the wire so a script
// can distinguish a completed-but-failed request from a dead session.
func sendFinalReply(ctx context.Context, mgr *channel.Manager, msg channel.Message, response string, llmExhausted bool, logger zerolog.Logger) {
	outMsg := channel.Message{Content: response, Metadata: msg.Metadata}
	if llmExhausted {
		outMsg.Metadata = withMetadata(msg.Metadata, zmqcli.ErrorCode, protocol.ErrorCodeLLMUnavailable)
	}

	if sendErr := mgr.SendTo(ctx, msg.ChannelName, outMsg); sendErr != nil {
		logger.Error().Err(sendErr).Str("channel", msg.ChannelName).Msg("failed to send reply")
	} else {
		logger.Info().Str("channel", msg.ChannelName).Int("chars", len(response)).Msg("reply sent")
	}
}

// assembleTailDynamic concatenates the stable prefix and the history
// projection: the
// current turn's recorded turn-context line already sits at its
// chronological position — right after the user message, followed by any
// in-turn tool-call groups — so nothing is appended. The prefix is copied so
// no attempt can mutate another's input.
func assembleTailDynamic(prefix []*schema.Message, history []persistence.HistoryMessage) []*schema.Message {
	out := make([]*schema.Message, 0, len(prefix)+len(history))
	out = append(out, prefix...)

	return append(out, toSchemaMessages(history)...)
}

// assembleHeadSafe folds the current turn's frozen turn context into a
// leading system message and omits that turn's recorded
// turn-context line from the history projection so it is never duplicated;
// previous turns' turn-context lines stay as user messages at their
// positions.
func assembleHeadSafe(prefix []*schema.Message, history []persistence.HistoryMessage, headTurnContext string) []*schema.Message {
	projected := toSchemaMessages(withoutCurrentTurnContext(history))

	out := make([]*schema.Message, 0, 1+len(prefix)+len(projected))
	out = append(out, schema.SystemMessage(headTurnContext))
	out = append(out, prefix...)

	return append(out, projected...)
}

// withoutCurrentTurnContext returns history minus the turn-context line of
// its last turn (the turn in flight), if any.
func withoutCurrentTurnContext(history []persistence.HistoryMessage) []persistence.HistoryMessage {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Kind == persistence.KindTurnContext {
			out := make([]persistence.HistoryMessage, 0, len(history)-1)
			out = append(out, history[:i]...)

			return append(out, history[i+1:]...)
		}
		if history[i].IsTurnBoundary() {
			break
		}
	}

	return history
}

// freezeTurnContextInput captures the per-turn dynamic values once, at turn
// start: the tail-dynamic input (minute timestamp) is the one recorded in the
// transcript; the head-safe input differs only by its hour-quantized
// timestamp of the same instant.
func freezeTurnContextInput(fmtCfg channelFormatterCfg, at time.Time) (tail, head formatter.TurnContextInput) {
	tail = formatter.TurnContextInput{
		Timestamp:       formatTimeStamp(at, llm.LayoutTailDynamic),
		InlineFormatter: fmtCfg.skillName != "" && !fmtCfg.postStep,
	}

	head = tail
	head.Timestamp = formatTimeStamp(at, llm.LayoutHeadSafe)

	return tail, head
}

// transcriptRecorder returns the transcript sink bound to history: each
// emission is added as it materializes, and each completed tool-call group
// is delta-appended to the session JSONL before the next model call
// A failed append is logged — the
// in-memory history, which the restart gate reads, stays authoritative.
func transcriptRecorder(ctx context.Context, history *persistence.ConversationHistory, logger zerolog.Logger) func(schema.Message) {
	return func(m schema.Message) {
		line := persistence.HistoryMessage{Content: m.Content, Timestamp: time.Now()}

		switch m.Role {
		case schema.Assistant:
			line.Role = persistence.RoleAssistant
			for _, tc := range m.ToolCalls {
				line.ToolCalls = append(line.ToolCalls, persistence.ToolCallRecord{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case schema.Tool:
			line.Role = persistence.RoleTool
			line.ToolCallID = m.ToolCallID
		default:
			return
		}

		history.Add(line)

		if line.Role == persistence.RoleTool && !history.HasOpenToolCallGroup() {
			if err := history.Save(ctx); err != nil {
				logger.Error().Err(err).Msg("failed to save tool-call group checkpoint")
			}
		}
	}
}

// currentTimeStamp renders the current date/time, quantized to the active
// layout's granularity: minute under LayoutTailDynamic,
// hour under LayoutHeadSafe. "00" is not a Go reference-time token, so under
// LayoutHeadSafe it is emitted as a literal, making the rendered string
// identical for the whole wall-clock hour rather than merely truncating the
// real minute value.
func currentTimeStamp(layout llm.PromptLayout) string {
	return formatTimeStamp(time.Now(), layout)
}

// formatTimeStamp renders t with the layout's quantization (see
// currentTimeStamp).
func formatTimeStamp(t time.Time, layout llm.PromptLayout) string {
	format := "Monday, 02 January 2006, 15:04 MST"
	if layout != llm.LayoutTailDynamic {
		format = "Monday, 02 January 2006, 15:00 MST"
	}

	return t.Format(format)
}

// typingInterval is how often we re-send the Telegram "typing…" action.
// Telegram expires the indicator after ~5 s, so 4 s keeps it visible.
const typingInterval = 4 * time.Second

// startTypingLoop sends a Telegram "typing…" chat action immediately and then
// repeats it every typingInterval until the returned stop function is called.
// For non-Telegram channels the returned stop function is a no-op.
func startTypingLoop(ctx context.Context, mgr *channel.Manager, msg channel.Message) (stop func()) {
	noop := func() {}

	if msg.ChannelName != telegram.ChannelName {
		return noop
	}

	chatIDStr, ok := msg.Metadata[telegram.MetadataChatID]
	if !ok {
		return noop
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return noop
	}

	ch := mgr.Channel(msg.ChannelName)
	if ch == nil {
		return noop
	}

	tch, ok := ch.(*telegram.Channel)
	if !ok {
		return noop
	}

	// Send immediately so the indicator appears right away.
	tch.SendTyping(ctx, chatID)

	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(typingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				tch.SendTyping(ctx, chatID)
			}
		}
	}()

	var once sync.Once

	return func() {
		once.Do(func() { close(done) })
	}
}

// toSchemaMessages projects the persisted session transcript onto Eino
// schema messages: user lines (turn-context lines included) as user
// messages, assistant lines with their tool calls, tool results with their
// call id and the tool name of the call they answer, and system lines as
// system messages.
func toSchemaMessages(turns []persistence.HistoryMessage) []*schema.Message {
	out := make([]*schema.Message, 0, len(turns))
	callNames := map[string]string{}

	for _, t := range turns {
		switch t.Role {
		case persistence.RoleUser:
			out = append(out, schema.UserMessage(t.Content))
		case persistence.RoleAssistant:
			var calls []schema.ToolCall
			for _, tc := range t.ToolCalls {
				callNames[tc.ID] = tc.Name
				calls = append(calls, schema.ToolCall{
					ID:       tc.ID,
					Type:     "function",
					Function: schema.FunctionCall{Name: tc.Name, Arguments: tc.Arguments},
				})
			}
			out = append(out, schema.AssistantMessage(t.Content, calls))
		case persistence.RoleTool:
			out = append(out, schema.ToolMessage(t.Content, t.ToolCallID, schema.WithToolName(callNames[t.ToolCallID])))
		case persistence.RoleSystem:
			out = append(out, schema.SystemMessage(t.Content))
		}
	}

	return out
}

// truncateLog shortens s to at most maxLen runes for log output.
func truncateLog(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}

	return string(runes[:maxLen]) + "…"
}

// getZMQChannel returns the *zmqcli.Channel if the given channel name maps to
// a ZMQ CLI channel, or nil otherwise. This allows the notifier to call
// SendNotification for progress messages without consuming the pending reply slot.
func getZMQChannel(mgr *channel.Manager, channelName string) *zmqcli.Channel {
	ch := mgr.Channel(channelName)
	if ch == nil {
		return nil
	}

	zmqCh, ok := ch.(*zmqcli.Channel)
	if !ok {
		return nil
	}

	return zmqCh
}

// orchestratorStatusSource adapts *orchestrator.Orchestrator to
// zmqcli.ModelStatusSource (llm-status-visibility), keeping the orchestrator's
// dependency graph out of the channel package. The default/fallback mapping
// follows the chain order compiled by llm.CompileCandidates: index 0 is the
// configured llm.default; any higher active index is a fallback.
type orchestratorStatusSource struct {
	orch *orchestrator.Orchestrator
}

// Guarantees at compile time that orchestratorStatusSource implements
// zmqcli.ModelStatusSource.
var _ zmqcli.ModelStatusSource = orchestratorStatusSource{}

// ModelStatus returns the current status for handshake reporting.
func (s orchestratorStatusSource) ModelStatus() zmqcli.ModelStatusEvent {
	return modelStatusEventFor(s.orch.Snapshot())
}

// SubscribeModelStatus bridges the orchestrator's transition stream to the
// channel's event shape. The returned cancel stops the bridge goroutine and
// releases the orchestrator subscription.
func (s orchestratorStatusSource) SubscribeModelStatus() (<-chan zmqcli.ModelStatusEvent, func()) {
	transitions, cancelOrch := s.orch.Subscribe()

	events := make(chan zmqcli.ModelStatusEvent, 16)
	done := make(chan struct{})

	go func() {
		defer close(events)

		for {
			select {
			case <-done:
				return
			case tr, ok := <-transitions:
				if !ok {
					return
				}

				select {
				case events <- modelStatusEventFor(tr.To):
				case <-done:
					return
				}
			}
		}
	}()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			cancelOrch()
		})
	}

	return events, cancel
}

// modelStatusEventFor translates an orchestrator snapshot into the wire
// model-status event: unavailable when no candidate is healthy, default when
// the active candidate is the configured default (chain index 0), fallback
// for any other active candidate.
func modelStatusEventFor(snap orchestrator.Snapshot) zmqcli.ModelStatusEvent {
	if snap.State == orchestrator.StateUnavailable {
		return zmqcli.ModelStatusEvent{State: protocol.ModelStateUnavailable}
	}

	state := protocol.ModelStateFallback
	if snap.ActiveIndex == 0 {
		state = protocol.ModelStateDefault
	}

	return zmqcli.ModelStatusEvent{Ref: snap.ActiveRef, State: state}
}

// withMetadata returns a copy of meta with key set to value. It never
// mutates the caller's map — the incoming message metadata is shared across
// the whole turn (streaming callbacks, notifier, history) and must stay
// stable.
func withMetadata(meta map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[key] = value

	return out
}

// streamWriter is the CLI-channel surface needed for streaming delivery.
type streamWriter interface {
	Writer() io.Writer
}

// progressNotifier is the ZMQ-channel surface needed for progress and
// stream-chunk notifications that do not consume the pending reply slot.
type progressNotifier interface {
	SendNotification(ctx context.Context, sessionID, content string) error
}

// telegramStreamSender is the Telegram-channel surface needed for streaming
// delivery (first message creation + subsequent edits).
type telegramStreamSender interface {
	SendStreamMessage(ctx context.Context, chatID int64, content string) (int, error)
	SendStreamEdit(ctx context.Context, chatID int64, messageID int, content string) error
}

// Compile-time assertions: the concrete channel types satisfy the streaming
// surfaces above.
var (
	_ streamWriter         = (*cli.Channel)(nil)
	_ progressNotifier     = (*zmqcli.Channel)(nil)
	_ telegramStreamSender = (*telegram.Channel)(nil)
)

// streamState holds per-channel state for incremental streaming delivery.
type streamState struct {
	// Telegram: message_id obtained from the first sendMessage, used for
	// subsequent editMessageText calls.
	tgMessageID int
	tgChatID    int64
	// tgBuffer marks the unflushed-tail state: a chunk sets tgDirty, the
	// flusher clears it. criticalities-remediation (H7): intermediate edits
	// carry the CUMULATIVE tgFull text, so a mid-stream edit never replaces
	// the message with only the last flush window.
	tgBuffer strings.Builder
	tgDirty  bool
	// tgFull accumulates everything received so far. The terminal path uses
	// it to append the incomplete-response notice after the partial output
	// instead of replacing it (llm-outage-delivery, task 3.3).
	tgFull   strings.Builder
	tgTicker *time.Ticker
	tgDone   chan struct{}
	// tgClosed guards the H7 race: set under tgMu BEFORE the final edit, so
	// no flushTelegram can follow the final edit with stale partial content.
	tgClosed bool
	tgMu     sync.Mutex
}

// streamingCallbacks bundles the chunk callback and the post-run delivery
// functions. cleanup finalises a successful streamed turn; terminal delivers
// the fixed terminal failure message for a failed turn (llm-outage-delivery).
// Both take a context parameter supplied by the dispatcher at call time —
// the message-scoped context, never the cancelled agent-run context, so a
// turn that fails BY the agent-run deadline still delivers its terminal
// message (design A4).
type streamingCallbacks struct {
	onChunk          func(string)
	cleanup          func(ctx context.Context, finalContent string)
	terminal         func(ctx context.Context, terminalMsg string)
	streamedDelivery bool // true when the final Send should be skipped
	// dispose closes the per-turn support loops (typing indicator, Telegram
	// flush ticker) WITHOUT delivering anything. It is the panic-path safety
	// net in processMessage (criticalities-remediation M14): cleanup/terminal
	// may never run when the turn panics, and the loops would otherwise leak
	// tickers until process exit.
	dispose func()
}

// telegramStreamEditInterval is how often the Telegram streaming flusher
// sends editMessageText updates. Telegram's practical edit rate limit is
// ~1 per second per message.
const telegramStreamEditInterval = 1 * time.Second

// terminalAppendMargin is the rune-count safety margin below
// telegram.MaxMessageLength used when deciding whether the partial output
// plus the terminal notice still fits a single edited message. HTML
// sanitisation can expand the text (entity escaping), and SendStreamEdit
// truncates at the limit — the margin guarantees the terminal notice is
// never the part that gets truncated away.
const terminalAppendMargin = 256

// buildStreamingCallback constructs a channel-specific streaming delivery
// pipeline.
//
// streamedDelivery=true means the dispatcher should skip the final
// Channel.Send() (CLI writes chunks directly, Telegram does a final edit).
// streamedDelivery=false means Send is still needed (ZMQ must consume the
// pending reply slot with the final reply).
//
// Channels are matched by capability interfaces (streamWriter,
// progressNotifier, telegramStreamSender) rather than concrete types, so the
// dispatcher depends only on the delivery surface each channel exposes and
// tests can substitute fakes. cli and zmqcli share the logical channel name
// "cli". Returns nil when the channel exposes none of the streaming
// surfaces, in which case the dispatcher's final Send is the only delivery
// path — including for terminal failure messages.
func buildStreamingCallback(
	ctx context.Context,
	mgr *channel.Manager,
	msg channel.Message,
	logger zerolog.Logger,
) *streamingCallbacks {
	ch := mgr.Channel(msg.ChannelName)
	if ch == nil {
		return nil
	}

	// CLI channel (stdin/stdout): chunks are written directly to the writer.
	if w, ok := ch.(streamWriter); ok {
		var wrote bool // tracks whether any partial output reached the writer

		return &streamingCallbacks{
			onChunk: func(chunk string) {
				if chunk != "" {
					wrote = true
				}

				fmt.Fprint(w.Writer(), chunk)
			},
			cleanup: func(context.Context, string) {
				// Chunks were written raw as they arrived; finalise with a
				// trailing newline (unchanged success behaviour).
				fmt.Fprintln(w.Writer())
			},
			terminal: func(_ context.Context, terminalMsg string) {
				// A failed CLI turn must not collapse to a bare newline
				// (task 3.1): print the terminal message after whatever
				// partial output is already on screen, separated by a
				// newline so the notice cannot be mistaken for the answer.
				if wrote {
					fmt.Fprintln(w.Writer())
				}

				fmt.Fprintln(w.Writer(), terminalMsg)
			},
			dispose:          func() {}, // CLI has no per-turn support loops
			streamedDelivery: true,
		}
	}

	// ZMQ CLI channel: chunks ride SendNotification (progress) so the pending
	// reply slot stays free for the final reply.
	if pn, ok := ch.(progressNotifier); ok {
		sessionID := msg.Metadata[zmqcli.ConnectionID]

		return &streamingCallbacks{
			dispose: func() {}, // ZMQ progress has no per-turn support loops
			onChunk: func(chunk string) {
				if notifyErr := pn.SendNotification(ctx, sessionID, chunk); notifyErr != nil {
					logger.Warn().Err(notifyErr).Str("channel", msg.ChannelName).Msg("failed to send stream chunk")
				}
			},
			cleanup: func(context.Context, string) {},
			// terminal is a no-op: the terminal message is delivered by the
			// dispatcher's final Send so the pending reply slot is consumed
			// exactly once (task 3.4).
			terminal:         func(context.Context, string) {},
			streamedDelivery: false,
		}
	}

	// Telegram channel.
	if ts, ok := ch.(telegramStreamSender); ok {
		chatIDStr, ok := msg.Metadata[telegram.MetadataChatID]
		if !ok || chatIDStr == "" {
			return nil
		}

		chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
		if err != nil {
			return nil
		}

		st := &streamState{
			tgChatID: chatID,
		}
		st.tgTicker = time.NewTicker(telegramStreamEditInterval)
		st.tgDone = make(chan struct{})

		// criticalities-remediation (M14): panic-path disposal — closes the
		// flusher and stops the ticker without delivering, so a panic in
		// processMessage cannot leak the ticker goroutines.
		dispose := func() {
			st.tgMu.Lock()
			defer st.tgMu.Unlock()

			if !st.tgClosed {
				st.tgClosed = true
				close(st.tgDone)
			}
			st.tgTicker.Stop()
		}

		// Background flusher: every tick, flush buffered content via editMessageText.
		go func() {
			for {
				select {
				case <-st.tgDone:
					return
				case <-st.tgTicker.C:
					st.flushTelegram(ctx, ts, logger)
				}
			}
		}()

		return &streamingCallbacks{
			onChunk: func(chunk string) {
				st.tgMu.Lock()
				defer st.tgMu.Unlock()

				if st.tgClosed {
					return // H7: nothing lands after the completion path
				}

				st.tgFull.WriteString(chunk)

				if st.tgMessageID == 0 {
					msgID, sendErr := ts.SendStreamMessage(ctx, chatID, chunk)
					if sendErr != nil {
						logger.Warn().Err(sendErr).Str("channel", msg.ChannelName).Msg("failed to send first stream chunk")

						return
					}
					st.tgMessageID = msgID
					st.tgDirty = true

					return
				}

				st.tgBuffer.WriteString(chunk)
				st.tgDirty = true
			},
			cleanup: func(tctx context.Context, finalContent string) {
				// H7: the closed flag is set UNDER the same mutex the flusher
				// holds, so no flush can land after the final edit below.
				st.tgMu.Lock()
				defer st.tgMu.Unlock()

				if !st.tgClosed {
					st.tgClosed = true
					close(st.tgDone)
				}
				st.tgTicker.Stop()

				if st.tgMessageID == 0 {
					return
				}

				if editErr := ts.SendStreamEdit(tctx, st.tgChatID, st.tgMessageID, finalContent); editErr != nil {
					logger.Warn().Err(editErr).Str("channel", "telegram").Msg("failed final stream edit")
				}
			},
			terminal: func(tctx context.Context, terminalMsg string) {
				// H7: same barrier as cleanup — under the mutex, before any edit.
				st.tgMu.Lock()
				defer st.tgMu.Unlock()

				if !st.tgClosed {
					st.tgClosed = true
					close(st.tgDone)
				}
				st.tgTicker.Stop()

				// No stream message exists (failure before the first chunk):
				// create exactly one new message carrying the terminal text —
				// the pre-change defect was total silence here (task 3.2).
				if st.tgMessageID == 0 {
					if _, sendErr := ts.SendStreamMessage(tctx, st.tgChatID, terminalMsg); sendErr != nil {
						logger.Warn().Err(sendErr).Str("channel", "telegram").Msg("failed to send terminal message")
					}

					return
				}

				// Partial output is already on screen: retain it and append the
				// terminal message without replacing the partial (task 3.3).
				// When the combined text would brush the 4096-char limit, flush
				// the partial as-is and send the terminal message as a new
				// message so the notice itself is never truncated away.
				combined := st.tgFull.String() + "\n\n" + terminalMsg
				if len([]rune(combined)) <= telegram.MaxMessageLength-terminalAppendMargin {
					if editErr := ts.SendStreamEdit(tctx, st.tgChatID, st.tgMessageID, combined); editErr != nil {
						logger.Warn().Err(editErr).Str("channel", "telegram").Msg("failed terminal stream edit")
					}

					return
				}

				if editErr := ts.SendStreamEdit(tctx, st.tgChatID, st.tgMessageID, st.tgFull.String()); editErr != nil {
					logger.Warn().Err(editErr).Str("channel", "telegram").Msg("failed terminal stream edit")
				}

				if _, sendErr := ts.SendStreamMessage(tctx, st.tgChatID, terminalMsg); sendErr != nil {
					logger.Warn().Err(sendErr).Str("channel", "telegram").Msg("failed to send terminal message")
				}
			},
			dispose:          dispose,
			streamedDelivery: true,
		}
	}

	return nil
}

// flushTelegram sends the cumulative content as an editMessageText.
// criticalities-remediation (H7): the edit carries st.tgFull (everything
// received so far), never only the chunks buffered since the previous flush;
// and a flush cannot land after the completion path (tgClosed, checked under
// the same mutex that guards the final edit).
func (st *streamState) flushTelegram(ctx context.Context, ts telegramStreamSender, logger zerolog.Logger) {
	st.tgMu.Lock()
	defer st.tgMu.Unlock()

	// A flush racing the completion path loses: once tgClosed is set, every
	// subsequent flush is a no-op — the final edit is the last edit.
	if st.tgClosed || st.tgMessageID == 0 || !st.tgDirty {
		return
	}

	st.tgDirty = false
	content := st.tgFull.String()

	if editErr := ts.SendStreamEdit(ctx, st.tgChatID, st.tgMessageID, content); editErr != nil {
		st.tgDirty = true // retry on the next tick
		logger.Warn().Err(editErr).Str("channel", "telegram").Msg("failed to edit stream chunk")
	}
}

// applyFormatterPostStep runs the post-step formatter LLM call when a skill
// name is configured for this channel AND formatter_post_step is true.
// Returns response unchanged otherwise.
func applyFormatterPostStep(
	ctx context.Context,
	fmtr *formatter.Formatter,
	fmtCfg channelFormatterCfg,
	channelName string,
	response string,
	logger zerolog.Logger,
) string {
	if fmtCfg.skillName == "" || !fmtCfg.postStep {
		return response
	}

	formatted, err := fmtr.Format(ctx, fmtCfg.skillName, response)
	if err != nil {
		// An auxiliary consumer failure must never fail a turn whose main
		// response succeeded (llm-outage-delivery A5): the content is already
		// correct, only its presentation is lost. Deliver the unformatted
		// response and log at WARN naming the failed consumer.
		logger.Warn().
			Err(err).
			Str("channel", channelName).
			Str("consumer", "post-step-formatter").
			Msg("post-step formatter failed; delivering unformatted response")

		return response
	}

	return formatted
}

// applyFormatterInlineReinforcement implements the position-1 half of the
// formatter inline-reinforcement mode (formatter_post_step: false and a
// non-empty formatter_skill): wrap the formatter skill's SKILL.md in a
// static <formatter_skill>...</formatter_skill> tag and prepend it as a
// SystemMessage. The current UserMessage is never rewritten — the
// verification reminder and formatting directive live in the separate
// per-turn turn-context message built by buildTurnContextInput +
// formatter.BuildTurnContext, so the same conversation slot holds identical
// bytes across turns.
//
// Under formatter_post_step: true or empty formatter_skill this function
// returns conversation unchanged, guaranteeing the post-step path is
// byte-for-byte identical to its pre-change behavior.
func applyFormatterInlineReinforcement(
	ctx context.Context,
	conversation []*schema.Message,
	fmtCfg channelFormatterCfg,
	fmtr *formatter.Formatter,
	channelName string,
	logger zerolog.Logger,
) []*schema.Message {
	if fmtCfg.skillName == "" || fmtCfg.postStep {
		return conversation
	}

	if fmtPrompt, fmtErr := fmtr.LoadSkillPrompt(ctx, fmtCfg.skillName); fmtErr == nil && fmtPrompt != "" {
		wrapped := "<formatter_skill>\n" + fmtPrompt + "\n</formatter_skill>"
		conversation = append([]*schema.Message{schema.SystemMessage(wrapped)}, conversation...)
	} else if fmtErr != nil {
		logger.Warn().Err(fmtErr).Str("channel", channelName).Msg("failed to load inline formatter skill")
	}

	return conversation
}

// shutdownResources performs graceful cleanup of long-lived resources
// (excluding the channel manager, which is stopped earlier to allow the
// dispatcher to drain before resources are released).
func shutdownResources(
	mcpRuntime homemcp.Runtime,
	skillCache *skillcache.SkillCache,
	skillsMgr *skills.Manager,
	rootLogger zerolog.Logger,
) {
	logger := rootLogger.With().Str("component", "shutdown").Logger()

	if mcpRuntime != nil {
		if err := mcpRuntime.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing MCP client")
		}
	}

	if err := skillCache.Close(); err != nil {
		logger.Error().Err(err).Msg("error closing skill cache")
	}

	if err := skillsMgr.Close(); err != nil {
		logger.Error().Err(err).Msg("error closing skills manager")
	}
}

// warnThoughtsLevelMismatch emits a single WARN record when logging.thoughts is
// true but no sink is at debug or trace (meaning thought records will never be
// emitted anywhere).  This is a configuration hint, not a hard error.
func warnThoughtsLevelMismatch(logger zerolog.Logger, cfg *config.Config, res logging.Result) {
	if !cfg.Logging.Thoughts {
		return
	}

	if !thoughtsLevelMismatch(res.FileLevel, res.ConsoleLevel, res.HasFile) {
		return
	}

	logger.Warn().
		Str("console_level", res.ConsoleLevel.String()).
		Str("file_level", res.FileLevel.String()).
		Bool("has_file", res.HasFile).
		Msg("logging.thoughts is enabled but no log sink is at debug or trace; thought events will not be emitted (raise logging.level or logging.console_level to debug)")
}

// thoughtsLevelMismatch returns true when neither the console level nor (when
// a log file is configured) the file level admits DEBUG records.
// Exported as a narrow helper so the predicate can be unit-tested in isolation.
func thoughtsLevelMismatch(fileLevel, consoleLevel zerolog.Level, hasFile bool) bool {
	if consoleLevel <= zerolog.DebugLevel {
		return false
	}

	return !hasFile || fileLevel > zerolog.DebugLevel
}

// warnOllamaContextWindowMismatch is the WARN body factored out for testing.
func warnOllamaContextWindowMismatch(logger zerolog.Logger, cfg *config.Config) {
	for name, entry := range cfg.LLM.Providers {
		if entry.Provider != config.ProviderOllama || entry.ContextWindow == 0 {
			continue
		}
		if entry.Options == nil || entry.Options.NumCtx == nil || *entry.Options.NumCtx == entry.ContextWindow {
			continue
		}

		logger.Warn().
			Str("provider", name).
			Int("num_ctx", *entry.Options.NumCtx).
			Int("context_window", entry.ContextWindow).
			Msg("ollama options.num_ctx differs from context_window — Ollama silently truncates the prompt to num_ctx, so the occupancy percentage computed from context_window is unreliable")
	}
}

// warnSecretFilePermissions checks the permissions of every secret material
// file referenced through the configuration (criticalities-remediation LOW
// secret-handling): a file readable by group or others produces a WARN naming the
// path and the offending bits. Boot proceeds — correcting the permissions is
// the operator's action.
// warnConfigPermissions reports when the elected config file is readable by
// group or others (unix only — the Windows ACL typically already restricts
// it, and the check is inapplicable there). Advisory, never fatal.
func warnConfigPermissions(path string) string {
	if onWindows() || path == "" {
		return ""
	}

	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := info.Mode().Perm(); perm&(0o0040|0o0004) != 0 {
		return fmt.Sprintf("config file %s is readable by group or others (%s) — consider tightening to 0600 (it may hold provider keys and tokens)", path, perm.String())
	}

	return ""
}

//nolint:gochecknoglobals // test seam for the OS gate of the permission warning
var onWindows = func() bool { return runtime.GOOS == "windows" }

func warnSecretFilePermissions(cfg *config.Config, rootLogger zerolog.Logger) {
	logger := rootLogger.With().Str("component", "config-secrets").Logger()

	check := func(path, kind string) {
		if path == "" {
			return
		}

		info, err := os.Stat(path)
		if err != nil {
			return
		}

		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			logger.Warn().
				Str("file", path).
				Str("kind", kind).
				Str("perm", perm.String()).
				Msg("secret file is readable by group or others — tighten its permissions (0600)")
		}
	}

	for name, providerCfg := range cfg.LLM.Providers {
		check(providerCfg.APIKeyFile, "llm.providers."+name+".api_key_file")
	}

	if cfg.Channels.ZMQCLI.Enabled {
		check(cfg.Channels.ZMQCLI.AuthTokenFile, "channels.zmqcli.auth_token_file")
	}

	for key, serverCfg := range cfg.MCPServers {
		if serverCfg.OAuth != nil {
			check(serverCfg.OAuth.ClientSecretFile, "mcp_servers."+key+".oauth.client_secret_file")
		}
	}
}

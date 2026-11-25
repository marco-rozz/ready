package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/agent/toolnames"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/formatter"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/okf"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/skillssync"
	"github.com/marco-rozz/ready/internal/subagent"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/marco-rozz/ready/internal/twin"
	"github.com/marco-rozz/ready/internal/usage"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// runtimeGeneration is the reloadable half of `ready serve` (runtime-reload
// capability): everything rebuilt from config.yaml and the Secret store on a
// Runtime reload. The Process Shell (serveShell and its locks, session and
// usage stores, dispatcher, the zmqcli listener) outlives every generation.
type runtimeGeneration struct {
	ctx    context.Context
	cancel context.CancelFunc

	cfg               *config.Config
	bootCfg           *config.Config
	activeProfileName string
	bootSecrets       *bootSecretState

	llmCands      []orchestrator.Candidate
	llmResolver   *toolnames.Resolver
	llmDisclosure bool
	llmProfile    string
	childRunnerMu sync.Mutex
	childRunners  map[string]map[string]agent.Runner

	skillsMgr    *skills.Manager
	skillCache   *skillcache.SkillCache
	haTwin       *hatwin.Twin
	mcpRuntime   homemcp.Runtime
	agentTools   []tool.BaseTool
	cfgMgr       *tools.ConfigManager
	orch         *orchestrator.Orchestrator
	agentReg     *agentRegistry
	fmtr         *formatter.Formatter
	fmtRegistry  formatterRegistry
	bootstrapper *bootstrap.Bootstrapper
	bootstrapCfg map[string][]string
	aux          model.BaseChatModel
}

// generationStageError adds the failed builder stage while preserving the cause.
func generationStageError(stage string, err error) error {
	return fmt.Errorf("failed to %s: %w", stage, err)
}

// ── generation leases (subagent-runtime D14, task 3.4) ───────────────────

// generationLeases is the refcounted lease API over the active runtime
// generation. Background child dispatches NEVER take the session worker's
// generation gate (a child waiting for RLock while its owner holds RLock
// inside subagent_wait would stall every address, and a cascade close under
// the write lock that joins children blocked on RLock would deadlock):
// instead a dispatch acquires a lease on the generation it started on and
// releases it at dispatch end. The swap installs the new generation without
// waiting for leases; a generation is retired (shutdown) when its last lease
// is released — bounded by the child dispatch timeout. Leases are never
// acquired while holding the gate.
type generationLeases struct {
	mu      sync.Mutex
	current *runtimeGeneration
	refs    int
	// retired holds generations replaced while still leased, in swap order.
	retired []*runtimeGeneration

	// retire shuts down a fully released generation (injectable for tests).
	retire func(ctx context.Context, g *runtimeGeneration)

	logger zerolog.Logger
}

// newGenerationLeases builds the lease manager over an initial generation.
func newGenerationLeases(initial *runtimeGeneration, logger zerolog.Logger) *generationLeases {
	return &generationLeases{current: initial, logger: logger}
}

// Acquire takes one lease on the active generation and returns it with the
// release func. Never call while holding the generation gate. The returned
// generation is consumed by the child driver (cmd wiring, subagent-runtime
// 4.1).
func (l *generationLeases) Acquire(ctx context.Context) (*runtimeGeneration, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.refs++
	g := l.current

	return g, func() { l.Release(ctx, g) }
}

// Swap installs next as the active generation from the lease manager's
// perspective and retires the previous one — immediately when unleased,
// deferred to the last lease release otherwise (logged). Called by the swap
// path after applyGeneration, outside the gate. The swap NEVER waits for
// leases.
func (l *generationLeases) Swap(ctx context.Context, next *runtimeGeneration) {
	l.mu.Lock()

	previous := l.current
	if previous == nil || previous == next {
		l.current = next
		l.mu.Unlock()

		return
	}

	l.current = next

	if l.refs > 0 {
		l.retired = append(l.retired, previous)
		l.mu.Unlock()

		l.logger.Info().Msg("previous runtime generation leased by a background dispatch — retirement deferred to its release")

		return
	}

	l.mu.Unlock()

	l.retireAll(ctx, []*runtimeGeneration{previous})
}

// Release drops one lease. When the released generation is the active one
// the refcount just drops; when it is a deferred-retirement generation and
// this was its last lease, every deferral is retired now (the leases and the
// deferrals drain together — a child finishing on an old generation retires
// every generation older than the active one).
func (l *generationLeases) Release(ctx context.Context, g *runtimeGeneration) {
	l.mu.Lock()

	if g == l.current {
		if l.refs > 0 {
			l.refs--
		}
		l.mu.Unlock()

		return
	}

	deferred := false
	for _, r := range l.retired {
		if r == g {
			deferred = true

			break
		}
	}

	if !deferred {
		l.mu.Unlock()

		return
	}

	if l.refs--; l.refs > 0 {
		l.mu.Unlock()

		return
	}

	pending := append([]*runtimeGeneration(nil), l.retired...)
	l.retired = nil
	l.refs = 0
	l.mu.Unlock()

	l.retireAll(ctx, pending)
}

// retireAll shuts down the deferred generations (in swap order).
func (l *generationLeases) retireAll(ctx context.Context, pending []*runtimeGeneration) {
	for _, g := range pending {
		if l.retire != nil {
			l.retire(ctx, g)

			continue
		}

		g.shutdown(ctx, l.logger)
	}
}

// PendingRetired reports the count of generations awaiting lease release
// (test seam).
func (l *generationLeases) PendingRetired() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.retired)
}

// ActiveLeases reports the live lease count (test seam).
func (l *generationLeases) ActiveLeases() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.refs
}

// ShutdownAll cancels every lease-holder's generation context and retires
// every generation (active + deferred) — the daemon shutdown path. The
// dispatch contexts are cancelled by the generation's own cancel (the child
// driver's deadline ctx derives from it).
func (l *generationLeases) ShutdownAll(ctx context.Context) {
	l.mu.Lock()

	current := l.current
	pending := append([]*runtimeGeneration(nil), l.retired...)
	l.retired = nil
	l.current = nil
	l.refs = 0
	l.mu.Unlock()

	if current != nil {
		pending = append([]*runtimeGeneration{current}, pending...)
	}

	l.retireAll(ctx, pending)
}

// shutdown cancels the generation context and releases its resources. It is
// nil-safe and tolerates a partially built generation.
func (g *runtimeGeneration) shutdown(ctx context.Context, logger zerolog.Logger) {
	if g == nil {
		return
	}
	if g.cancel != nil {
		g.cancel()
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if g.bootstrapper != nil {
		if err := g.bootstrapper.CloseAll(shutdownCtx); err != nil {
			logger.Error().Err(err).Msg("error closing bootstrapper sessions")
		}
	}
	if g.haTwin != nil {
		if err := g.haTwin.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing HA digital twin")
		}
	}
	if g.mcpRuntime != nil {
		if err := g.mcpRuntime.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing MCP client")
		}
	}
	if g.skillCache != nil {
		if err := g.skillCache.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing skill cache")
		}
	}
	if g.skillsMgr != nil {
		if err := g.skillsMgr.Close(); err != nil {
			logger.Error().Err(err).Msg("error closing skills manager")
		}
	}
	if g.orch != nil {
		g.orch.Wait()
	}
}

// serveShell is the Process Shell: it lives for the whole OS process and owns
// every resource a Runtime reload must not drop.
type serveShell struct {
	ctx         context.Context
	opts        ServeOptions
	subagents   *subagent.Runtime
	okfWriter   *okf.Writer
	resolution  config.Resolution
	rootLogger  zerolog.Logger
	logger      zerolog.Logger
	configState *tools.ConfigManagerState

	mgr      *channel.Manager
	rt       *sessionRuntime
	coord    *reloadCoordinator
	zmqBind  atomic.Value // string: bind of the live zmqcli listener
	reloadFn zmqcli.RuntimeReloadHandler
}

// initInfra creates the skills manager and the skill cache.
func initInfra(ctx context.Context, cfg *config.Config, rootLogger zerolog.Logger) (*skills.Manager, *skillcache.SkillCache, error) {
	logger := rootLogger.With().Str("component", "infra").Logger()

	skillsMgr, err := skills.NewManager(ctx, &cfg.Skills, rootLogger)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create skills manager: %w", err)
	}

	sc, err := skillcache.New()
	if err != nil {
		_ = skillsMgr.Close()

		return nil, nil, fmt.Errorf("failed to create skill cache: %w", err)
	}
	sc.WithLogger(logger.With().Str("component", "skillcache").Logger())

	return skillsMgr, sc, nil
}

// buildGeneration builds one Runtime Generation from the (already prepared)
// configuration under a fresh generation context. On failure everything built
// so far is released and no process-level effect remains, so a failed reload
// leaves the previous generation active.
func (s *serveShell) buildGeneration(cfg *config.Config) (g *runtimeGeneration, err error) {
	genCtx, cancel := context.WithCancel(s.ctx)
	g = &runtimeGeneration{ctx: genCtx, cancel: cancel, cfg: cfg}
	defer func() {
		if err != nil {
			g.shutdown(genCtx, s.rootLogger)
			g = nil
		}
	}()

	logger := s.logger
	cfg.Origin = s.resolution.Origin

	profileName, activeProfile, err := cfg.ResolveProfile(s.opts.Profile, os.Getenv(config.ProfileEnvName))
	if err != nil {
		return g, fmt.Errorf("failed to resolve MCP profile: %w", err)
	}
	g.activeProfileName = profileName

	// The agent's encrypted Secret store: pending credentials are DERIVED
	// here from the references and the store; a merely absent ZMQ shared
	// secret is generated, a present-but-broken one refuses the build.
	bootSecrets, err := prepareBootSecrets(genCtx, cfg, s.rootLogger, s.resolution.AgentDir)
	if err != nil {
		return g, generationStageError("prepare boot secrets", err)
	}
	g.bootSecrets = bootSecrets
	pendingPaths := bootSecrets.Pending

	config.RunSecretCleanup(genCtx, bootSecrets.Store, s.resolution.Path, s.rootLogger)
	logPendingBootEffects(logger, bootSecrets.Report, cfg.Channels.ZMQCLI.Enabled, s.resolution.AgentDir)
	logBootVisibility(logger, s.resolution, cfg, profileName, activeProfile)
	logSecretPermissionWarnings(logger, cfg)

	if err = skillssync.Run(genCtx, cfg.Skills, cfg.RuntimeHome, s.rootLogger); err != nil {
		return g, fmt.Errorf("failed to sync the skill library: %w", err)
	}

	skillsMgr, skillCache, err := initInfra(genCtx, cfg, s.rootLogger)
	if err != nil {
		return g, generationStageError("initialize runtime infrastructure", err)
	}
	g.skillsMgr, g.skillCache = skillsMgr, skillCache

	// guided-init B.2: every pending-affected subsystem presents as off for
	// this generation (degraded view; the real config still drives the LLM
	// registry whose pending providers fail per candidate).
	bootCfg := degradedBootView(cfg, pendingPaths)
	g.bootCfg = bootCfg

	haToken, err := resolveTwinToken(genCtx, cfg, bootCfg)
	if err != nil {
		return g, generationStageError("resolve Home Assistant token", err)
	}

	haTwin, twinErr := twin.Init(genCtx, bootCfg.HomeAssistant, haToken, s.rootLogger)
	if twinErr != nil {
		return g, fmt.Errorf("failed to start HA digital twin: %w", twinErr)
	}
	g.haTwin = haTwin

	agentTools, mcpRuntime, blockedTools, cfgMgr := buildTools(genCtx, bootCfg, skillsMgr, skillCache, s.opts.MockMCP, profileName, haTwin, s.rootLogger,
		config.ProfileOverrideInputs{Flag: s.opts.Profile, Env: os.Getenv(config.ProfileEnvName)}, s.subagents, s.okfWriter, s.configState)
	g.mcpRuntime, g.cfgMgr = mcpRuntime, cfgMgr

	discloseDenied := activeProfile.DiscloseDeniedTools
	agentTools = withPolicyBlockTool(agentTools, discloseDenied, logger, profileName, len(blockedTools))
	g.agentTools = agentTools

	resolver := buildToolNameResolver(genCtx, agentTools, s.rootLogger, blockedTools, discloseDenied)
	g.llmResolver, g.llmDisclosure, g.llmProfile = resolver, discloseDenied, profileName

	// Definitions revalidated at reload only (subagent-runtime task 1.4):
	// a file that fails validation keeps the previous version with a WARN;
	// boot is the only fatal path. No fsnotify watcher.
	if s.subagents != nil {
		if defs, derr := subagent.LoadDefinitions(bootCfg.Subagents.Path); derr != nil {
			logger.Warn().Err(derr).Msg("subagent definition revalidation failed at reload — the previous definitions stay active")
		} else {
			s.subagents.SetDefinitions(subagent.NewDefinitions(defs))
		}
	}

	llmCands, fatalLLMErr := buildLLMCandidates(genCtx, cfg, agentTools, s.rootLogger, resolver, profileName, discloseDenied, pendingPaths)
	if fatalLLMErr != nil {
		return g, fmt.Errorf("invalid LLM registry: %w", fatalLLMErr)
	}

	// Per-provider parallelism (subagent-runtime D18): one limiter per
	// generation, keyed by provider instance name (per-generation because
	// the candidate chain — and its orchestrator — are generation-scoped).
	slotLimits := make(map[string]int, len(cfg.LLM.Providers))
	for name, p := range cfg.LLM.Providers {
		if p.MaxParallel > 0 {
			slotLimits[name] = p.MaxParallel
		}
	}

	g.llmCands = llmCands

	orch := orchestrator.New(orchestrator.Config{
		Candidates:      llmCands,
		Registry:        &cfg.LLM,
		Recovery:        cfg.LLM.ResolvedRecovery(),
		AttemptTimeout:  cfg.LLM.ResolvedFailoverAttemptTimeout(),
		Retries:         cfg.LLM.ResolvedFailoverRetries(),
		RetryBackoff:    cfg.LLM.ResolvedFailoverRetryBackoff(),
		RetryBackoffCap: 30 * time.Second,
		Slots:           orchestrator.NewSlotLimiter(slotLimits),
	}, s.rootLogger)
	orch.Boot(genCtx)
	orch.Start(genCtx)
	g.orch = orch

	llmSnap := orch.Snapshot()
	if llmSnap.State == orchestrator.StateUnavailable {
		logger.Warn().
			Strs("candidates", candidateRefs(llmCands)).
			Msg("no LLM candidate ready: daemon runs unavailable; recovery probing begins with agent traffic — check llm provider endpoints/credentials")
	} else {
		logger.Info().Str("active_model", llmSnap.ActiveRef).Str("api", llmSnap.ActiveAPI).Msg("LLM model resolved")

		if llmSnap.ActiveRef != cfg.LLM.Default {
			logger.Warn().
				Str("default_model", cfg.LLM.Default).
				Str("active_model", llmSnap.ActiveRef).
				Msg("LLM failover activated a different model than the default; session history is retained but may reference a different provider/protocol")
		}
	}

	g.agentReg = &agentRegistry{exec: orch, orch: orch, layout: orch.ActiveLayout()}
	g.aux = orch.AuxModel()
	g.fmtr = formatter.New(g.aux, skillsMgr, skillCache)
	g.fmtRegistry = buildFormatterRegistry(bootCfg)
	g.bootstrapCfg = buildBootstrapCfg(bootCfg, activeProfile.InjectSkills)
	g.bootstrapper = bootstrap.New(skillsMgr, skillCache, g.bootstrapCfg, s.rootLogger)

	// Hot-reload callback: evict file cache and invalidate bootstrap static
	// shell so the next turn rebuilds from updated files.
	bs := g.bootstrapper
	skillsMgr.OnReload(func(_ context.Context, skillName string) {
		if path, pathErr := skillsMgr.SkillFilePath(skillName); pathErr == nil {
			skillCache.Evict(path)
		}

		bs.Invalidate(skillName)
	})

	startTopologySnapshotter(genCtx, haTwin, skillsMgr, g.bootstrapper, s.rootLogger)

	if cr, ok := mcpRuntime.(*homemcp.CompositeRuntime); ok {
		cr.StartMonitors(genCtx)
		logger.Info().Msg("MCP health monitors started")
	}

	return g, nil
}

// applyGeneration installs g into the session runtime. The caller holds
// rt.genMu for writing (or owns rt exclusively at boot).
func (rt *sessionRuntime) applyGeneration(g *runtimeGeneration) {
	rt.agentReg = g.agentReg
	rt.activeProfileName = g.activeProfileName
	rt.fmtr = g.fmtr
	rt.reg = g.fmtRegistry
	rt.bootstrapper = g.bootstrapper
	rt.bootstrapCfg = g.bootstrapCfg
	rt.skillsMgr = g.skillsMgr
	rt.skillCache = g.skillCache
	rt.agentTools = g.agentTools
	rt.cfgMgr = g.cfgMgr
	rt.configManagerAllowed = isConfigManagerAllowed(g.bootCfg, g.activeProfileName)
	if g.agentReg != nil {
		rt.liveOrch.Store(g.agentReg.orch)
	}
	if g.bootCfg != nil {
		configureSessionRuntime(g.bootCfg, rt, g.aux)
	}
	// New agentskills manager, new tool catalog: every existing Session
	// re-creates its skills session and re-pins compaction skills next Turn.
	rt.resetForReload()
}

// buildZMQChannel creates the zmqcli listener channel for cfg.
func buildZMQChannel(ctx context.Context, cfg *config.Config, rootLogger zerolog.Logger) (*zmqcli.Channel, error) {
	logger := rootLogger.With().Str("component", "channel-manager").Logger()

	secret, ok, err := cfg.ResolveSecret(ctx, cfg.Channels.ZMQCLI.AuthToken, secretstore.FieldZMQAuthToken)
	if err != nil || !ok {
		return nil, fmt.Errorf("failed to resolve the zmqcli shared secret: %w", err)
	}

	zmqCfg := cfg.Channels.ZMQCLI
	zmqCfg.ReplyTimeout = clampReplyTimeout(zmqCfg.ReplyTimeout, logger)

	zch, err := zmqcli.NewChannel(ctx, zmqCfg, secret, cfg.Logging.ZMQPayloadsEnabled, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create zmqcli channel: %w", err)
	}

	logger.Info().
		Str("bind", cfg.Channels.ZMQCLI.Bind).
		Dur("connection_idle_timeout", cfg.Channels.ZMQCLI.ConnectionIdleTimeout).
		Msg("zmqcli channel enabled")

	return zch, nil
}

// clampReplyTimeout raises reply_timeout to the agent run budget: the
// watchdog must never fire before the agent's own deadline.
func clampReplyTimeout(configured time.Duration, logger zerolog.Logger) time.Duration {
	minReplyTimeout := agentRunTimeout + formatterTimeout + 30*time.Second
	if configured < minReplyTimeout {
		logger.Warn().
			Dur("configured_reply_timeout", configured).
			Dur("min_reply_timeout", minReplyTimeout).
			Msg("channels.zmqcli.reply_timeout is too low for the agent run budget — raising it")

		return minReplyTimeout
	}

	return configured
}

// buildTelegramChannel creates the Telegram channel for cfg.
func buildTelegramChannel(ctx context.Context, cfg *config.Config, rootLogger zerolog.Logger) (*telegram.Channel, error) {
	logger := rootLogger.With().Str("component", "channel-manager").Logger()

	tgToken, tgOK, tgErr := cfg.ResolveSecret(ctx, cfg.Channels.Telegram.Token, "channels.telegram.token")
	if tgErr != nil || !tgOK {
		return nil, fmt.Errorf("failed to resolve the Telegram token: %w", tgErr)
	}
	tch, err := telegram.New(cfg.Channels.Telegram, tgToken, rootLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram channel: %w", err)
	}

	logger.Info().
		Int("whitelist_entries", len(cfg.Channels.Telegram.AllowFrom)).
		Msg("Telegram channel enabled")

	return tch, nil
}

// wireZMQ (re)installs every ZMQ hook of generation g on the live listener.
func (s *serveShell) wireZMQ(g *runtimeGeneration) {
	zmqCh := getZMQChannel(s.mgr, zmqcli.ChannelName)
	if zmqCh == nil {
		return
	}

	wireZMQBootHooks(s.mgr, g.orch, s.resolution.AgentDir)
	wireZMQSessions(s.ctx, zmqCh, s.rt, g.cfg, g.bootSecrets, g.activeProfileName, s.reloadFn)
}

// swapChannels brings the generation-scoped channels (Telegram, legacy stdin
// cli) in line with g, and the zmqcli listener when its enabled/bind state
// changed. An unchanged listener keeps its Connection: only its timeouts are
// updated in place. A changed listener is rebuilt shortly AFTER the reload
// reply has been delivered over the old one.
func (s *serveShell) swapChannels(g *runtimeGeneration) {
	// A reload landing during the daemon shutdown (SIGINT racing the
	// config-save reload) must never (re)start channels: the manager is
	// stopping, and a channel built here would keep its own poller alive
	// behind the teardown path.
	if s.ctx.Err() != nil {
		return
	}

	cfg := g.bootCfg

	if s.mgr.Channel(telegram.ChannelName) != nil {
		if err := s.mgr.StopChannel(telegram.ChannelName); err != nil {
			s.logger.Warn().Err(err).Msg("failed to stop the previous Telegram channel")
		}
	}
	if cfg.Channels.Telegram.Enabled {
		if tch, err := buildTelegramChannel(g.ctx, cfg, s.rootLogger); err != nil {
			s.logger.Warn().Err(err).Msg("Telegram channel not started after runtime reload")
		} else {
			s.mgr.AddOrReplace(tch)
		}
	}

	existing := s.mgr.Channel(zmqcli.ChannelName)
	liveZMQ, isZMQ := existing.(*zmqcli.Channel)
	_, isCLI := existing.(*cli.Channel)

	switch {
	case cfg.Channels.ZMQCLI.Enabled && isZMQ && s.currentBind() == cfg.Channels.ZMQCLI.Bind:
		zc := cfg.Channels.ZMQCLI
		liveZMQ.UpdateTimeouts(zc.ConnectionIdleTimeout, clampReplyTimeout(zc.ReplyTimeout, s.logger), zc.LivenessTimeout)
	case cfg.Channels.CLI.Enabled && isCLI:
		// The stdin reader stays as it is.
	case existing == nil && !cfg.Channels.ZMQCLI.Enabled && !cfg.Channels.CLI.Enabled:
	case isZMQ:
		s.logger.Info().Msg("zmqcli enabled/bind changed: the listener is rebuilt after this reload's reply")
		time.AfterFunc(time.Second, func() { s.rebuildCLISlot() })
	default:
		s.rebuildCLISlot()
	}
}

func (s *serveShell) currentBind() string {
	v, _ := s.zmqBind.Load().(string)

	return v
}

// rebuildCLISlot replaces whatever owns the shared "cli" channel name with
// what the CURRENT generation's configuration asks for.
func (s *serveShell) rebuildCLISlot() {
	// Deferred rebuild (time.AfterFunc) firing after the shutdown began:
	// never rebuild the listener behind the teardown path.
	if s.ctx.Err() != nil {
		return
	}

	g := s.coord.currentGen()
	if g == nil {
		return
	}
	cfg := g.bootCfg

	if s.mgr.Channel(zmqcli.ChannelName) != nil {
		if err := s.mgr.StopChannel(zmqcli.ChannelName); err != nil {
			s.logger.Warn().Err(err).Msg("failed to stop the previous cli channel")
		}
	}
	s.zmqBind.Store("")

	switch {
	case cfg.Channels.ZMQCLI.Enabled:
		zch, err := buildZMQChannel(s.ctx, cfg, s.rootLogger)
		if err != nil {
			s.logger.Error().Err(err).Msg("zmqcli listener not rebuilt after runtime reload")

			return
		}
		s.mgr.AddOrReplace(zch)
		s.zmqBind.Store(cfg.Channels.ZMQCLI.Bind)
		s.wireZMQ(g)
	case cfg.Channels.CLI.Enabled:
		s.mgr.AddOrReplace(cli.New())
	}
}

// reloadResponse renders the runtime_reload / config_reloaded payload from g.
func (s *serveShell) reloadResponse(ctx context.Context, g *runtimeGeneration) protocol.Response {
	ev := modelStatusEventFor(g.orch.Snapshot())
	resp := protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		ModelRef:     ev.Ref,
		ModelState:   ev.State,
		AgentID:      g.cfg.Agent.ID,
		AgentName:    g.cfg.AgentDisplayName(),
		Credentials:  protocolCredentials(deriveOAuthCredentials(ctx, g.cfg, g.bootSecrets.Store)),
	}
	if rep, err := newSecretEntry(g.cfg, g.bootSecrets, nil, nil).Report(ctx); err == nil {
		resp.Secrets = protocolSecrets(rep)
	}

	return resp
}

// childRunnerSet builds (or returns the memoised) per-candidate runner set
// for a child definition snapshot (subagent-runtime task 3.2, D17): one
// runner per chain candidate built for the child's catalog and iteration
// cap, keyed by candidate reference. Memoised inside the generation (a
// snapshot is stable: prompt body, tools, caps) and retired with it. A pin
// outside the chain is the fixed structural error.
func (g *runtimeGeneration) childRunnerSet(
	ctx context.Context,
	def subagent.Definition,
	pinnedModel string,
	tools []tool.BaseTool,
	rootLogger zerolog.Logger,
) (map[string]agent.Runner, error) {
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		if info, err := t.Info(ctx); err == nil && info != nil {
			toolNames = append(toolNames, info.Name)
		}
	}

	key := def.Name + "|" + pinnedModel + "|" + strings.Join(toolNames, ",") + "|" + fmt.Sprint(def.EffectiveMaxIterations())

	g.childRunnerMu.Lock()
	if set, ok := g.childRunners[key]; ok {
		g.childRunnerMu.Unlock()

		return set, nil
	}
	g.childRunnerMu.Unlock()

	if pinnedModel != "" && !chainHasRef(g.llmCands, pinnedModel) {
		return nil, fmt.Errorf("%w: %q", subagent.ErrModelPinOutsideChain, pinnedModel)
	}

	logger := rootLogger.With().Str("component", "subagent-runner-set").Str("type", def.Name).Logger()
	// Child runner sets share no thought tap with owner turns (a single tap
	// would interleave; children log nothing here).
	tap := agent.NewTap(rootLogger, false)

	set := make(map[string]agent.Runner, len(g.llmCands))

	for _, cand := range g.llmCands {
		if cand.Runner == nil {
			continue // a candidate whose construction failed stays skipped
		}

		if pinnedModel != "" && cand.Ref != pinnedModel {
			continue
		}

		runner, err := g.buildChildRunner(ctx, cand, def, tools, tap, logger)
		if err != nil {
			logger.Warn().Err(err).Str("model", cand.Ref).Msg("child runner construction failed; candidate skipped for this definition")

			continue
		}

		set[cand.Ref] = runner
	}

	if len(set) == 0 {
		return nil, fmt.Errorf("failed to build the child runner set for %q: no candidate constructed", def.Name)
	}

	g.childRunnerMu.Lock()
	if g.childRunners == nil {
		g.childRunners = map[string]map[string]agent.Runner{}
	}
	g.childRunners[key] = set
	g.childRunnerMu.Unlock()

	return set, nil
}

// chainHasRef reports whether the chain carries ref.
func chainHasRef(cands []orchestrator.Candidate, ref string) bool {
	for _, c := range cands {
		if c.Ref == ref {
			return true
		}
	}

	return false
}

// buildChildRunner constructs one child runner on the candidate's model,
// both construction paths, with the child's tools and iteration cap.
func (g *runtimeGeneration) buildChildRunner(
	ctx context.Context,
	cand orchestrator.Candidate,
	def subagent.Definition,
	tools []tool.BaseTool,
	tap *agent.Tap,
	logger zerolog.Logger,
) (agent.Runner, error) {
	maxIter := def.EffectiveMaxIterations()

	if cand.Model.IsAgentic() {
		recorded := usage.NewRecordedAgenticModel(cand.Model.Agentic)

		// The blocked-call disclosure rewrites use the OWNER catalog's ghost
		// index; a child catalog is a strict subset, so the resolver stays
		// the generation's.
		var runnerModel model.AgenticModel = recorded
		if g.llmDisclosure {
			runnerModel = toolnames.NewDisclosingAgenticModel(recorded, g.llmResolver, logger, g.llmProfile)
		}

		return agent.NewAgentic(ctx, agent.AgenticConfig{
			Model:         runnerModel,
			Tools:         tools,
			SystemPrompt:  "", // the child prompt lives in the dispatch prefix
			MaxIterations: maxIter,
		}, tap)
	}

	recorded := usage.NewRecordedChatModel(cand.Model.Chat)

	var chatModel *toolnames.NormalizingChatModel
	if g.llmDisclosure {
		chatModel = toolnames.NewDisclosingChatModel(recorded, g.llmResolver, logger, g.llmProfile)
	} else {
		chatModel = toolnames.NewNormalizingChatModel(recorded, g.llmResolver, logger)
	}

	return agent.New(ctx, agent.Config{
		ChatModel:     chatModel,
		Tools:         tools,
		SystemPrompt:  "",
		MaxIterations: maxIter,
	}, tap)
}

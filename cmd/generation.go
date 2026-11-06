package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/formatter"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/skillssync"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/marco-rozz/ready/internal/twin"
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
	// config-manager staging carries secret material: released with its generation.
	if g.cfgMgr != nil {
		g.cfgMgr.Close()
	}
	if g.orch != nil {
		g.orch.Wait()
	}
}

// serveShell is the Process Shell: it lives for the whole OS process and owns
// every resource a Runtime reload must not drop.
type serveShell struct {
	ctx        context.Context
	opts       ServeOptions
	resolution config.Resolution
	rootLogger zerolog.Logger
	logger     zerolog.Logger

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
		config.ProfileOverrideInputs{Flag: s.opts.Profile, Env: os.Getenv(config.ProfileEnvName)})
	g.mcpRuntime, g.cfgMgr = mcpRuntime, cfgMgr

	discloseDenied := activeProfile.DiscloseDeniedTools
	agentTools = withPolicyBlockTool(agentTools, discloseDenied, logger, profileName, len(blockedTools))
	g.agentTools = agentTools

	resolver := buildToolNameResolver(genCtx, agentTools, s.rootLogger, blockedTools, discloseDenied)

	llmCands, fatalLLMErr := buildLLMCandidates(genCtx, cfg, agentTools, s.rootLogger, resolver, profileName, discloseDenied, pendingPaths)
	if fatalLLMErr != nil {
		return g, fmt.Errorf("invalid LLM registry: %w", fatalLLMErr)
	}

	orch := orchestrator.New(orchestrator.Config{
		Candidates:      llmCands,
		Registry:        &cfg.LLM,
		Recovery:        cfg.LLM.ResolvedRecovery(),
		AttemptTimeout:  cfg.LLM.ResolvedFailoverAttemptTimeout(),
		Retries:         cfg.LLM.ResolvedFailoverRetries(),
		RetryBackoff:    cfg.LLM.ResolvedFailoverRetryBackoff(),
		RetryBackoffCap: 30 * time.Second,
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
	rt.fmtr = g.fmtr
	rt.reg = g.fmtRegistry
	rt.bootstrapper = g.bootstrapper
	rt.bootstrapCfg = g.bootstrapCfg
	rt.skillsMgr = g.skillsMgr
	rt.skillCache = g.skillCache
	rt.agentTools = g.agentTools
	rt.cfgMgr = g.cfgMgr
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

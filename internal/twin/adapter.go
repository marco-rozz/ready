// Package twin is the glue layer between READY's internal config/wiring and the
// detachable pkg/hatwin library. It owns the config→options adapter, the
// non-blocking boot-time initialization, and (in later phases) the asset
// snapshot generation and Eino query tool.
//
// Boundary: pkg/hatwin has no imports of this project's internal/ packages and
// no knowledge of Eino. All such coupling lives here.
package twin

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/profile"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// defaultTwinProfiles is used when cfg.Profiles is absent entirely (nil map),
// preserving the Twin's tool surface for operators upgrading from a config
// that predates per-owner gating. BOTH wire names are exposed (D10).
// An operator who explicitly declares home_assistant.profiles.<name> without
// the tools they want gets the normal fail-closed WARN instead — this default
// only applies when the whole block is missing.
func defaultTwinProfiles() map[string]config.OwnerProfileConfig {
	return map[string]config.OwnerProfileConfig{
		"default": {Allow: []string{queryToolName, commandToolName}},
		"edit":    {Allow: []string{queryToolName, commandToolName}},
	}
}

// BuildTools returns the Twin's own tools (ha_state_query and ha_command),
// gated by home_assistant.profiles[activeProfileName]. It returns nil when
// the Twin is disabled (cfg.Enabled false or t nil) — the tools disappear
// from the system prompt entirely rather than registering stubs.
func BuildTools(
	ctx context.Context,
	cfg config.HomeAssistantConfig,
	t *hatwin.Twin,
	activeProfileName string,
	decisionLog bool,
	logger zerolog.Logger,
) ([]tool.BaseTool, []profile.BlockedTool) {
	if !cfg.Enabled || t == nil {
		return nil, nil
	}

	profiles := cfg.Profiles
	if profiles == nil {
		profiles = defaultTwinProfiles()
	}

	tools := make([]tool.BaseTool, 0, 2)
	tools = append(tools, NewQueryTool(t, cfg.StaleThresholdStates, cfg.HardStaleThreshold))

	// Write path (twin-native-actuation D5/D12): validate the configured
	// domains against the vocabulary (unknown ⇒ WARN, ignored), map the
	// config onto the library options, build the tool.
	domains, unknownDomains := validateCommandDomains(cfg.Commands.Domains)
	if len(unknownDomains) > 0 {
		logger.Warn().
			Strs("unknown_domains", unknownDomains).
			Strs("known_domains", hatwin.KnownCommandDomains()).
			Msg("home_assistant.commands.domains lists unknown domains; ignoring them (lock and alarm_control_panel are never available)")
	}
	t.SetActiveCommandDomains(domains)
	if cfg.Commands.VerifyTimeout > 0 {
		if cfg.Commands.VerifyTimeout < time.Second {
			cfg.Commands.VerifyTimeout = time.Second
		} else if cfg.Commands.VerifyTimeout > 30*time.Second {
			cfg.Commands.VerifyTimeout = 30 * time.Second
		}
	}
	verifyTimeout := cfg.Commands.VerifyTimeout
	if verifyTimeout <= 0 {
		verifyTimeout = 5 * time.Second
	}
	maxTargets := cfg.Commands.MaxTargets
	if maxTargets <= 0 {
		maxTargets = 20
	}
	tools = append(tools, NewCommandTool(t, commandToolConfig{
		VerifyTimeout: verifyTimeout,
		MaxTargets:    maxTargets,
	}, logger))

	return profile.GateOwner(ctx, "home_assistant (twin)", tools, profiles, activeProfileName, decisionLog, logger)
}

// validateCommandDomains normalizes the operator's domain list against the
// vocabulary table (D5): nil ⇒ every table domain except siren; an explicit
// list is taken literally; unknown entries are reported back for the boot
// WARN and dropped.
func validateCommandDomains(domains []string) ([]string, []string) {
	if domains == nil {
		return hatwin.DefaultCommandDomains(), nil
	}

	known := make(map[string]bool)
	for _, d := range hatwin.KnownCommandDomains() {
		known[d] = true
	}

	var (
		active  []string
		unknown []string
	)
	for _, d := range domains {
		if known[d] {
			active = append(active, d)

			continue
		}
		unknown = append(unknown, d)
	}

	return active, unknown
}

// This is the single translation boundary between the YAML config model and
// the library's options model; neither side should know about the other's types.
func OptionsFromConfig(cfg config.HomeAssistantConfig, logger zerolog.Logger) hatwin.Options {
	mon := cfg.Monitoring

	return hatwin.Options{
		URL:                       cfg.WebSocket.URL,
		Token:                     cfg.WebSocket.Token,
		TokenFile:                 cfg.WebSocket.TokenFile,
		PingInterval:              mon.PingInterval,
		TopologyDebounce:          mon.TopologyDebounce,
		InitialBackoff:            mon.InitialBackoff,
		MaxBackoff:                mon.MaxBackoff,
		BackoffFactor:             mon.BackoffFactor,
		MaxRetries:                mon.MaxRetries,
		OutstandingPingsThreshold: cfg.OutstandingPingsThreshold,
		StaleThresholdStates:      cfg.StaleThresholdStates,
		HardStaleThreshold:        cfg.HardStaleThreshold,
		Logger:                    logger,
	}
}

// Init creates and starts the digital twin from config, then waits up to
// cfg.Monitoring.SyncTimeout for the initial sync. On timeout or connection
// failure the twin is returned in a degraded-but-running state (reads return
// ErrNotReady until HA becomes reachable). Returns nil when
// cfg.Enabled is false.
//
// Init returns (nil, nil) when the twin is disabled. A construction failure is
// returned to the caller — criticalities-remediation (LOW): no log.Fatal
// inside a library constructor; the bootstrapper retains control over failure
// handling and cleanup ordering.
func Init(ctx context.Context, cfg config.HomeAssistantConfig, logger zerolog.Logger) (*hatwin.Twin, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	log := logger.With().Str("component", "hatwin").Logger()

	opts := OptionsFromConfig(cfg, log)
	twin, err := hatwin.New(opts)
	if err != nil {
		log.Error().Err(err).Msg("failed to create HA digital twin")

		return nil, fmt.Errorf("failed to create HA digital twin: %w", err)
	}

	twin.Start(ctx)

	syncTimeout := cfg.Monitoring.SyncTimeout
	if syncTimeout <= 0 {
		syncTimeout = 15 * time.Second
	}

	waitCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	if err := twin.WaitReady(waitCtx); err != nil {
		log.Warn().
			Dur("sync_timeout", syncTimeout).
			Msg("HA digital twin not ready at boot, continuing degraded (twin recovers on reconnect)")
	} else {
		log.Info().
			Int("states", twin.StateCount()).
			Msg("HA digital twin ready")
	}

	return twin, nil
}

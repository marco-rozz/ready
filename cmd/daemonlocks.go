package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/twin"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// acquireDirectoryLocks performs the per-user-install-agent-isolation boot
// lock sequence for one agent directory: the shared maintenance gate first,
// then exclusive daemon ownership — both before config migration, configured
// logging, sync, retention, network setup or workers. A duplicate start
// therefore has no runtime side effects and an installer purge can exclude
// this daemon for its whole operation. The returned release func drops the
// ownership lock, then the gate; errors join the serve error.
func acquireDirectoryLocks(
	ctx context.Context,
	agentDir string,
) (release func(serveErr *error), releaseErr error) {
	maintenance, err := daemonlock.AcquireMaintenanceShared(ctx, agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire the maintenance gate on %s: %w", agentDir, err)
	}

	ownership, err := daemonlock.AcquireOwnership(ctx, agentDir)
	if err != nil {
		_ = maintenance.Release()

		return nil, fmt.Errorf("failed to acquire ownership of %s: %w", agentDir, err)
	}

	return func(serveErr *error) {
		// Released in reverse acquisition order: ownership (the OS lock —
		// not the stable file — is the ownership proof) after the runtime
		// teardown, then the gate. A close error cannot block shutdown.
		if relErr := ownership.Release(); relErr != nil {
			*serveErr = errors.Join(*serveErr, fmt.Errorf("release ownership: %w", relErr))
		}
		if relErr := maintenance.Release(); relErr != nil {
			*serveErr = errors.Join(*serveErr, fmt.Errorf("release maintenance gate: %w", relErr))
		}
	}, nil
}

// ensureConfigExists fails a run whose sole config file is missing or is a
// directory — after selection (fresh machine) and again under ownership
// (a purge that raced between selection and acquisition).
func ensureConfigExists(configPath string, underOwnership bool) error {
	info, err := os.Stat(configPath)
	if err == nil && !info.IsDir() {
		return nil
	}
	if underOwnership {
		return fmt.Errorf("config %s is missing after acquiring ownership (purge conflict?)", configPath)
	}
	if err == nil {
		return fmt.Errorf("config %s is not a regular file", configPath)
	}

	return fmt.Errorf("agent directory %s does not exist (it must hold %s)", filepath.Dir(configPath), config.ConfigFileName)
}

// startTopologySnapshotter starts the HA-twin topology snapshot generator for
// the home-topology skill. It waits for the twin to be ready internally and
// regenerates on every topology-change signal. A missing skill dir disables
// the snapshots (WARN), never the boot.
func startTopologySnapshotter(
	ctx context.Context,
	haTwin *hatwin.Twin,
	skillsMgr *skills.Manager,
	bootstrapper *bootstrap.Bootstrapper,
	rootLogger zerolog.Logger,
) {
	logger := rootLogger.With().Str("component", "topology-snapshotter").Logger()
	if haTwin == nil {
		return
	}
	skillDir, err := skillsMgr.GetSkillLocation("home-topology")
	if err != nil {
		logger.Warn().Err(err).Msg("home-topology skill not found — digital twin snapshots disabled")

		return
	}
	snapshotter := twin.NewSnapshotter(haTwin, skillDir, bootstrapper, rootLogger)
	snapshotter.Start(ctx)
}

// wireZMQBootHooks wires the ZMQ channel's boot-time hooks: the orchestrator
// model-status source (llm-status-visibility) and the authenticated
// handshake's agent_dir display (per-user-install-agent-isolation),
// home-prefix sanitised daemon-side (a leading ~ is the daemon user's home).
func wireZMQBootHooks(mgr *channel.Manager, orch *orchestrator.Orchestrator, agentDir string) {
	zmqCh := getZMQChannel(mgr, zmqcli.ChannelName)
	if zmqCh == nil {
		return
	}
	zmqCh.WatchModelStatus(orchestratorStatusSource{orch: orch})
	zmqCh.SetAgentDirDisplay(privacy.FromHome(nil).Redact(agentDir))
}

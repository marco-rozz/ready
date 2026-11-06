package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/urfave/cli/v3"
)

// newMaintenanceCommand declares the HIDDEN `ready maintenance` subcommand:
// the cross-platform installer↔daemon purge coordination point (task 3.4).
// It is not part of the operator-facing three-subcommand surface and is not
// documented in help output; only the installers invoke it.
//
// Contract: it acquires the EXCLUSIVE maintenance gate on the agent
// directory — refusing with a busy error when a daemon or mutating login
// holds the shared gate — and then blocks, holding the OS lock, until it is
// terminated (SIGINT/SIGTERM) or its stdin reaches EOF. The installers use
// it as a lock holder around destructive removal:
//
//	"$BIN" maintenance --agent-dir ~/.ready > "$marker" 2>&1 &
//	# wait until the marker prints the lock marker, perform deletions
//	# (config first), then kill the holder — or let it see EOF.
//
// The OS lock releases on termination by itself; no state is ever written
// besides the stable gate file (never unlinked by an ordinary release).
const maintenanceLockedMarker = "MAINTENANCE-LOCKED"

func newMaintenanceCommand() *cli.Command {
	return &cli.Command{
		Name:   "maintenance",
		Usage:  "hold the exclusive maintenance gate (installer purge coordination)",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "agent-dir",
				Usage:    "agent directory whose gate to hold",
				Required: true,
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			return runMaintenanceHolder(ctx, c.String(flagAgentDirName), os.Stdin, os.Stdout)
		},
	}
}

// runMaintenanceHolder is the testable body: acquire exclusive, announce,
// block until done or signal, release, exit.
func runMaintenanceHolder(ctx context.Context, agentDir string, stdin io.Reader, stdout io.Writer) error {
	if agentDir == "" {
		return fmt.Errorf("maintenance requires --agent-dir")
	}
	if info, err := os.Stat(agentDir); err != nil || !info.IsDir() {
		return fmt.Errorf("agent directory %s does not exist", agentDir)
	}

	gate, err := daemonlock.AcquireMaintenanceExclusive(ctx, agentDir)
	if err != nil {
		// A busy error names the conflict so an installer purge aborts
		// before deleting anything.
		return fmt.Errorf("cannot take the exclusive maintenance gate on %s: %w", agentDir, err)
	}

	// Announce so the installer can wait for readiness; flush explicitly.
	_, _ = fmt.Fprintf(stdout, "%s %s\n", maintenanceLockedMarker, gate.Path())
	if f, ok := stdout.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}

	// Block until: outer context cancellation, a signal, or stdin EOF.
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signalCh)

	done := make(chan struct{}, 1)
	go func() {
		// Drain stdin until EOF/error; a writer is allowed to speak. The
		// error is not actionable — EOF or a closed pipe both mean "stop".
		_, _ = io.Copy(io.Discard, stdin)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}

	if err := gate.Release(); err != nil {
		return fmt.Errorf("release the maintenance gate: %w", err)
	}

	return nil
}

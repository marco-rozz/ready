package main

// The `ready init` subcommand of guided-init-and-direct-secret-entry: a
// thin dispatcher over internal/initwizard. Only init may create a missing
// agent directory; the sole config is config.yaml inside it and the removed
// --config refuses here exactly like serve/login. init never starts a chat
// TUI, an agent runtime or the configured logging subsystem.
import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/initwizard"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// initLabel is the subcommand name used by the dispatcher tests.
const initLabel = "init"

// ExitInterrupted is the termination exit code of the POSIX convention
// process; wizard interruptions surface as 130 (128 + SIGINT(2)).
const ExitInterrupted = 130

// newInitCommand declares `ready init [--agent-dir DIR]`.
func newInitCommand(stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  initLabel,
		Usage: "Bootstrap a READY. agent directory with a guided, deterministic wizard (no model involved)",
		Description: "Guided init asks exactly one provider, its configurable endpoint, the opencode wire API " +
			"(where applicable) and one model, stores the credentials in the agent's encrypted secret store (the chat " +
			"secret is generated, the API key where required is asked hidden), creates the agent identity and saves starter defaults for JSON logging, skills and conversations. With an existing config it offers a " +
			"bounded offline repair (default provider replacement, key fix or chat secret rotation) while the " +
			"physical agent is inactive.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				// Optional: the default agent directory ~/.ready is
				// selected exactly like serve/login (cwd-independent) —
				// and only init may create a missing one.
				Name:  flagAgentDirName,
				Usage: "agent directory to bootstrap (default: ~/.ready)",
			},
			&cli.BoolFlag{
				Name:  "new-identity",
				Usage: "give an existing agent a fresh, independent identity (use it on a COPY of an agent directory)",
			},
		},
		// --config is deliberately not defined anywhere: urfave/cli rejects
		// unknown flags with its own loud error, matching the removed-flag
		// contract of the agent-isolation change.
		Action: func(ctx context.Context, c *cli.Command) error {
			return runInit(ctx, c, stderr)
		},
	}
}

// runInit executes one init invocation and owns its exit-code semantics:
// 0 = a valid config was committed or the repair was explicitly declined;
// 1 = invalid input, non-terminal, persistence or refusal error; 130 =
// interruption. A committed-but-late failure still states the committed
// fact (never implied rollback).
func runInit(ctx context.Context, c *cli.Command, stderr io.Writer) error {
	out := os.Stdout
	stdinTTY := term.IsTerminal(int(os.Stdin.Fd()))
	workingDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("locate working directory for setup commands: %w", err)
	}
	workingDir, err = filepath.EvalSymlinks(workingDir)
	if err != nil {
		return fmt.Errorf("resolve working directory for setup commands: %w", err)
	}

	opts := initwizard.Options{
		ExplicitAgentDir: c.String(flagAgentDirName),
		WorkingDir:       workingDir,
		Stdin:            os.Stdin,
		Stdout:           out,
		Stderr:           stderr,
		StdinTTY:         stdinTTY,
		RunCodexLogin:    runInitCodexLogin,
		NewIdentity:      c.Bool("new-identity"),
	}

	outcome, err := initwizard.Run(ctx, opts)

	// The wizard maps interruption/decline internally; cmd/init.go only
	// owns the exit codes.
	switch {
	case outcome.DeclinedRepair:
		// Repair declined: existing state unchanged, clean exit 0.
		_, _ = fmt.Fprintln(out, "Everything left unchanged.")

		return nil

	case outcome.Interrupted:
		if outcome.Committed {
			_, _ = fmt.Fprintln(out, "Setup interrupted after saving. Your files are kept; use this agent's server and chat commands shown above.")
		} else {
			_, _ = fmt.Fprintln(out, "Init interrupted before publication; no configuration was saved.")
		}

		return cli.Exit("", ExitInterrupted)

	case err != nil:
		exitErr := wrapExitOne(err)
		// The root dispatcher suppresses ExitCoder messages because
		// commands render their own diagnostics.
		_, _ = fmt.Fprintln(stderr, exitErr.Error())
		if !outcome.Committed {
			_, _ = fmt.Fprintln(stderr, "No new configuration was saved. Resolve the message above, then run the same init command again.")
		}

		return exitErr

	default:
		return nil
	}
}

// wrapExitOne renders a failing wizard error as one stderr line and
// returns cli.Exit for the generic error exit.
func wrapExitOne(err error) cli.ExitCoder {
	flat := clsFirstErrorLine(err)
	if strings.TrimSpace(flat) == "" {
		flat = "init failed"
	}

	return cli.Exit(flat, ExitErr)
}

// clsFirstErrorLine flattens a (possibly joined) error chain into one safe
// line; secrets never cross these messages by construction (every error in
// the wizard is metadata-only).

// clsFirstErrorLine flattens a (possibly joined) error chain into one
// safe operator-facing line; secrets never cross these messages by
// construction (every wizard error is metadata-only).
func clsFirstErrorLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

// runInitCodexLogin is the in-process Codex OAuth entry wired into the
// wizard's optional offer. It loads the just-committed config by its
// elected path, opens its Secret store and runs the same library function
// the login subcommand uses.
func runInitCodexLogin(ctx context.Context, agentDir string, deviceCode bool) error {
	cfg, err := electAndLoadConfig(ctx, agentDir, os.Stdout)
	if err != nil {
		return err
	}
	if _, _, err = openLoginSecrets(ctx, cfg, false); err != nil {
		return err
	}

	// The Codex provider is the sole LLM OAuth target produced by a
	// guided-init commit; name it from the config (the instance is the
	// map key).
	for name, provider := range cfg.LLM.Providers {
		if provider.Provider == config.ProviderOpenAICodex {
			return loginCodexProvider(ctx, cfg, name, deviceCode, os.Stdin, os.Stdout)
		}
	}

	return fmt.Errorf("the committed config names no openai-codex provider")
}

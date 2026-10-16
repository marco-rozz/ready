// Package main is the entry point for the ready binary.
//
// The binary exposes three subcommands and nothing else:
//
//   - `ready serve`  — run the long-lived daemon (formerly the bare
//     `READY` invocation; see Serve in serve.go for the body).
//   - `ready chat`   — connect a local CLI client to a running daemon
//     over the wire protocol defined in internal/protocol.
//   - `ready login`  — authenticate to an OAuth-protected MCP server; the
//     only place a browser is ever opened (see runLogin in login.go).
//
// Invoking the binary with no subcommand is a hard error — there is no
// implicit fallback to `serve`. This is deliberate (decision A9 in the CLI/ZMQ
// rollout handoff) so a misconfigured launcher fails loudly instead of
// silently starting the wrong process.
//
// The dispatcher itself is intentionally tiny: it builds a `*cli.Command`
// tree and runs it. All boot logic lives in serve.go; CLI client logic lives
// in chat.go; the login flow lives in login.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
)

// noSubcommandMessage is the exact stderr line emitted when the binary is
// invoked without a subcommand. The wording is part of the public CLI
// contract: tests pin to this string, and operators see it when they forget
// to update their systemd unit or launcher script.
const noSubcommandMessage = "missing subcommand: use 'serve', 'chat' or 'login'"

const homeAgent = "ready"

// Build information, injected at release time via goreleaser's ldflags
// (-X main.version / main.commit / main.date). A plain `go build` leaves
// the dev placeholders in place — `ready --version` still prints a
// non-empty version instead of failing.
//
//nolint:gochecknoglobals // build information: injected by -ldflags -X at release time
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString renders the canonical version line: "ready <version>
// (commit <sha>, built <date>)". It lives in one place so --version and
// the chat TUI info block cannot drift.
func versionString() string {
	return fmt.Sprintf("ready %s (commit %s, built %s)", version, commit, date)
}

// versionPrinter pins the --version behaviour: the root command's native
// Version field makes urfave/cli handle `ready --version` on its own path
// (stdout, exit 0, no runtime, no subcommand), and the printer below
// replaces the library's default rendering. No custom subcommand is
// registered — the dispatcher still exposes exactly serve/chat/login.
// The version printing is wired by installing the printer on urfave/cli's
// package-level VersionPrinter (v3 exposes no per-command printer field); a
// single init at package initialisation keeps run/newRootCommand side-effect
// free.
//
//nolint:gochecknoinits // cli.VersionPrinter is a package var: there is no per-command slot
func init() {
	cli.VersionPrinter = versionPrinter
}

// versionPrinter renders the canonical version line (see versionString) to
// the command's writer.
func versionPrinter(c *cli.Command) {
	_, _ = fmt.Fprintln(c.Root().Writer, versionString())
}

// errNoSubcommand is returned by the root Action when no subcommand was
// supplied. It is caught in main() and rendered without an extra prefix so
// the operator-facing message stays clean.
var errNoSubcommand = errors.New(noSubcommandMessage)

// newRootCommand builds the *cli.Command tree for the binary. The stderr
// writer is injected so tests can capture output without touching os.Stderr.
//
// Keep this function free of side effects (no os.Exit, no logger creation):
// it is exercised directly by dispatcher tests, which assert on the returned
// error and on the buffered stderr.
//
// urfave/cli's default ExitErrHandler writes the error to ErrWriter and then
// calls os.Exit on its own when an Action returns a cli.ExitCoder. We disable
// that here so the dispatcher owns both printing and exit-code mapping —
// otherwise every subcommand sees its error message printed twice and
// run()/main() never get a chance to clean up.
func newRootCommand(stderr io.Writer) *cli.Command {
	root := &cli.Command{
		Name:      homeAgent,
		Usage:     "Run the home-automation agent daemon or connect a CLI client to it",
		Version:   version,
		ErrWriter: stderr,
		// The root Action fires when no subcommand matched. If urfave parked
		// a positional arg on the root it means the operator typed an unknown
		// subcommand (urfave routes it here instead of CommandNotFound when
		// no subcommands matched); otherwise we are in the bare-invocation
		// hard-error path described in decision A9 of the rollout handoff.
		Action: func(_ context.Context, c *cli.Command) error {
			if c.NArg() > 0 {
				_, _ = fmt.Fprintf(stderr, "unknown subcommand: %q\n", c.Args().First())

				return cli.Exit("", ExitErr)
			}

			_, _ = fmt.Fprintln(stderr, noSubcommandMessage)

			// Wrap as cli.Exit so run() sees an ExitCoder and does NOT
			// reprint the message as a generic "Error: ..." line.
			return cli.Exit(errNoSubcommand, ExitErr)
		},
		// Suppress urfave's built-in "print error + os.Exit" handler.
		// Subcommands print on their own and return cli.Exit values that
		// exitCodeFor turns into the final exit code in main().
		ExitErrHandler: func(_ context.Context, _ *cli.Command, _ error) {},
		// Surface unknown subcommand or flag typos as plain errors rather
		// than the default "show usage and exit 0" behaviour.
		CommandNotFound: func(_ context.Context, _ *cli.Command, name string) {
			_, _ = fmt.Fprintf(stderr, "unknown subcommand: %q\n", name)
		},
		Commands: []*cli.Command{
			newServeCommand(stderr),
			newChatCommand(stderr),
			newLoginCommand(stderr),
		},
	}

	return root
}

// run is the testable entry point. It builds the root command, executes it
// with the supplied argv, and returns the urfave error verbatim plus the
// exit code main() should propagate.
//
// Subcommands that produce a user-facing message (no-subcommand, unknown
// subcommand, chat stub) print to stderr themselves and return a
// cli.ExitCoder so run() does NOT double-print. Any other error path
// (notably Serve's wrapped config-load failures) is rendered here as a
// single "Error: ..." line so the operator sees it on the terminal.
func run(ctx context.Context, args []string, stderr io.Writer) (int, error) {
	root := newRootCommand(stderr)

	err := root.Run(ctx, args)
	if err == nil {
		return ExitOK, nil
	}

	if _, ok := errors.AsType[cli.ExitCoder](err); !ok {
		_, _ = fmt.Fprintf(stderr, "Error: %s\n", err.Error())
	}

	return exitCodeFor(err), err
}

// exitCodeFor maps urfave/cli errors to the home-agent exit-code table.
// Subcommands that have their own conventions (chat) communicate via
// cli.Exit; everything else collapses to ExitErr.
func exitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	var exitErr cli.ExitCoder
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return ExitErr
}

func main() {
	// Bind SIGINT/SIGTERM to a top-level cancellable context so the daemon
	// (and any future cli client transport) can shut down gracefully when
	// the operator hits Ctrl+C. Note that we cannot rely on a `defer stop()`
	// here because the function exits via os.Exit at the end — defers do
	// not run. We invoke stop() explicitly before the final exit.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	code, _ := run(ctx, os.Args, os.Stderr)

	stop()
	os.Exit(code)
}

package main

import (
	"context"
	"io"

	"github.com/urfave/cli/v3"
)

// newServeCommand declares the `serve` subcommand surface. The actual daemon
// body lives in Serve() (serve.go); this file is intentionally thin so
// adding or renaming a flag never forces a rewrite of the long boot block.
//
// stderr is forwarded to Serve only through wrapped errors — urfave/cli
// renders them on its own io.Writer by default. We keep the parameter on the
// constructor for symmetry with newRootCommand and to give tests a hook in
// case Serve grows direct stderr writes later.
func newServeCommand(_ io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Run the home-agent daemon (channels, MCP, agent loop)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				// Optional: without it the default agent directory ~/.ready
				// is selected, independent of the working directory. The
				// directory must exist and hold the sole config.yaml.
				Name:  flagAgentDirName,
				Usage: "agent directory holding config.yaml and the agent's private state (default: ~/.ready)",
			},
			&cli.BoolFlag{
				Name:  "mock-mcp",
				Usage: "use the in-process MCP mock instead of a real Home Assistant connection",
				Value: false,
			},
			&cli.StringFlag{
				Name:  "profile",
				Usage: "active MCP profile (default|edit|<custom>); overrides READY_DEFAULT_PROFILE and default_profile config",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			// chat-logo-modes: the banner is subcommand-scoped now that the
			// unconditional print in main() is gone. serve is operator-facing
			// (never script-parsed on stdout), so it keeps printing it
			// unconditionally. `chat` prints only on its TUI path (cmd/chat.go);
			// `login` does not print it at all.
			printReady()

			opts := ServeOptions{
				AgentDir: c.String(flagAgentDirName),
				MockMCP:  c.Bool("mock-mcp"),
				Profile:  c.String("profile"),
			}

			return Serve(ctx, opts)
		},
	}
}

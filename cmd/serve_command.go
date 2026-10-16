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
				// Optional: without it the resolution chain elects
				// ./.ready/config.yaml, then ~/.ready/config.yaml.
				Name:  "config",
				Usage: "path to the YAML configuration file (elected through the resolution chain when omitted)",
			},
			&cli.BoolFlag{
				Name:  "mock-mcp",
				Usage: "use the in-process MCP mock instead of a real Home Assistant connection",
				Value: false,
			},
			&cli.StringFlag{
				Name:  "profile",
				Usage: "active MCP profile (default|edit|<custom>); overrides READY_MCP_PROFILE and mcp_profile config",
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
				ConfigPath: c.String("config"),
				MockMCP:    c.Bool("mock-mcp"),
				Profile:    c.String("profile"),
			}

			return Serve(ctx, opts)
		},
	}
}

package initwizard

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/marco-rozz/ready/internal/config"
)

const platformWindows = "windows"

const firstChatPrompt = "Let's make READY work for me. Show me my current configuration, explain the defaults, and guide me step by step to tailor my agent to my needs. Ask one question at a time."

func printSavedDefaults(_ context.Context, o Options, res config.Resolution, cfg *config.Config) {
	_, _ = fmt.Fprintf(o.stdout(), `
Starter settings saved — you can change all of these later:
  Logs: JSON on stderr and in %s, level INFO (logging.level: info).
  Log rotation: daily, or at %s per file; keep up to %d local calendar days,
  including today, within a %s total limit. Older logs may be removed sooner
  when that space limit is reached. File names include the date.
  Skills: %s, automatic library sync and live reloading of local edits.
  Conversations: %s, daily reset boundary at %s local time.
  An open chat is not interrupted; the reset is checked on the next connection.
  Ended conversations: keep up to %d calendar days, within %s total;
  the current conversation is kept.
  Context: automatic summaries are enabled to keep long conversations manageable.

Edit %s or ask READY in admin chat to change these settings.
Restart the server after saving configuration changes to apply them.
`, o.pathArgument(cfg.Logging.FilePath), config.ByteSize(cfg.Logging.FileRotation.EffectiveMaxSegmentSize()),
		cfg.Logging.FileRotation.EffectiveRetentionDays(), config.ByteSize(cfg.Logging.FileRotation.EffectiveMaxTotalSize()),
		o.pathArgument(cfg.Skills.Path), o.pathArgument(cfg.Sessions.Path), cfg.Sessions.Reset.DailyAt,
		cfg.Sessions.Retention.Days(), cfg.Sessions.Retention.MaxTotalSize, o.pathArgument(res.Path))
}

func quoteHost(path string) string { return config.QuoteAgentDir(path) }

func executableName(goos string) string {
	if goos == platformWindows {
		return "ready.exe"
	}

	return "ready"
}

// shellArgument keeps ordinary examples short and quotes shell metacharacters.
func shellArgument(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		safe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-_/.:", r) ||
			(runtime.GOOS == platformWindows && r == '\\')

		return !safe
	}) == -1 {
		return value
	}

	return quoteHost(value)
}

func (o Options) pathArgument(path string) string {
	if o.WorkingDir != "" {
		if relative, err := filepath.Rel(o.WorkingDir, path); err == nil {
			path = relative
			if path != "." && path != ".." && !strings.HasPrefix(path, ".."+string(filepath.Separator)) {
				path = "." + string(filepath.Separator) + path
			}
		}
	}

	return shellArgument(path)
}

func (o Options) initCommand(agentDir string) string {
	return executableName(runtime.GOOS) + " init --agent-dir " + o.pathArgument(agentDir)
}

// chatCommand is the chat client invocation for this agent; --agent-dir is
// printed only for a non-default agent.
func (o Options) chatCommand(agentDir string) string {
	command := executableName(runtime.GOOS) + " chat"
	if !config.IsDefaultAgentDir(agentDir) {
		command += " --agent-dir " + o.pathArgument(agentDir)
	}

	return command
}

func (o Options) loginCommand(agentDir, instance string) string {
	return executableName(runtime.GOOS) + " login " + shellArgument("llm:"+instance) + " --agent-dir " + o.pathArgument(agentDir)
}

func (o Options) serveCommand(agentDir string, admin bool) string {
	command := executableName(runtime.GOOS) + " serve --agent-dir " + o.pathArgument(agentDir)
	if admin {
		command += " --profile admin"
	}

	return command
}

// chatAddress turns a server's wildcard bind into a connectable local address.
func chatAddress(bind string) string { return config.ConnectableAddress(bind) }

func printInitialCommands(_ context.Context, o Options, res config.Resolution, cfg *config.Config) {
	if cfg == nil {
		return
	}
	out := o.stdout()
	_, admin := cfg.Profiles["admin"]
	_, _ = fmt.Fprintln(out, "\nNext steps — start READY, then open your chat")
	if o.WorkingDir != "" {
		_, _ = fmt.Fprintf(out, "\nRun the examples from this directory in BOTH terminals:\n   cd %s\n", shellArgument(o.WorkingDir))
	}
	_, _ = fmt.Fprintf(out, "\nThese commands use %s from PATH. If it is not found, open a new terminal\nafter installation and check the installer's PATH instructions.\n", executableName(runtime.GOOS))
	if runtime.GOOS == platformWindows {
		_, _ = fmt.Fprintln(out, "On Windows the executable is ready.exe. The examples below use PowerShell syntax.")
	} else {
		_, _ = fmt.Fprintln(out, "On Linux and macOS the installed executable is named ready (no extension).")
	}
	_, _ = fmt.Fprintf(out, "\n1. In this terminal, start the READY server:\n\n   %s\n\n   Leave this terminal open while you use READY. The server handles your\n   messages; this command does not open the chat interface.\n", o.serveCommand(res.AgentDir, admin))
	if admin {
		_, _ = fmt.Fprintln(out, "   The admin profile is required to manage configuration. It is selected by\n   the server with --profile admin; chat uses the server's active profile.\n   Private credential entry also requires a loopback server address.")
	}
	_, _ = fmt.Fprintln(out, "\n2. Open a SECOND terminal in the SAME directory. Once the server is running:")
	switch {
	case !cfg.Channels.ZMQCLI.Enabled:
		_, _ = fmt.Fprintf(out, "\n   Chat is disabled in %s. Enable channels.zmqcli and restart the server before connecting.\n", o.pathArgument(res.Path))
	default:
		chat := o.chatCommand(res.AgentDir)
		address := chatAddress(cfg.Channels.ZMQCLI.Bind)
		_, _ = fmt.Fprintf(out, "\n   With --address (explicitly selects this server):\n   %s --address %s\n\n   Without --address:\n", chat, shellArgument(address))
		addressHint := "For this agent, READY_ADDR must be unset or match the address above."
		if address != defaultZMQBind {
			addressHint = "For this agent, READY_ADDR must match the address above."
			_, _ = fmt.Fprintln(out, "   Set READY_ADDR first because this server uses a non-default address:")
			if runtime.GOOS == platformWindows {
				_, _ = fmt.Fprintf(out, "   $env:READY_ADDR = %s\n", quoteHost(address))
			} else {
				_, _ = fmt.Fprintf(out, "   export READY_ADDR=%s\n", shellArgument(address))
			}
		}
		_, _ = fmt.Fprintf(out, "   %s\n\n   Without --address, chat uses READY_ADDR when set, otherwise %s.\n   %s\n   The chat client reads this agent's chat secret by itself (same machine).\n   Type a message and press Enter. If connection fails, check the SERVER terminal.\n", chat, defaultZMQBind, addressHint)
	}
	_, _ = fmt.Fprintf(out, "\n3. To stop READY, press Ctrl+C in the SERVER terminal.\n   After editing the configuration, stop it and start it again to load changes.\n\n   For everyday use, start with the configured default profile:\n   %s\n\n   Keep --agent-dir: it selects this agent's configuration, history and skills.\n   The chat commands stay the same when you change the server profile.\n", o.serveCommand(res.AgentDir, false))
	_, _ = fmt.Fprintln(out, "\n4. Make READY your own")
	if admin {
		_, _ = fmt.Fprintln(out, "   With the server running under --profile admin, paste this message into READY chat:")
	} else {
		_, _ = fmt.Fprintln(out, "   Configuration changes require a server profile with configuration tools enabled.\n   To begin, paste this message into READY chat:")
	}
	_, _ = fmt.Fprintf(out, "\n   %s\n", firstChatPrompt)
}

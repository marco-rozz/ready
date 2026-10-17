# Using READY. — subcommands, chat client, TUI, env vars

The binary is `ready` and has exactly three subcommands: `serve`, `chat`,
`login`. The bare command is a hard error on purpose:

- `ready` (no subcommand) → stderr `missing subcommand: use 'serve', 'chat' or 'login'`, exit 1.

## ready serve

The long-lived daemon: channels, MCP runtime, skills, agent loop.

- Flags: `--config <path>` (optional; otherwise chain-elected — see
  `install-upgrade.md`), `--mock-mcp` (in-process mock server, replaces the
  real MCP catalog), `--profile <name>` (overrides `READY_MCP_PROFILE` and
  the `mcp_profile` config key — note this only selects the profile; the
  profile switch itself still requires a restart).
- Prints the READY banner unconditionally, then logs the elected config as
  INFO (`config`, `origin`, `runtime_home`) and warns when the file is
  readable by group/others on unix.

## ready chat

The terminal client. It connects to a running daemon over ZeroMQ and
authenticates with HMAC-SHA256 challenge-response (`session_start` →
`challenge` nonce → `hex(HMAC-SHA256(token, nonce))`).

Flags:

| Flag | Meaning |
|---|---|
| `--address` | daemon endpoint; default `tcp://127.0.0.1:7600`; `READY_ADDR` is the fallback |
| `--token` | shared secret; `READY_TOKEN` is the fallback |
| `--token-file` | read the token from a file (lowest priority after flag and env) |
| `--exec "MSG"` | one-shot: single request, reply, `session_end`, exit |
| `--reply-timeout` | per-reply deadline; default 900s |
| `--raw` | disables markdown rendering and live preview; final reply only (always idempotently redacted) |

`chat` never reads `config.yaml` — tokens and addresses come only from
flags/env. Without env vars and without a token flag, the token stays empty
(HMAC over an empty secret fails against a configured daemon secret).

### TUI (interactive terminal)

When both stdin and stdout are a TTY and `--raw` and `--exec` are absent, a
Bubble Tea interface runs: full-height transcript viewport, multiline input
editor, persistent status line (turn state, live model state, usage,
new-content indicator).

- Keys: `Return` sends; `Shift+Return` / `Alt+Enter` / `Ctrl+J` insert a
  newline (`Ctrl+J` works everywhere; `Shift+Return` needs a cooperative
  terminal, see `READY_SHIFT_ENTER` below); `Ctrl+C` quits (exit 130);
  `Ctrl+D` quits on an empty editor (exit 0), else forward-deletes;
  `Ctrl+G` jumps to the bottom; mouse wheel scrolls.
- Typing a message while a turn is in flight queues it; the queue drains
  FIFO when the turn completes. The daemon itself rejects concurrent
  requests (`request_in_flight`), so nothing double-fires.
- `/session` and `/context` are TUI-reserved: they are answered by the
  daemon directly (no LLM call, no history change) and render as transcript
  blocks — Session ID, address, compaction count, token totals, model.
- `/new` and `/compact [instructions]` are NOT reserved: they are forwarded
  as ordinary messages and answered by the daemon's session-management
  layer (fixed answers: `New session started.`, `Session already new.`,
  `Context compacted.`, `Nothing to compact.`,
  `Compaction failed; the session is unchanged.`). See `sessions-life.md`.
- Model state in the status line: green = configured default, yellow = an
  active fallback, red = `LLM unavailable` (no candidate healthy).
- Usage segment `R… C…% O… · ctx …% [turns»llm calls]` is hidden until the
  first usage data arrives; `ctx` tints yellow ≥70% and red ≥90%.
- When the LLM chain is exhausted, the failure arrives as a `reply` with
  `error_code: llm_unavailable` and renders as a completed turn — the
  session stays open. Session expiry messages (`no_active_session`,
  `session_timeout`) remain terminal (exit 1).
- Startup banner: `READY_LOGO` selects the presentation
  (`auto` = PNG on image-capable terminals, glyph otherwise; `png`, `glyph`,
  `off`). Unset/unknown = `auto`. `--exec`, piped paths and `--raw` never
  print a banner in any mode.
- `READY_SHIFT_ENTER` (`on`/`off`, default automatic) recovers `Shift+Return`
  out of band on terminals that do not negotiate it (macOS Terminal.app,
  some multiplexers). The fallback is skipped over SSH.

### Non-TUI paths

- Piped stdin/stdout: each line is one request, each reply rendered once;
  EOF (Ctrl+D) exits 0.
- `--exec "MSG"`: one request, one reply, exit 0 on success. If the reply is
  `error_code: llm_unavailable`, the terminal message goes to stderr and the
  exit code is 1 (the generic-failure row of the table below, unchanged so
  scripts' exit-code assumptions never break). Scripts distinguish an
  exhausted LLM chain from other failures via the reply's `error_code`,
  never via the exit code.

### Exit codes (`ready chat` and `ready login`)

| Code | Meaning |
|---|---|
| 0 | success (also: EOF/Ctrl+D, daemon shutdown control) |
| 1 | generic error |
| 2 | daemon busy — another session is attached |
| 3 | HMAC authentication failed |
| 124 | reply deadline reached (`timeout(1)` convention) |
| 130 | SIGINT |

## ready login

The only place a browser is ever opened. Handles OAuth for MCP servers and
the `openai-codex` LLM provider.

- Targets: `mcp:<server>`, `llm:<provider>` (only `openai-codex` providers).
  An unqualified name is accepted when it matches exactly one namespace;
  two matches are refused as ambiguous.
- No target: every OAuth-capable MCP server and codex provider runs, one
  browser flow at a time.
- MCP flow: discovery → one-time Dynamic Client Registration when needed →
  interactive authorization → credential persisted → connect → the full
  unfiltered tool catalog is printed plus how many tools the active profile
  would expose, and a reminder that a running daemon needs a restart before
  these tools appear (the tool slice is built once at boot).
- `--status`: credential table (grant, presence, expiry, action required),
  no network I/O, nothing written.
- `--forget <target>`: RFC 7592 deregister (best-effort) then delete the
  stored credential. Mutually exclusive with `--status`.
- `--device-code`: headless flow for codex (default is browser PKCE).
- Prints `using config <path>` as its first line; exit codes per the table
  above (3 = authorization failed).

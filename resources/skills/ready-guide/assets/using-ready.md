# Using READY. — subcommands, chat client, TUI, env vars

The binary is `ready` and has the subcommands `init`, `serve`, `chat`, and
`login` and `secrets`. The bare command is a hard error on purpose:

- `ready` (no subcommand) → stderr `missing subcommand: use 'serve', 'chat', 'login', 'init' or 'secrets'`, exit 1.

## ready serve

The long-lived daemon: channels, MCP runtime, skills, agent loop.

- Flags: `--agent-dir <directory>` (optional; default `~/.ready`,
  cwd-independent — see `agent-directories.md`), `--mock-mcp` (in-process
  mock server, replaces the real MCP catalog), `--profile <name>` (overrides
  `READY_DEFAULT_PROFILE` and the `default_profile` config key — note this only
  selects the profile; switching profile that way still requires a restart,
  while a saved `default_profile` applies on the in-process reload).
- Prints the READY banner unconditionally, then logs the selected agent
  directory as INFO (`agent_dir`, `config`, `origin`) and warns when the
  config file is readable by group/others on unix.

## ready init

The installer leaves config.example.yaml in program storage and creates no
agent config. Run ready init (ready.exe init on Windows) after installation
to enter the full wizard for a new agent; existing configs open offline repair.

`ready init [--agent-dir <directory>]` is the deterministic terminal wizard
for a new or repairable current-schema agent. It uses `~/.ready` when no
directory is supplied, independently of cwd, and is the only command allowed
to create a missing agent directory. API-key input disables terminal echo;
EOF, cancellation and non-TTY input fail before publication. The generated
config holds references only; the provider key and the chat secret are in the encrypted Secret store. `--config` is rejected.

Ollama asks `local` or `cloud`: local defaults to `http://127.0.0.1:11434`
without a key, cloud to `https://ollama.com` with a required API key. The
host is editable and must omit `/api`, which the client appends. The encrypted Secret store preserves entered/generated values exactly.
Missing `ready-guide` content produces a warning and a saved config without
admin skill injection; install the library and add `ready-guide` to
`profiles.admin.inject_skills` to enable it. Failures print their reason on
stderr; interruptions before publication explicitly report that nothing was saved.

The final output comes after optional checks and gives numbered instructions:
start `serve` in one terminal, leave it running, and open `chat` in a second
terminal on the same computer. Commands use `ready` from PATH on Linux/macOS and `ready.exe` on Windows,
with relative agent paths anchored to the displayed working directory.
Use that directory in both terminals. Chat is shown with and without
`--address`; custom addresses set `READY_ADDR` for the second variant.
Setup uses `--profile admin` only on `serve`, to enable configuration
management. The chat client uses the active server profile and has no
`--profile` option.
Wildcard server binds are translated to local loopback addresses for the client.
The wizard also explains stopping with Ctrl+C and restarting with the configured
default profile. Saving configuration does not start READY or download skills;
init prepares the first skill copy from the installed shared library, under
agent ownership after config publication. The server synchronizes later
library updates at startup, preserving local edits. Missing libraries warn;
copy failures preserve the saved config and are retried by serve.

New agents save editable starter defaults: JSON logging on stderr and in
`logs/ready.log`, INFO (`level: info`), daily/10MB rotation, up to seven local
calendar days and a 100MB total cap. Skills use auto sync and hot reload;
conversations reset at the daily 04:00 local boundary on the next connection,
without interrupting an open chat. Ended conversations retain up to 30 days
within 200MB; automatic context summaries are enabled. Space limits can
remove older files sooner. Confirmed admin-chat saves reload the runtime
automatically; direct file edits require a server restart. Logging changes
always require a restart. Offline repair preserves existing settings.
The last wizard step suggests a first chat message to review the current
configuration and personalize the agent, one question at a time.

## ready chat

The terminal client. It connects to a running daemon over ZeroMQ and
authenticates with HMAC-SHA256 challenge-response (`session_start` →
`challenge` nonce → `hex(HMAC-SHA256(token, nonce))`).
The ZMQ wire protocol is version 3 and must match on client and server.
Select the profile when starting `serve`; the chat command stays the same.

Flags:

| Flag | Meaning |
|---|---|
| `--address` | daemon endpoint; default `tcp://127.0.0.1:7600`; `READY_ADDR` is the fallback |
| `--agent-dir` | the agent whose address and chat secret to use (default `~/.ready`) |
| `--token` | shared secret; `READY_TOKEN` is the fallback |
| `--token-file` | read the token from a file (after flag and env, before the agent's own secret) |
| `--exec "MSG"` | one-shot: single request, reply, `session_end`, exit |
| `--reply-timeout` | per-reply deadline; default 900s |
| `--raw` | disables markdown rendering and live preview; final reply only (always idempotently redacted) |

With no token flag or `READY_TOKEN`, `chat` reads only the selected agent's
`channels.zmqcli` and `secrets` blocks and that one chat secret (read-only, no
locks, nothing written) and, without `--address`/`READY_ADDR`, that agent's bind
address. For a daemon on another host copy the secret with
`ready secrets reveal zmqcli_auth_token --to-file F` and pass `--token-file F`.

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
- When pending credentials are advertised, the ZMQ TUI opens a separate RED
  `/secrets` area only when the active profile exposes `config-manager` and
  the listener is numeric loopback. It never puts literal values in chat
  drafts, history or model messages. Escape skips; consecutive Escape,
  Ctrl+C or Ctrl+D exits the area. Each committed value is persisted
  immediately and the pending list is refreshed. **Post-save activation**:
  after a turn that saved the configuration (or on connect with incomplete
  items) the TUI runs, in sequence and without leaving the screen, the red
  secret editor, then each OAuth login (`login_required` target: the browser
  opens, the URL is printed, paste the redirect URL/code + Enter or Esc to
  skip), then an in-process runtime reload (`reloading` spinner, then
  `── configuration reloaded ──`): no restart, same Session and Connection.
  Secret input is masked by default; Ctrl+R reveals the current field. OAuth
  redirect/code input is visible; SSH forwarding to a numeric
  loopback listener supports remote use. The ZMQ token alone is adopted
  immediately after atomic file replacement; update separate client copies.
- Usage segment `R… C…% O… · ctx …% [turns»llm calls]` is hidden until the
  first usage data arrives; `ctx` tints yellow ≥70% and red ≥90%.
- When the LLM chain is exhausted, the failure arrives as a `reply` with
  `error_code: llm_unavailable` and renders as a completed turn — the
  session stays open. A lost Connection (`no_active_session`) is terminal (exit 1); reconnect
  to resume the current conversation Session. Connection expiry does not
  itself archive that Session.
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

### Exit codes (`ready chat`)

| Code | Meaning |
|---|---|
| 0 | success (also: EOF/Ctrl+D, daemon shutdown control) |
| 1 | generic error |
| 2 | daemon busy — another session is attached |
| 3 | HMAC authentication failed |
| 124 | reply deadline reached (`timeout(1)` convention) |
| 130 | SIGINT |

## ready login

The standalone OAuth command for MCP servers and the `openai-codex` LLM
provider. The admin TUI can also coordinate an OAuth browser flow through
the daemon during post-save activation.

- Targets: `mcp:<server>`, `llm:<provider>` (only `openai-codex` providers).
  An unqualified name is accepted when it matches exactly one namespace;
  two matches are refused as ambiguous.
- No target: every OAuth-capable MCP server and codex provider runs, one
  browser flow at a time.
- MCP flow: discovery → one-time Dynamic Client Registration when needed →
  interactive authorization → credential persisted → connect → the full
  unfiltered tool catalog is printed plus how many tools the active profile
  would expose. An already connected target adopts fresh credentials; a target
  omitted from the catalog needs a runtime reload or restart to expose tools.
  The post-save TUI performs that reload after its login flow.
- `--status`: credential table (grant, presence, expiry, action required),
  no network I/O, nothing written.
- `--forget <target>`: RFC 7592 deregister (best-effort) then delete the
  stored credential. Mutually exclusive with `--status`.
- `--device-code`: headless flow for codex (default is browser PKCE).
- Prints `using agent directory <path>` as its first line. Login failures use
  their own authorization/status handling; the table above describes chat.

## ready secrets

Manage the selected agent's encrypted Secret store with `--agent-dir <directory>`
(default `~/.ready`). `list` shows names and state, never values; `set <name>`
uses a hidden terminal prompt (`--stdin` is available for a pipe); `rm <name>`
refuses a referenced Secret unless forced. `reveal <name> --to-file F` writes
one value to a new private file. Never paste it into conversational chat.

`adopt` converts hand-written literals in recognized sensitive configuration
fields into references and seals the previous config. `export-key` / `import-key`
back up and restore the Store key; `move-key` changes its location; `forget-key`
removes an OS-vault key entry. See `agent-directories.md` and
`config-manager.md` for key placement, copy identity and recovery.

The default files are `<agent-dir>/secrets.age` and `secrets.key`. Keeping
both together does not protect against a process running as the same user;
keep a recoverable copy of the key when backing up or moving the store.

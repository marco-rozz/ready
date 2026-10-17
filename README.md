# READY.

![READY.](READY.png)

`READY.` is a home-automation assistant you host yourself: you talk to your
Home Assistant in natural language — from a terminal (over ZeroMQ), Telegram,
WhatsApp, or a Commodore 64 over serial — and it turns the request into
device commands that are **verified, not just dispatched**.

Most of the value is in the answer to one question: *did it actually work?*
`READY.` does not accept a command as done until Home Assistant's event stream
confirms the effect on the target entity. Every command returns a per-entity
outcome — `applied`, `pending`, `already`, `unchanged`, `unavailable`,
`error` — and when anything goes wrong part-way, the reply says what may have
run, so a failed request never invites a blind repeat. If the LLM provider
dies mid-turn, the failover chain (Ollama, OpenRouter, OpenCode, or an
experimental ChatGPT/Codex-OAuth provider) resumes from the recorded
transcript without re-executing the actions already completed.

Under the hood it is a Go daemon orchestrating an LLM through the **ReAct**
pattern (Thought → Action → Observation) with native tool calling, connected
to Home Assistant via the Model Context Protocol plus an always-on WebSocket
digital twin that mirrors every entity state and registry in memory.

## Install

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | bash
```

Windows (PowerShell 5.1+):

```powershell
irm https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.ps1 | iex
```

Options cannot be given to a piped shell directly — use the parameter-passing
forms:

```bash
curl -fsSL <install.sh-url> | bash -s -- --path        # also edit the rc file
curl -fsSL <install.sh-url> | bash -s -- --uninstall
```

```powershell
& ([scriptblock]::Create((irm <install.ps1-url>))) -Uninstall
& ([scriptblock]::Create((irm <install.ps1-url>))) -Prefix D:\ready\bin
```

Installer options (same semantics on both scripts): `--prefix <dir>` (binary
directory; default `$HOME/.ready/bin`, Windows `%LOCALAPPDATA%\Programs\ready\bin`),
`--global` (sh only: `/usr/local/bin` via sudo), `--path` (sh only: apply the
PATH export), `--uninstall`, `--purge` (with `--uninstall`: also delete user
data; asks for confirmation unless `--yes`). Pin a version with the `VERSION`
environment variable:

```bash
VERSION=v0.1.2 curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/v0.1.2/scripts/install.sh | bash
```

Notes:

- The installer is idempotent and upgradable: re-running it replaces the
  binary and the shipped skill library, **keeps your `config.yaml`** and never
  touches your runtime skill copies or session transcripts. End of every
  upgrade: the fixed line `restart ready serve to use the new version`.
- Linux amd64/arm64 binaries are dynamically linked and require the glibc
  loader (`/lib64/ld-linux-x86-64.so.2`, `/lib/ld-linux-aarch64.so.1`):
  Alpine/musl, NixOS and distroless hosts are unsupported (the linux `arm/v6`
  build is statically linked and needs no loader).
- The SHA256 verification of every download guards **integrity** (corruption,
  truncated downloads), not authenticity: the trust root is the GitHub
  repository and TLS.

## How READY. finds its config

Exactly one config file is elected, in strict order — no merging, the rejected
candidates have no influence on the run:

1. the explicit `--config <path>` flag (a missing flagged file fails the run
   naming that path);
2. `./.ready/config.yaml` relative to the working directory;
3. `~/.ready/config.yaml` (the Install home; `%USERPROFILE%\.ready\config.yaml`
   on Windows).

**The runtime home** — the directory containing the elected config — is the
root of every runtime path: `sessions/`, `skills/`, and any relative
`logging.file_path`. A deployment-local run keeps its data next to its config;
a global-config run keeps everything inside the Install home. One deliberate
carve-out keeps things sane: the OAuth credential store anchors to the Install
home (`~/.ready/oauth/`) because a credential is per-user identity, not
deployment data — an explicitly configured relative `oauth.path` still follows
the runtime home.

`ready serve` and `ready login` both announce the elected config at start
(the daemon logs it at INFO and warns — never fails — when it is readable by
group or others on unix; `login` prints `using config <path>` as its first
line), so a login run from the wrong directory is visible immediately.

### Install home layout

```
~/.ready/
├── bin/ready            # the binary (unless --prefix / --global / Windows)
├── config.yaml          # global config fallback (copy-if-missing, 0600)
├── library/skills/      # the shipped skill library (installer-owned;
│                        #   replaced wholesale on every install)
├── skills/              # runtime skill copy (written only by the daemon;
│                        #   populated and kept current by the boot sync)
├── sessions/            # session transcripts (global-config runs)
└── oauth/               # default OAuth credential store
```

The library is the installer's source of truth, replaced on every install;
your writes live in the runtime copies. At boot `ready serve` syncs
`<runtime home>/skills/` from the library **per file** (design choice in the
docs: three-way check against a recorded state): files you never touched
receive shipped fixes, your edits are never overwritten (a conflict is left
untouched and reported), your deletions are respected, and machine-generated
assets (`generated/`) are always yours. The behaviour is one config key:

```yaml
skills:
  sync: auto         # auto (default) | notify (report only) | off
  # library_path: ../resources/skills/   # override; relative paths resolve
                                         # against the runtime home
```

## Configure before first use (required)

The daemon ships no usable default brain — it will not start a useful
conversation until **you** tell it which LLM provider to talk to. This is
the one mandatory, hand-made configuration step, and it happens before the
first `ready serve`:

1. Open the elected config file (see the section below — usually the copy
   the installer created at `~/.ready/config.yaml`).
2. Edit the `llm:` block: declare at least one **provider instance**
   under `llm.providers` (type, wire `api`, `base_url`, credentials) and
   point `llm.default` at it as `<provider-instance>::<model-id>`, e.g.
   `"ollama-gemma::gemma4:31b"`. Every key is commented in
   [config.example.yaml](config.example.yaml), including the failover
   chain and the experimental ChatGPT/Codex provider
   (`ready login llm:<name>`).
3. Restart `ready serve`. Without this, the daemon boots in the
   `LLM unavailable` state and every request fails until a provider
   answers.

### The `home_assistant:` block (the digital twin — not the MCP server)

The **root** `home_assistant:` block is *not* the MCP server entry under
`mcp_servers:` (that one is deprecated — see the commented reference in
`config.example.yaml`). It is the **digital twin**: an always-on WebSocket
connection to your Home Assistant that keeps an in-memory mirror of every
entity state and registry, and serves the agent two dedicated tools:

- `ha_state_query` — reads the mirror (entities, areas) with freshness
  signals attached (`twin_connected`, `last_ws_activity_ago_seconds`,
  `stale`); past the hard-stale threshold it refuses to serve rather than
  showing you an old house.
- `ha_command` — commands devices on the same live connection and **verifies**
  the effect against Home Assistant's own state event stream, per entity:
  `applied`, `pending`, `already`, `unchanged`, `unavailable`, `error`.
  "Done" means observed, not merely sent.

Under the default profile the twin is the **only** read path: the MCP read
supersede tools are denied, so the agent cannot answer from a stale mirror
it never checked. Enable and configure it (WebSocket `url` + long-lived
`token`, thresholds, command domains in the `home_assistant.commands`
section of `config.example.yaml`) before relying on anything that changes
your home. The config is read at boot — enabling it later requires a
daemon restart.

### Not sure what goes where? Ask READY. itself.

Once the LLM/provider block is working, the shipped skill
**`ready-guide`** knows the full configuration surface (channels, profiles,
sessions, compaction, tuning, troubleshooting). Chat with the daemon
(`ready chat`) and ask, for example: *"help me enable and configure the
digital twin"* or *"walk me through adding Telegram"*. The assistant loads
the matching section and walks you through it one YAML block at a time.

## Environment variables

| Variable | Meaning |
|---|---|
| `READY_ADDR` | fallback for `ready chat --address` (default `tcp://127.0.0.1:7600`) |
| `READY_TOKEN` | fallback for `ready chat --token` (unset: the token stays empty and the HMAC handshake fails against a configured daemon secret; use `--token-file`) |
| `READY_MCP_PROFILE` | fallback for `serve --profile` (`default` / `edit` / custom) |
| `READY_LOG_LEVEL` | forces the log level of both sinks (`serve` only) |
| `READY_LOGO` | the `ready chat` logo presentation: `auto` (default) / `png` / `glyph` / `off` |
| `READY_SHIFT_ENTER` | force the SHIFT+RETURN out-of-band fallback `on`/`off` (auto by default; `Ctrl+J` / `Alt+Enter` always work) |

The former `READY_HOME_*` prefix is retired; there is no alias — rename any
launcher environment accordingly.

## Subcommands

- **`ready serve [--config <path>] [--mock-mcp]`** — the long-lived daemon.
  `--config` is optional (the resolution chain applies). `--mock-mcp` selects
  the in-process MCP mock for local development.
- **`ready chat --address <addr> [--token T] [--token-file F] [--exec MSG] [--reply-timeout 900s] [--raw]`**
  — the terminal client (Bubble Tea TUI, markdown rendering, usage and model
  status in the status line; `/session` and `/context` are reserved commands).
- **`ready login [mcp:<server>|llm:<provider>] [--config <path>] [--status] [--forget] [--device-code]`**
  — OAuth login for MCP servers and the `openai-codex` provider; the only
  place a browser is opened. `--forget` deletes a credential.

`ready --version` prints `ready <version> (commit <sha>, built <date>)`.

### `ready chat` exit codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | Generic error (connection failure, parse error, LLM service unavailable…) |
| `2` | Daemon busy — another session is already attached |
| `3` | HMAC authentication failed |
| `124` | Timeout (`timeout(1)` convention) |
| `130` | SIGINT |

`chat` never writes files outside the config the daemon uses, and never
initialises logging: errors are single human-readable lines on stderr.
The `llm_unavailable` failure is a *completed request that failed*: its
terminal message goes to stderr with exit `1` on `--exec` and piped paths —
scripts distinguish it by the reply's `error_code: llm_unavailable` field.

## Configuration

`config.example.yaml` is the complete, commented reference. Highlights:

- `llm.providers` + `llm.default` / `llm.failover` — named provider
  connections referenced as `<provider>::<model>`; the failover chain is
  walked live at runtime with health tracking and cooldown ladders.
- `channels` — `cli` (stdin) or `zmqcli` (for `ready chat`), `telegram`,
  `serial`, `whatsapp`; each may carry a `formatter_skill`.
- `mcp_servers` — Home Assistant (or any MCP server), Streamable HTTP by
  default, per-server tool prefixing, reconnect/health knobs, OAuth.
- `home_assistant` — the always-on digital twin behind the
  `ha_state_query` / `ha_command` tools (read/write discipline, effect
  verification, stale thresholds).
- `skills` — path, hot reload, `sync`, `library_path` (see above).
- `sessions` — per-address transcript files, reset rules (`daily`/`idle`),
  retention; `/new` and `/compact` are session commands.
- `compaction` — automatic context compaction with a summariser skill.
- `mcp_profile` + per-owner `profiles.<name>.{allow,deny}` — tool gating
  (fail-closed); `disclose_denied_tools` turns blocked calls into an explicit
  disclosure.

Credentials (provider keys, ZMQ HMAC token, OAuth secrets) live in
`config.yaml` (`0600`, created by the installer as a copy of the example
config) or in files referenced by `*_file` keys.

## Development

```bash
go build -o ready ./cmd       # or: make build
go run ./cmd serve            # elects ./ready/config.yaml, then ~/.ready/config.yaml
go test ./...
make qa                       # lint + tests + vulnerability scan
make snapshot                 # goreleaser release --snapshot (local bundle check)
```

CI builds the exact release matrix (linux amd64/arm64/arm-v6+`noshiftprobe`,
darwin amd64/arm64, windows amd64 — all `CGO_ENABLED=0`) and asserts the
linux/arm6 artifact is statically linked; releases publish from a `v*` tag
push via goreleaser.

## License

MIT — see [LICENSE](LICENSE).

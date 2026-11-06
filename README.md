# READY.

![READY.](READY.png)

**Your home, in your words. Actions checked against what actually happened.**

READY. is a self-hosted assistant that connects natural-language requests to
Home Assistant. Run its Go daemon on your own hardware, talk to it from a
terminal or Telegram, and choose where the language model runs: on your own
machine through Ollama, or with a configured cloud provider.

The goal is a home assistant that understands the house, carries out a request,
and explains the result. The foundation is already here: a live digital twin,
verified device commands, persistent conversations, runtime model failover,
and guided configuration that keeps credentials out of the model's context.

[Get started](#get-started) · [Install](#install) ·
[Configuration](#configuration) · [Secrets](#secrets) ·
[Development](#development)

## What READY. can do today

| Capability | What it gives you |
|---|---|
| **A live view of your home** | A WebSocket digital twin mirrors Home Assistant entity states and registries in memory. State queries carry freshness signals and refuse to serve a hard-stale view. |
| **Commands with evidence** | `ha_command` checks effects against Home Assistant state events and reports an outcome per entity, including pending or unverified results. An acknowledgement alone is not proof of success. |
| **Your choice of model** | Ollama, OpenRouter and OpenCode integrations, plus an experimental ChatGPT/Codex OAuth provider. Ordered failover tracks health and resumes eligible failed turns from recorded tool results. |
| **A terminal built for conversations** | Streaming replies, Markdown, multiline input, model status and live token usage in the TUI; a one-shot mode for scripts; Telegram for a second way in. |
| **Continuity across restarts** | Separate conversations per channel address, append-only session transcripts, `/new`, `/compact`, automatic summaries and configurable retention. |
| **Guided setup and configuration** | `ready init` creates an agent. An opt-in admin profile lets you review and confirm configuration edits in chat, then complete Secrets and OAuth in the TUI and reload the runtime. |
| **Skills and MCP tools** | File-based skills describe your home and workflows; MCP connects additional tools. Profiles control which tools each agent exposes. Shipped skill updates preserve local edits. |
| **Small machines, independent agents** | A static ARMv6 build supports Raspberry Pi 1-class hosts. Separate agent directories keep configuration, credentials, skills and conversations independent. |

For example, with Home Assistant and the relevant tools configured, ask:

- “Which lights are still on downstairs?”
- “Turn off the living-room lights and check the result.”
- “Help me add Telegram to this agent.” — in an admin session.

The LLM plans the steps through Eino ADK's **ReAct** loop
(Thought → Action → Observation). Home Assistant supplies the observations.
Verification describes the state reported by Home Assistant; it cannot prove a
physical effect that a device or integration does not report. Automatic retry
or failover requires a complete recorded tool-call group and no final-answer
content delivered yet; a partial failure can still require your attention.

## Project status

**READY. is under active construction.** The capabilities above are implemented,
but configuration and protocols can change between releases. Read the upgrade
guidance before replacing an existing installation and keep a recoverable backup
of the agent directory and its Store key.

The ChatGPT/Codex provider is experimental. Serial/PETSCII access for vintage
terminals, including the Commodore 64, is a planned direction; the serial
channel is not implemented yet. Tool profiles limit the available catalog;
they are not an OS sandbox for code or scripts executed by a skill.

> **A home server does not need to be a new computer.** The release matrix
> includes a statically linked Linux ARMv6 build for a 2014 Raspberry Pi.
> The LLM can run on another machine. Slow storage may leave the first start
> silent for tens of seconds before the banner; see the shipped `ready-guide`
> troubleshooting asset.

## Get started

1. Install READY. using the commands below, then open a new terminal.
2. Run `ready init` (`ready.exe init` on Windows). Choose your provider/model;
   the wizard creates the agent, collects credentials privately, and prints
   the exact commands for your setup.
3. For the default agent, start the setup server with
   `ready serve --profile admin`, then run `ready chat` in a second terminal.
   For a custom directory or endpoint, use the commands printed by the wizard.
4. Ask READY. to help configure the Home Assistant twin and any MCP servers you
   need. Review configuration changes before confirming a save. Once configured,
   stop the setup server and run `ready serve` with your normal profile.

A fresh agent has no connection to your home until you configure one.
The admin profile is for setup; available device actions depend on Home
Assistant, the active command vocabulary and tool-profile permissions.

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
curl -fsSL <install.sh-url> | bash -s -- --no-modify-path   # skip the PATH edit
curl -fsSL <install.sh-url> | bash -s -- --uninstall
curl -fsSL <install.sh-url> | bash -s -- --uninstall --purge
```

```powershell
& ([scriptblock]::Create((irm <install.ps1-url>))) -Uninstall
& ([scriptblock]::Create((irm <install.ps1-url>))) -Uninstall -Purge
& ([scriptblock]::Create((irm <install.ps1-url>))) -NoModifyPath
```

Installer options (same semantics on both scripts): `--no-modify-path` /
`-NoModifyPath` (skip the persistent PATH edit; manual instructions are
printed either way), `--uninstall` / `-Uninstall` (removes the program and
the shipped skill library — never PATH entries or agent data), `--purge` /
`-Purge` (with `--uninstall`: additionally deletes the **default** agent
directory `~/.ready` after confirmation — refuses an active daemon or login;
explicit agent directories are never discovered or removed; asks unless
`--yes` / `-Yes`). Root (Unix) and elevated (Windows) invocations are refused:
READY installs per-user only. Pin a version with the `VERSION` environment
variable (the tag below is an example, not an upgrade recommendation):

```bash
curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | VERSION=v0.2.0 bash
```

Notes:

- The installer is idempotent and upgradable: re-running it replaces the
  program and the shipped skill library, **keeps your `config.yaml`** and never
  touches your runtime skill copies or session transcripts. End of every
  upgrade: a reminder to restart `ready serve` (`ready.exe serve` on Windows).
- Linux amd64/arm64 binaries are dynamically linked and require the glibc
  loader (`/lib64/ld-linux-x86-64.so.2`, `/lib/ld-linux-aarch64.so.1`):
  Alpine/musl, NixOS and distroless hosts are unsupported (the linux `arm/v6`
  build is statically linked and needs no loader — see the highlighted note
  above: a 2014 Raspberry Pi with an ARMv6 processor is enough to host it).
- The SHA256 verification of every download guards **integrity** (corruption,
  truncated downloads), not authenticity: the trust root is the GitHub
  repository and TLS.

After installation, open a new terminal and run `ready init` (`ready.exe init`
on Windows). The installer puts the example in program storage and never
creates or overwrites an agent configuration. A fresh default agent therefore
gets the full wizard and its starter defaults. Existing agent configurations
are kept and `init` offers offline repair for them.

## How READY. finds its config

An agent is identified by its **agent directory** — a physical directory that
holds exactly one `config.yaml` and all of the agent's private state. `ready
serve` and every `ready login` mode select it with the optional `--agent-dir
<directory>` flag; without the flag they always use `~/.ready`
(`%USERPROFILE%\.ready` on Windows), **regardless of the working directory**.
There is no cwd discovery, no merging and no alternative filename: a selected
directory must already exist and hold `config.yaml` — a missing directory or
config fails `serve` or `login` naming that path, with no fallback and no
auto-initialisation.

The agent directory is the root of every runtime path: `sessions/`, `skills/`,
any relative `logging.file_path`, and the agent's encrypted Secret store
(`secrets.age` + its key, see [Secrets](#secrets)). Configured writable paths must stay
inside the agent directory after resolving links; the shipped skill library
(below) is the one read-only program-content exception.

`ready serve` and `ready login` both announce the agent directory at start
(the daemon logs it at INFO and warns — never fails — when the config is
readable by group or others on unix; `login` prints `using agent directory
<path>` as its first line), so a login run against the wrong agent is visible
immediately. The interactive `ready chat` TUI shows the directory in its
startup info block, reported by the daemon itself after authentication.

New configurations use `config_version: 2`, independently of the READY release
version. Future schema upgrades run automatically in `ready serve`, with
validation, backup and atomic replacement. `login` never migrates config; an
older schema produces a same-directory `serve --agent-dir` command. See
[configuration versions and recovery](docs/config-versioning.md) for the
version rules, backup policy and migration-author checklist.

### Install layout

```
~/.local/share/ready/bin/ready       # the program body (Unix)
~/.local/bin/ready -> ../share/ready/bin/ready   # the launcher (Unix)
~/.local/share/ready/config.example.yaml # installed reference, not an agent config
~/.local/share/ready/library/skills  # the shipped skill library
                                     #   (installer-owned, program content)

~/.ready/                            # the default agent directory
├── config.yaml                      # sole config (created by ready init, 0600)
├── skills/                          # runtime skill copy (prepared by init;
│                                    #   kept current by the boot sync)
├── sessions/                        # session transcripts
├── secrets.age                      # encrypted Secrets + OAuth Credentials
├── secrets.key                      # the Store key (0600; see "Secrets")
└── oauth/                           # per-target coordination locks only
```

Windows installs `ready.exe` at `%USERPROFILE%\.local\bin\ready.exe` and the
library at `%USERPROFILE%\.local\share\ready\library\skills` — the agent
directory and its state are identical in shape.

The library is the installer's source of truth, replaced on every install;
your writes live in the runtime copies. `ready init` prepares the first copy
after saving the config, while still holding ownership. At boot `ready serve` syncs
`<agent dir>/skills/` from the library **per file** (three-way check against
a recorded state): files you never touched receive shipped fixes, your edits
are never overwritten (a conflict is left untouched and reported), your
deletions are respected, and machine-generated assets (`generated/`) are
always yours. The behaviour is one config key:

```yaml
skills:
  sync: auto         # auto (default) | notify (report only) | off
  # library_path: ../resources/skills/   # read-only source override; relative
                                         # paths resolve against the agent dir
```

Running two agents means two agent directories with distinct channel
resources (ZMQ binds, Telegram bot credentials): see
[agent directories](docs/agent-directories.md) for the full isolation
contract — locks, crash recovery, local OAuth and the exact
upgrade/uninstall/purge effects.

## Configure before first use (required)

For a new agent, use the deterministic terminal wizard first:

```sh
ready init [--agent-dir <directory>]
```

It asks for one provider/model with echo-disabled key input, creates the
agent's encrypted Secret store, its key and its identity (`agent.id`,
`agent.name`), generates the chat secret, and publishes `config.yaml` last —
the file holds references only. It rejects `--config` and never solicits
secrets in chat. Existing current-schema configurations can be repaired
offline with the same command; `ready init --new-identity` gives a COPY of an
agent directory its own independent identity.

For Ollama, choose `local` (default host `http://127.0.0.1:11434`, no key)
or `cloud` (default host `https://ollama.com`, API key required). The host
remains editable; READY appends `/api/...` itself. If the shipped
`ready-guide` library is absent, init still saves the config and warns; the
admin profile omits its skill injection until you install the library and
add `ready-guide` to `profiles.admin.inject_skills`. Refusals print their
reason on stderr.

The wizard explains each question and, after optional connection/model checks,
finishes with copyable commands for two terminals: keep `serve` running in
the first, then connect with `chat` in the second. Commands use `ready` from PATH on Linux/macOS and `ready.exe` on Windows,
with paths relative to the displayed working directory. Both terminals must
use that directory. Chat is shown with and without `--address`; custom
addresses require `READY_ADDR` for the variant without the flag. The setup
server command uses `--profile admin` to enable configuration management.
Only `serve` selects the profile; `chat` uses the active server profile and
has no `--profile` option.
It also explains how to stop the server and restart with the default profile.
`init` copies the installed shared skill library into the new agent;
`serve` keeps that copy current at startup, preserving local edits. A missing
library is reported; install or update READY and the next server start can
prepare the copy. A copy failure after config publication reports that the
configuration was saved, without deleting it.

New agents explicitly save editable defaults: JSON logs on stderr and in
`logs/ready.log` at INFO (`logging.level: info`), with daily/10MB rotation,
up to seven local calendar days of history and a 100MB total cap. Log files
receive dated names; size limits may remove older logs sooner. Skill sync
is `auto`, local skill edits reload live, and conversations use `sessions/`
with a daily 04:00 local reset boundary (checked on the next chat connection,
without interrupting an open chat). Ended conversations retain up to 30 days
within 200MB; automatic context summaries are enabled. These starter settings
can be changed through admin chat (a confirmed save activates by itself, see
*Post-save activation* below) or `config.yaml` followed by a server restart.
The wizard ends with a suggested first message to review the configuration
and personalize the agent one question at a time. Existing configurations
keep their settings during offline repair.

The daemon needs a configured LLM provider before the first `ready serve`.
The wizard above writes this configuration; to configure it by hand instead:

1. Open the agent's `config.yaml` (see the section below — usually the copy
   `ready init` created at `~/.ready/config.yaml`). For manual setup, create
   the agent directory and copy the installed `config.example.yaml` into it
   as `config.yaml` first. Replace the example `agent.id` with a fresh UUID
   before creating the Secret store; never reuse the example identity.
2. Edit the `llm:` block: declare at least one **provider instance**
   under `llm.providers` (type, wire `api`, `base_url`, credentials) and
   point `llm.default` at it as `<provider-instance>::<model-id>`, e.g.
   `"ollama-gemma::gemma4:31b"`. Every key is commented in
   [config.example.yaml](config.example.yaml), including the failover
   chain and the experimental ChatGPT/Codex provider
   (`ready login llm:<name>`).
3. Reference the shared secret of the `ready chat` channel:
   `channels.zmqcli.auth_token: !secret zmqcli_auth_token`. `zmqcli` is the
   default operator interface and turns on as soon as that reference exists.
   `ready serve` generates the value (64 hex characters) the first time it
   boots with the reference empty. Without the reference, and without another
   enabled channel such as Telegram, `ready serve` refuses to start.
4. Start (or restart) `ready serve`. Without a working provider the daemon
   boots in the `LLM unavailable` state and every request fails until a
   provider answers.

A `!secret` reference whose value is not set yet is *pending*: the daemon
derives that itself (it never depends on what a model wrote), boots degraded
for the unrelated components and starts the affected ones at the next
in-process runtime reload (automatic after the TUI collects the value) or
restart. A present ZMQ secret that breaks the 32-character rule, or a missing
external file for it, still refuses boot, with the exact `ready secrets set`
command. Values are entered in the authenticated ZMQ TUI under a profile
that exposes `config-manager` (e.g. `--profile admin`) on a numeric loopback listener (use SSH forwarding for
remote access; the editor opens by itself at the end of a turn that left
something pending), or offline with `ready secrets set <name>`. Select the admin
profile on `ready serve`, not on `ready chat`. The ZMQ protocol version is 3
and must match on both ends.

The RED editor masks the input (bullets and a character count); **Ctrl+R**
shows the current field, a value with leading/trailing whitespace is flagged
(it is stored exactly as typed) and a value containing a newline needs a
second RETURN. Escape skips a field; two Escapes or Ctrl+C/Ctrl+D return to
chat. `/secrets` lists every Secret with its state, `/secrets set <name>`
rotates any one and `/secrets rm <name> confirm` deletes an unused one. A new
chat secret is adopted by the next handshake; every other component activates
through the in-process runtime reload that follows a committed value.

**Post-save activation (no restart).** After a turn whose `config-manager`
save committed, on connecting with incomplete items, or after an on-demand
rotation, the TUI runs three phases without leaving the screen: (1) the RED
secret editor for every pending `!secret`; (2) an interactive OAuth login for
every `login_required` target (`llm:*` then `mcp:*` — the browser opens, the
URL is printed, paste the redirect URL/code and Enter, or Esc to skip); (3) an
in-process *runtime reload* (`reloading` in the status line, then
`── configuration reloaded ──`) that rebuilds the LLM orchestrator, MCP
servers, Home Assistant twin, Telegram channel, skills and tool catalog while
keeping the lock, the chat Connection and the conversation Session. A save
from Telegram, a remote ZMQ client or `--exec` reloads at the end of the turn;
pending secrets/logins then wait for the local TUI (or `ready secrets set` /
`ready login`). A failed reload keeps the previous runtime active.
`logging.*` and `channels.zmqcli` bind changes are the exceptions (the logger
needs a restart; a changed bind rebuilds the listener just after the reply).

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
your home. The config is read at boot — enabling it later activates
with the in-process runtime reload that follows a confirmed save.

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
| `READY_TOKEN` | fallback for `ready chat --token` (explicit flags and this variable win over the agent's own chat secret) |
| `READY_DEFAULT_PROFILE` | fallback for `serve --profile` (`default` / `edit` / custom) |
| `READY_LOG_LEVEL` | forces the log level of both sinks (`serve` only) |
| `READY_LOGO` | the `ready chat` logo presentation: `auto` (default) / `png` / `glyph` / `off` |
| `READY_SHIFT_ENTER` | force the SHIFT+RETURN out-of-band fallback `on`/`off` (auto by default; `Ctrl+J` / `Alt+Enter` always work) |

The former `READY_HOME_*` prefix is retired; there is no alias — rename any
launcher environment accordingly.

## Subcommands

- **`ready serve [--agent-dir <directory>] [--mock-mcp] [--profile <name>]`** — the long-lived
  daemon. `--agent-dir` is optional (default `~/.ready`, cwd-independent).
  `--mock-mcp` selects the in-process MCP mock for local development.
- **`ready chat [--agent-dir <directory>] [--address <addr>] [--token T] [--token-file F] [--exec MSG] [--reply-timeout 900s] [--raw]`**
  — the terminal client (Bubble Tea TUI, markdown rendering, usage and model
  status in the status line; `/session`, `/context` and `/secrets` are reserved commands).
  Start the server with `serve --profile admin` to manage configuration;
  the chat client has no profile flag. With no token flags, chat reads the
  selected agent's chat secret itself (read-only, no locks: `--agent-dir`,
  default `~/.ready`) and, without `--address`, uses `READY_ADDR`, else that
  agent's `zmqcli.bind`, else `tcp://127.0.0.1:7600`.
- **`ready login [mcp:<server>|llm:<provider>] [--agent-dir <directory>] [--status] [--forget] [--device-code]`**
  — standalone OAuth login for MCP servers and the `openai-codex` provider.
  The admin TUI also supports daemon-coordinated OAuth login. `--forget`
  deletes a credential. Login may run
  while the daemon serves the same agent directory: credentials coordinate
  cross-process and the running daemon picks up a fresh login without a
  restart (a server skipped at boot is connected by the next in-process
  runtime reload, which the TUI triggers right after its in-TUI login). Credentials are
  stored encrypted in the agent's Secret store, so upgrading from a release
  that kept them in `oauth/*.json` needs a new login per target.
- **`ready secrets list|set|rm|reveal|adopt|export-key|import-key|move-key|forget-key [--agent-dir <directory>]`**
  — manage the Secret store offline, alongside a running daemon (see
  [Secrets](#secrets)).

A removed flag fails loudly: `--config` is gone (use `--agent-dir`).

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
- `channels` — `zmqcli` (for `ready chat`) or `cli` (stdin) — mutually
  exclusive, zmqcli is the default: it turns on automatically as soon as a
  shared-secret reference (`auth_token`) is configured
  and no explicit `enabled` key is set; `telegram` is also implemented.
  The `serial` configuration slot is reserved for future implementation.
  Each channel may carry a `formatter_skill`. By default zmqcli binds to loopback
  (`tcp://127.0.0.1:7600`). Prefer SSH forwarding for remote access: HMAC
  authenticates the connection but does not encrypt the TCP traffic. Direct
  LAN access requires an explicit non-loopback bind and a trusted network.
- `mcp_servers` — Home Assistant (or any MCP server), Streamable HTTP by
  default, per-server tool prefixing, reconnect/health knobs, OAuth.
- `home_assistant` — the always-on digital twin behind the
  `ha_state_query` / `ha_command` tools (read/write discipline, effect
  verification, stale thresholds).
- `skills` — path, hot reload, `sync`, `library_path` (see above).
- `sessions` — per-address transcript files, reset rules (`daily`/`idle`),
  retention; `/new` and `/compact` are session commands.
- `compaction` — automatic context compaction with a summariser skill.
- `default_profile` + per-owner `profiles.<name>.{allow,deny}` — tool gating
  (fail-closed); `disclose_denied_tools` turns blocked calls into an explicit
  disclosure. Internal builtin tools follow the same model: when
  `builtin_tools.profiles` is absent, `config-manager` is denied under both
  built-in profiles and the other internal tools stay exposed — editing the
  elected configuration is an explicit opt-in through a named profile's
  `builtin_tools` rules, and profiles never inherit.
- **`config-manager`** — the tool-assisted configuration editor. An allowed
  session stages exact-match edits against the elected config, validates the
  candidate privately with boot semantics and commits only on an explicit
  save, with a backup and atomic replacement. The file holds references only:
  the tool refuses to write a literal into a sensitive field (it suggests the
  `!secret` name), a hand-edited literal is masked in what the model sees and
  blocks a save until `ready secrets adopt` moves it into the store, and
  `agent.id` is immutable. A confirmed save activates automatically through an
  in-process runtime reload (no restart; only `logging.*` needs one). A
  reference without a value is completed in the
  admin TUI's secret editor or with `ready secrets set`; the model never sees
  or writes a value.

## Secrets

`config.yaml` accepts only references in recognized sensitive fields — provider API
keys, the Telegram bot token, the ZMQ shared secret, the Home Assistant
token, an MCP OAuth client secret and every MCP header value. For example:

```yaml
api_key: !secret openai_api_key       # a name in the agent's encrypted store
# Alternatively, use an absolute file outside the agent directory:
# api_key: !secret_file /run/key
```

A literal in one of those fields is a configuration error.
The store is `<agent-dir>/secrets.age`
(age-encrypted, one atomically replaced file with a `.prev` generation) and holds
both your Secrets and the OAuth Credentials that `ready login` obtains. Its
key lives, by default, in `<agent-dir>/secrets.key` (`0600`). That protects a
store that travels *without* its key (a backup, a copied directory); it does
not protect against a process running as you.

`ready secrets` works offline and next to a running daemon:

| Command | Effect |
|---|---|
| `list` | names, referencing fields, state (`set`, `pending`, `unused`, `pending_external`), Credentials — never values |
| `set <name> [--stdin]` | hidden prompt (or stdin); value rules apply (the chat secret needs 32+ characters); stored exactly as entered |
| `rm <name> [--force]` | remove an unused Secret |
| `reveal <name> [--to-file F]` | the only command that outputs a value: a new `0600` file, or a terminal |
| `adopt` | move literals left in `config.yaml` by hand into the store (the previous file is kept as an encrypted backup) |
| `export-key` / `import-key` | carry the Store key between machines, passphrase-protected; import verifies it decrypts the store |
| `move-key --to agent_dir\|os_vault\|file` | relocate the key and update `secrets.store_key` |
| `forget-key --yes` | delete the vault entry (warns that any copy of this agent on the host shares it) |

The key can instead live in the macOS Keychain or Windows Credential Manager
(`secrets.store_key: os_vault`, keyed by `agent.id`; unattended service boots
cannot reach a user vault) or in a file READY only reads, such as a systemd
credential (`store_key: file`, `store_key_file: /run/credentials/…`). A missing
key with an existing store is always an error — READY never creates a new key
over an existing store (`ready secrets import-key` restores it). For a remote
chat client, copy the chat secret with
`ready secrets reveal zmqcli_auth_token --to-file F`.

**Upgrading** from a release that kept credentials in `config.yaml` or `*_file`
files is automatic: `ready serve` imports the values into the store, rewrites
the fields as references, seals the previous config as
`config.yaml.bak.<timestamp>.<n>.age` (restorable with
`age -d -i <agent-dir>/secrets.key`), deletes the imported plaintext files and
READY's earlier plaintext config backups, and adds `agent.id`/`agent.name`.
A config still holding a `REDACTED-…` placeholder is refused with its path.
Each OAuth target needs a new `ready login`. To roll back, decrypt the backup
over `config.yaml`, recreate any deleted `*_file` with
`ready secrets reveal <name> --to-file <original path>` and run the previous
binary.

Never send a credential in a conversational chat message: it lands in the
session transcript permanently. The `config-manager` tool only ever sees
references; secrets already typed into conversation, comments, URLs or
unclassified fields are outside what READY can protect.

## Documentation

- [Configuration reference](config.example.yaml): supported keys, defaults and examples.
- [Agent directories](docs/agent-directories.md): multiple agents, ownership, skill copies and backups.
- [Configuration upgrades](docs/config-versioning.md): schema migration and recovery.
- [Experimental Codex provider](docs/openai-codex-provider.md): authentication and limitations.
- [READY guide](resources/skills/ready-guide/SKILL.md): the same operator guidance
  the assistant loads in chat, including troubleshooting and timeout tuning.

## Development

Use the Go version and toolchain declared in `go.mod`. The LLM and Home
Assistant are external services; installing Go does not install either.

```bash
go build -o ready ./cmd       # or: make build
go run ./cmd serve            # uses ~/.ready (pass --agent-dir for another agent)
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

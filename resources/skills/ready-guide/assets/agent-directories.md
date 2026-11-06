# Agent directories — where READY. runs and keeps its state

An agent is a directory containing exactly `config.yaml` and its private
state. Every `ready serve` and every `ready login` mode selects it with the
optional `--agent-dir <directory>` flag; without the flag they ALWAYS use
`~/.ready` (`%USERPROFILE%\.ready` on Windows), independent of the current
working directory. There is no cwd discovery, no config merging and no
alternative filename.

## Directory layout and fixed paths

```text
<agent-dir>/
├── config.yaml           # references only — never a secret value
├── secrets.age           # encrypted Secrets + OAuth Credentials (+ .prev)
├── secrets.key           # the Store key (default location)
├── oauth/                # per-target coordination locks only
├── sessions/
├── logs/                 # when file logging is enabled
├── skills/               # writable personalised copy
├── .daemon.lock          # ownership lock file; persistent, NOT proof of liveness
└── .maintenance.lock     # maintenance gate (serve/login shared, purge exclusive)
```

- Credentials and secrets live in `<agent-dir>/secrets.age`; `oauth.path` and
  `oauth.token_store` are removed keys, rejected at load — one agent
  directory, one store. The Store key may instead live in the OS vault
  (`secrets.store_key: os_vault`) or an external file (`store_key: file`).
- Configured writable paths (`sessions.path`, `skills.path`,
  `logging.file_path`) must stay inside the agent directory after resolving
  symlinks/junctions; an escaping destination fails at load, naming the key.
- The shipped skill library lives OUTSIDE every agent directory (see
  `install-upgrade.md`) and is the only program-content exception to that
  rule.
- `!secret_file /absolute/path` references are read-only inputs outside the
  agent directory, not agent state (a path inside the agent directory is
  refused: use `!secret`). The `*_file` keys no longer exist.
- Copying a whole agent directory copies its identity: give the copy its own
  with `ready init --agent-dir <copy> --new-identity`.

Generated agent names use the host name, truncated to 64 Unicode characters,
or `ready` if unavailable. Explicit `agent.name` values over 64 characters
are rejected rather than silently shortened.

## Running and selecting

Only `ready init` creates a missing agent directory. The installer places
config.example.yaml in program storage and never pre-creates an agent config. For Ollama it asks
local/cloud, proposes an editable host (`https://ollama.com` for cloud),
and requires a private cloud API key. Missing shared `ready-guide` content
warns and skips admin skill injection while allowing the config to be saved.

```sh
ready init --agent-dir /path/to/agents/casa
ready serve --agent-dir /path/to/agents/casa
ready login mcp:home_assistant --agent-dir /path/to/agents/casa
ready login --agent-dir /path/to/agents/casa --status
```

An explicit missing directory or config is an error naming the path; no other
agent is adopted and nothing is initialised automatically. Supported
environment overrides (`READY_DEFAULT_PROFILE`, `READY_LOG_LEVEL`, …) keep their
documented precedence and never bypass the confinement.

## Two agents

Prepare two existing directories, each with a valid config.yaml, and give
them nonconflicting channel resources: distinct ZMQ binds (e.g.
`tcp://127.0.0.1:7600` vs `tcp://127.0.0.1:7700`), distinct Telegram bot
credentials, deliberate routing of exclusive serial devices. Then:

```sh
ready serve --agent-dir /path/to/agents/casa
ready serve --agent-dir /path/to/agents/ufficio   # separate terminal/service
ready chat --agent-dir /path/to/agents/casa
```

Each agent keeps independent transcripts, skills and OAuth records. The
interactive `ready chat` startup block shows the daemon-reported directory,
so a client always knows WHICH agent it is talking to:

```text
daemon     tcp://127.0.0.1:7600
agent-dir  ~/agents/casa
session    <session UUIDv7>
client     <client version>
```

`~` in that display is the daemon user's home — a remote client never
substitutes its own path, and the directory is not conversation history.

## Ownership and recovery

Only one daemon owns an agent directory: a second `ready serve` fails
immediately — before config migration, logging, sync, retention or network
setup — even with different ports, another profile, or ZMQ/file logging
disabled. Alias spellings and links of the same physical directory contend on
the same lock. When a process dies, the OS releases the lock; the stable lock
files stay on disk and are never proof of liveness — do not delete them to
"unstick" a start, just run `ready serve` again.

`ready chat` and `ready login --status` are always available: status writes
nothing (no store, no lock files, no network).

## Login alongside a running daemon

Mutating login (browser login, refresh, forget) may run while serve holds the
directory: per-target cross-process coordination serializes credential
writes, and an interactive login that raced a concurrent change reports a
conflict instead of overwriting. A fresh login is picked up by an already
connected MCP target without a restart; a server skipped at boot is
connected by the next in-process runtime reload (or a restart). Forgetting a credential removes the LOCAL record — it is not a
provider-wide logout and does not cancel a request already sent with a token.

Callback ports: `oauth.redirect_port` applies only to MCP authorization-code
login (sequential logins may share it; concurrent listeners need distinct
ports — a busy port errors clearly). Codex browser login keeps its fixed
`localhost:1455` callback; device authorization opens no local listener.

## Skill copies and conflicts

Every agent syncs its own runtime `skills/` copy from the shared, installer-
owned library at boot, per file and three-way. Personalise the AGENT's copy;
never edit the shared original. On a conflict (your edit + a shipped change)
the agent keeps your bytes and reports both file paths — compare versions and
decide deliberately. Operator-deleted files stay deleted; operator-added and
`generated/` files are never touched by the sync. See `install-upgrade.md`
for the exact upgrade/uninstall/purge effects.

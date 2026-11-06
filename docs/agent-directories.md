# Agent directories and shared skills

## Choose the agent before editing configuration

An agent is a directory containing exactly `config.yaml` and its private state.
Choose it explicitly:

```sh
ready init --agent-dir /path/to/agents/casa
ready serve --agent-dir /path/to/agents/casa
ready login mcp:home_assistant --agent-dir /path/to/agents/casa
ready login --agent-dir /path/to/agents/casa --status
```

Without `--agent-dir`, serve and every login mode use `~/.ready/config.yaml`,
regardless of the current working directory. On Windows, `~` in these examples
means `%USERPROFILE%`. An explicit missing directory/config is an error; no
other agent is selected and no directory is initialised automatically. Only
`config.yaml` is accepted; the old `--config <file>` option is removed.

Only `ready init` creates a missing agent directory. The installer places
config.example.yaml in program storage and never pre-creates an agent config. For Ollama it asks
local/cloud, proposes an editable host (`https://ollama.com` for cloud),
and requires a private cloud API key. Missing shared `ready-guide` content
warns and skips admin skill injection while allowing the config to be saved.
Its deterministic terminal wizard writes one provider, Secrets into the encrypted store, the Store key and
agent identity, then publishes `config.yaml` last,
with no credential TODOs. It offers bounded offline repair for supported
current-schema configurations and refuses to modify an active agent. Start
`serve --profile admin` explicitly for guided configuration; ordinary startup
uses the configured default profile.

Existing supported environment overrides remain in effect. For example,
`READY_DEFAULT_PROFILE` still participates in profile selection and
`READY_LOG_LEVEL` still controls effective logging levels. These overrides do
not change ownership or allow writable state outside the selected directory.
Do not assume every YAML field has an environment-variable counterpart.

```text
agents/casa/
├── config.yaml           # references only — never a secret value
├── secrets.age           # the encrypted Secret store (Secrets + OAuth Credentials)
├── secrets.age.prev      # the previous generation (read fallback)
├── secrets.key           # the Store key (default location; 0600)
├── oauth/                # per-target coordination locks only
├── sessions/
├── logs/                 # when file logging is enabled
├── skills/               # writable personalised copy
├── .daemon.lock          # ownership lock file; persistent, not proof of liveness
└── .maintenance.lock     # maintenance gate (serve/login shared, purge exclusive)
```

The layout shows the main user-facing entries; internal coordination/config
backup files may also be present. `oauth.path` and `oauth.token_store` are
removed keys and are rejected at load. Session/log/skill path overrides must stay
inside the directory, including after links are resolved. Keep secret values
out of chat.

### Secrets and the Store key

Every sensitive field of `config.yaml` takes `!secret <name>` (a name in
`secrets.age`) or `!secret_file <absolute path>` (a file outside the agent
directory, used byte for byte except one trailing newline); a literal in a recognized sensitive field is a
configuration error. Review comments, URLs, custom fields and device metadata
before sharing configuration or backups; arbitrary text is not scrubbed. The agent
directory therefore contains everything but is only as private as its
`secrets.key`: **with the default `agent_dir` key location the encryption
protects a store that travels without its key (a backup, an archive, a
`config.yaml`-only copy), never against another process running as the same
user.** For more, move the key out of the directory with
`ready secrets move-key`:

- `--to os_vault`: macOS Keychain (via `/usr/bin/security`) or Windows
  Credential Manager, entry named after `agent.id`. Not available on Linux,
  and an unattended service boot without a user session cannot reach a user
  vault — use `agent_dir` or `file` for service-mode daemons.
- `--to file --path /abs/key`: a file READY only reads, for example a systemd
  credential. Setup: `systemd-creds encrypt` the output of
  `ready secrets export-key --raw` to a credential file, load it with
  `LoadCredentialEncrypted=ready-store-key:/etc/ready/store-key.cred` and set
  `secrets.store_key: file` plus
  `store_key_file: /run/credentials/ready.service/ready-store-key`.

A store whose key is missing is an error, never a reason to create a new key:
restore it with `ready secrets import-key` (a passphrase-protected
`export-key` file, or a bare key). Values, including whitespace, are stored and
used exactly as entered; the chat secret must be 32+ characters.

A reference with no usable value is *pending*. The daemon derives that itself:
the affected components stay off until the next in-process runtime reload
(or restart), unrelated ones start, and an
absent chat secret is generated at boot. A present chat secret that breaks its
rule, or a missing external file for it, refuses boot with the exact
`ready secrets set` command. Complete pending values in the authenticated admin
ZMQ TUI's masked editor on numeric loopback (SSH forwarding for remote access)
or offline with `ready secrets set`. A new chat secret is adopted by the next
handshake; other components start with the runtime reload the TUI triggers
after a committed value.

### Agent identity, copies and clones

`agent.id` is a required, immutable UUID; it is bound into `secrets.age` (a
config whose id differs from the store's refuses boot, naming both) and names
the vault entry. `agent.name` is a free label (host name by default) shown in
the chat info block and status line.

Copying the whole directory yields a second agent with the **same** id, key
and tokens (and, for `os_vault`, the same vault entry). Give the copy its own
identity with `ready init --agent-dir <copy> --new-identity`. `forget-key` on
one of two agents that share a vault entry removes the key of both.

## Run two agents

Prepare two existing directories, each with its own valid config.yaml. Set
nonconflicting listeners, for example casa on `tcp://127.0.0.1:7600` and
ufficio on `tcp://127.0.0.1:7700`. Each agent has its own chat secret. Choose channel credentials deliberately;
the same Telegram bot or exclusive serial device is not automatically split
by using another directory.

```sh
ready serve --agent-dir /path/to/agents/casa
ready serve --agent-dir /path/to/agents/ufficio
```

Run these in separate terminals/services. On the same machine, name the agent
and chat finds its address and chat secret itself:

```sh
ready chat --agent-dir /path/to/agents/casa
ready chat --agent-dir /path/to/agents/ufficio
```

From another host, copy the secret with
`ready secrets reveal zmqcli_auth_token --agent-dir … --to-file F` on the daemon
host and connect with `--address … --token-file F`.

Chat remains a client: it reads only `config_version`, `channels.zmqcli`,
`agent.id` and `secrets` of the selected agent plus that one Secret —
read-only, no locks, nothing written — and takes no daemon ownership.
Its interactive startup information includes:

```text
daemon     tcp://127.0.0.1:7600
agent      casa (<agent.id>)
agent-dir  ~/agents/casa
session    <session UUIDv7>
client     <client version>
```

The directory comes from the authenticated daemon. A remote client sees the
daemon's path; `~` refers to the daemon user's home. This operator information
is not a conversation turn/history record and does not add lines to raw, exec
or piped reply output.

## Ownership, login and recovery

Only one daemon may own an agent directory. A second start fails immediately —
before config migration, logs, skill sync or network setup — even with
different ports, a different profile, disabled ZMQ or disabled file logging.
Different spellings/links to the same physical directory do not create another
agent: they contend on the same ownership lock. The OS releases ownership when
a process dies; do not delete lock files to restart — the file is stable and
carries no meaning by itself.

`ready chat`, `ready secrets` and `ready login --status` stay usable at any
time: chat is a read-only client, `secrets` takes only the shared maintenance
gate and the short store lock, and status writes nothing (no store, no lock
files, no network).
Login that mutates credentials may run while serve is active. Credential
updates, refresh rotation and forget are coordinated per target: an
interactive login whose stored credential changed meanwhile reports a conflict
rather than overwriting newer state — inspect status and retry deliberately.
Forget prevents older work from restoring that local generation. It concerns
local records: it is not a provider-wide logout and does not cancel a request
already sent with a token.

New credentials can recover an already connected MCP target. A target omitted
from the boot tool catalog exposes its tools at the next in-process runtime
reload (the TUI triggers it after an in-TUI login) or a daemon restart.

For MCP authorization-code login, set `oauth.redirect_port` before first
registration. Sequential logins can reuse the same port; simultaneous
listeners need different ports — a busy port is a clear error, never a silent
fallback. Changing the port does not update a stored registration: follow the
mismatch error — explicitly forget and re-register a dynamically registered
client, or update the allowed redirects of an operator-registered client at
the authorization server. Repeating login alone does not repair the old
registration.

Codex browser login uses its fixed `localhost:1455` callback; the MCP port
knob does not change it. Codex device authorization needs no local callback
listener. OAuth Credentials are local to each agent's Secret store; identical copied
tokens or identical remote accounts may still have provider-side coupling.
Directory isolation is not a same-user OS sandbox.

## Shipped skill originals and personal copies

The installer owns a per-user library outside all agent data:

- Unix: `~/.local/share/ready/library/skills`.
- Windows: `%USERPROFILE%\.local\share\ready\library\skills`.

It contains the original skills distributed with READY, including ready-guide.
It must contain no credentials, private generated topology or agent-specific
personalisation. Every agent has its own writable runtime copy under its
selected directory. Explicit read-only source overrides are for separate
library content; never point at another agent's private copy or at the
destination itself.

Fresh init prepares the first skill copy after saving configuration, under
the same ownership lock. A copy failure keeps the saved config and reports
that serve will retry. At agent boot, per-file three-way synchronization reconciles the selected
runtime copy with the library and its recorded sync state:

| Situation | Result in automatic mode |
|---|---|
| New shipped file | Copied locally |
| Untouched local file, changed original | Local file updated |
| Personalised local file, changed original | Local edit kept; conflict reported |
| Previously synced file deleted locally | Stays deleted |
| Operator-added or generated file | Remains local and untouched |
| Shipped file removed upstream | Local orphan retained and reported |

`skills.sync: notify` reports without writing; `off` skips synchronization.
A manual binary-only deployment without a library keeps its existing runtime
copy; an absent library is not itself an error. One installation supplies one
shipped library version; agents do not pin independent original versions.

Edit the chosen agent's copy when personalising a skill. Compare both versions
when a conflict is reported; do not fix a conflict by overwriting every agent
or editing the installer-owned shared original. READY never writes generated
or personal files back into that library.

## Upgrade, uninstall, purge, move and copy

The program is installed separately from every agent: Unix body at
`~/.local/share/ready/bin/ready` with the relative launcher
`~/.local/bin/ready`; Windows exe at `%USERPROFILE%\.local\bin\ready.exe`.
Reinstall replaces the program and distributed library, keeps every existing
config and private copy, and asks for a restart. Each agent reconciles shipped
updates on its next boot.

Installers are per-user and refuse root (Unix) or elevated (Windows)
execution. PATH is configured conservatively for your login shell;
`--no-modify-path` (`-NoModifyPath`) opts out. Manual instructions are printed
even when PATH is unchanged. Shared PATH entries survive removal.

- Uninstall removes the owned program, launcher and shipped library,
  preserving every agent's data and PATH.
- Confirmed uninstall with purge additionally removes only the default
  `~/.ready` agent, after taking the directory's exclusive maintenance gate:
  an active daemon or mutating login makes combined removal fail BEFORE
  deleting program, library or private state. The config is removed first so
  no new daemon can boot against a recreated lock with an obsolete snapshot.
- Explicit agent directories are never discovered or purged by the installer.
- Deleting only inactive default-agent data leaves the program and library
  intact; the next `serve` without a config fails naming `config.yaml`.
- Moving an inactive complete agent directory preserves its local state
  (history, usage, credentials) subject to ordinary provider validity.
- Copying only `config.yaml` starts with no inherited transcript, store or key,
  and no secret value; its `!secret` references are pending until set, and its
  `!secret_file` references still point at the original files — review them.
- Copying the whole directory copies the store, its key and the tokens too; it
  does not create an independent identity (use `ready init --new-identity`) or
  guarantee safe parallel refresh of the copies.
- Uninstall with purge first asks `ready secrets forget-key --yes` to delete
  the default agent's vault entry, best-effort, then removes the directory.

# Guided configuration editing with `config-manager`

This asset describes the tool-assisted way to change the selected agent's
configuration: the agent conducts the discussion (as in the config
interview), then stages the edits mechanically with the `config-manager`
tool. The configuration file holds **references only** — never a secret value
— and nothing is written until you confirm.

## The assisted flow (read → discuss → update → confirm → save)

1. **read** — the agent opens staging: it receives the configuration text, a
   `stage_id` and a `revision`. Sensitive fields appear as references such as
   `api_key: !secret openai_api_key`. The stage is owned by the current
   Session: another Session's actions are refused.
2. **discuss** — the agent asks what to change, exactly as in the config
   interview protocol.
3. **update** — the agent applies a batch of exact `old_text` → `new_text`
   edits against the staged text and gets back a diff and the new revision.
   A batch is atomic: any refused edit commits nothing.
4. **confirm** — the agent shows you the diff and asks you to confirm before
   saving. This confirmation is a procedural rule of the guided flow, not a
   technical authorization: the tool itself never asks for a human
   approval token.
5. **save** — the tool validates the candidate with the same rules boot uses,
   writes a backup of the current file and replaces it atomically.
   **Activation is automatic** (post-save activation): in the local
   loopback `ready chat` TUI the red secret editor opens for pending
   `!secret` names, then interactive OAuth logins run in the TUI (browser
   opens, or paste the redirect URL/code, Esc skips one), then the daemon
   reloads its runtime in-process — same Session, same Connection, no
   restart. On Telegram, remote ZMQ or `--exec` the runtime reloads at the
   end of the turn; pending secrets/logins wait for the local TUI.

Always tell the user the save outcome, naming the selected agent's config
path. Before save, explicitly say the proposed/staged changes are not saved
and request confirmation. After save, use the tool's reported outcome:

- Committed: confirm the configuration is saved and activates automatically
  (except logging changes, which require a restart); say which new `!secret` names still need a value and
  which OAuth logins are pending, and where they are completed (TUI prompt at
  the end of the turn, or `ready secrets set` / `ready login`).
- Committed with warnings: confirm it was saved, report the warnings and
  next step, and do not retry the save.
- No changes: say there were no changes to save.
- Refused or failed before commit: say the changes were not saved and
  explain the next step. If the tool is unavailable, identify the file the
  user must edit/save manually; never imply a shown YAML block was written.

## How secrets are handled (no secret in chat, none in the file)

Every sensitive field — provider `api_key`, the Telegram `token`, the ZMQ
`auth_token`, the Home Assistant `websocket.token`, an MCP `oauth.client_secret`
and every MCP `headers` value — takes only

- `!secret <name>`: a name in the agent's encrypted store (names are
  `[a-z0-9_]`, up to 64 characters; use a descriptive one such as
  `openai_api_key`, `telegram_token`, `mcp_ha_authorization`), or
- `!secret_file /absolute/path`: a file outside the agent directory (for
  example a systemd credential).

**Never ask for, repeat or write a secret value** — it would land in the
transcript permanently. The decision rule that makes this work:

> **A block that needs a secret is COMPLETE the moment it carries the
> reference.** Stage and save it immediately — do NOT wait for a value,
> do NOT reply "mi manca solo il token" / "send me the token" /
> "aspetto il token". The correct closing sequence is: stage the block
> with the reference → show the diff → obtain explicit confirmation → save → tell the user the change
> activates by itself. The secret editor (red area) then opens by itself at
> the end of the turn and asks for the value, masked, outside the chat;
> the runtime reloads in-process right after.

Write the reference; the value is entered by the user:

- in the ZMQ TUI's **secret editor**, which opens by itself at the end of a
  turn that left a `!secret` name without a value (a profile that exposes
  `config-manager`, e.g. admin; numeric loopback listener; remote operators
  use SSH forwarding). The input is
  masked; Ctrl+R shows the current field, Escape skips it. `/secrets` lists
  every secret with its state, `/secrets set <name>` rotates one,
  `/secrets rm <name> confirm` deletes an unused one; or
- offline, with `ready secrets set <name>` (hidden prompt, or `--stdin`).

The tool refuses a literal in a sensitive field and suggests the reference to
write instead (never echoing what was typed). A literal that a hand edit left
in the file is masked in what you see and blocks a save: tell the user to run
`ready secrets adopt` (it moves the literals into the store and leaves an
encrypted backup). `agent.id` cannot be changed. `channels.zmqcli.auth_token`
changes are accepted only from a ZMQ Session.

A reference without a value is *pending*: the daemon derives that itself and
boots the unrelated components normally; the affected component starts at
the next in-process runtime reload (automatic once the value is entered in
the TUI) or restart. The chat secret is the exception: if its reference exists
and the value is absent, `ready serve` generates it; a value that exists but
is shorter than 32 characters (or a missing `!secret_file`) refuses boot with
the exact `ready secrets set` command. A new chat secret is adopted by the
next handshake without a restart; remote clients need a fresh copy
(`ready secrets reveal zmqcli_auth_token --to-file F`).

### What lives in the store, and what removes what

`ready secrets list` shows three kinds of entries (never values, with `--agent-dir`
for a non-default agent):

1. **Secrets** — values referenced by config.yaml as `!secret <name>`. Remove
   one by editing the configuration first, then `ready secrets rm <name>`
   (refuses while the config still references it — `--force` overrides, and
   the TUI writes it as `/secrets rm <name> confirm`).
2. **Credentials** — OAuth records managed by `ready login` (`mcp:<server>`
   for MCP servers, `llm:<provider>` for `openai-codex`). `ready secrets rm`
   cannot delete them (they are not in the Secrets map); the only remover is
   `ready login <target> --forget`, which requires the target to still be in
   the elected config.
3. **The Store key** — the encryption key of the store itself, held in
   `agent_dir`, in the OS vault, or in an external file (`secrets.store_key`).
   Its commands act on the key, not on entries: `export-key / import-key /
   move-key` for backup and migration, and `ready secrets forget-key` deletes
   ONLY the OS-vault entry holding the key (never the store or its entries;
   a warning reminds that same-host copies of the key allow anyone to open
   the store).

## Always name the follow-up login for OAuth components

**Ask at add time.** When staging a NEW component, ask whether it
authenticates via OAuth before saving: for `mcp_servers.<key>` entries
(enterprise/SaaS endpoints usually are — a `401` from the bare server is
the tell), if yes stage the `oauth:` block (typically `grant: auto`) in
the same edit — it is mutually exclusive with `headers.Authorization` and
unavailable on `transport: sse`.

When a staged/saved component authenticates via **OAuth** — the
`openai-codex` provider, or any `mcp_servers.<key>` entry with an `oauth:`
block — the configuration alone is NOT enough: ALWAYS end the proposal by
naming the exact follow-up command, in its own step separate from the
save/restart status:

- provider: `ready login llm:<instance-name>` (e.g. `ready login
  llm:codex-work`)
- MCP server: `ready login <server-key>` (or `ready login mcp:<server-key>`)
- non-default agent: append the quoted `--agent-dir <dir>` the tool prints

Never present the configuration as complete without it; remind that the
component's tools/model become available only after the login succeeds
(the TUI runs the login for you after a save — in the browser, without
leaving the chat — and reloads the runtime afterwards).

### Activation after a config-manager save

A confirmed save automatically activates reloadable settings. Do not ask
for a restart to load a new MCP server or provider. `logging.*` is the
exception: its changes require a daemon restart. A changed ZMQ bind/enabled
state rebuilds the listener after the reply and may require reconnecting.
A failed runtime reload keeps the previous runtime active: report the failure
separately from the successful file save. Normal activation paths are:

1. **Local loopback `ready chat` TUI, active profile exposes
   `config-manager`**: the login is AUTOMATIC — at the end of the saving
   turn the TUI opens the browser flow for every `login_required` target
   (after the RED secret editor, if any names are pending), then reloads
   the runtime in-process and the server's tools appear. Naming the
   `ready login` command is still required by the rule above (it is the
   out-of-band fallback), but present it as the fallback, never as the
   primary user action when they are in the TUI.
2. **Other channels (Telegram, remote ZMQ, `--exec`), or the operator
   saving from elsewhere**: the runtime reloads at the end of the turn;
   the pending OAuth login waits — completing it later with the named
   `ready login <target>` from a terminal (or the local TUI) picks it up.
   Only when the server was skipped at boot WITHOUT the credential (the
   reload of the saving turn happened before any login) do its tools need
   the NEXT in-process runtime reload or a daemon restart to appear —
   state that as a one-shot consequence of standalone login, not as a
   post-save step.

If the operator reports that the in-TUI login did not open after a save,
the likely causes are the gating conditions, all worth stating: they were
not on the loopback `ready chat` TUI, the active profile does not expose
`config-manager`, the save happened on a non-TUI channel, or the target
was not derivable as `login_required` at that moment — each ends in path
2 above, still without a restart of a freshly saved configuration.

## What the tool protects — and what it does not

The file contains no secret, so staging text, diffs, backups and logs cannot
leak one. Not protected (stated honestly): text you already typed into the
conversation, secrets in comments, URLs or unknown fields. The tool cannot
retroactively clean a transcript.

## Staging recovery

- `read` again as the owning Session returns your current staged state —
  edits and diff context intact.
- If the file was changed externally (by you or another writer) between read
  and save, save refuses and marks the staging conflicted: discard the
  staging, `read` fresh and reapply your intent. Nothing is overwritten on
  conflict.
- Ending the Session (`/new`, a reset rule) releases staging; a ZMQ client
  reconnect alone does not. Discarding releases it without touching the
  file.
- Repeating a just-completed save returns the same result without writing
  again.
- Entering secret values never competes with staging: it writes the store, not
  the file.

## Profile visibility

`config-manager` is an internal builtin tool: by default (when
`builtin_tools.profiles` is absent) it is **denied** under both `default`
and `edit` profiles and only the other internal tools are exposed. To use it,
declare a profile (e.g. `admin`) with a `builtin_tools` allow rule that
includes it — and remember profiles never inherit: every other policy owner
needs its own rules for that profile or contributes nothing under it.

## There are no `*_file` keys any more

`api_key_file`, `auth_token_file`, `token_file` and `client_secret_file` are
removed from the schema (a config using them is refused with a pointer to
`!secret_file`). Every sensitive field, including the Telegram token and each
MCP header value, takes `!secret` or `!secret_file`.

## Small local models: keep the guide in context

Progressive disclosure relies on the model choosing to load this guide
before answering config questions — strong hosted models do; small local
models (gemma-class) often don't, and improvise block shapes. When the
operator's admin profile runs on a small local model, recommend injecting
the guide into every admin session:

```yaml
profiles:
  admin:
    inject_skills: ["ready-guide"]
```

The SKILL.md map (interview order, per-domain asset names) is then always
in context, so the model reads the right `assets/config-*.md` before
staging. Cost: the guide map sits in every admin turn (~2–3k tokens); for
tiny Ollama context windows pair this with an explicit `context_window`
and matching `options.num_ctx`.

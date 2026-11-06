# Home Assistant: MCP servers, the digital twin, profiles, OAuth

## What "add Home Assistant" means (decision rule)

When the operator asks to add Home Assistant **without qualifying**, the
expected proposal is the **digital twin**: enable the `home_assistant:`
block over WebSocket. Concretely:

```yaml
home_assistant:
  enabled: true
  websocket:                          # NESTED under websocket — never flat
    url: ws://homeassistant.local:8123/api/websocket
    token: !secret home_assistant_token   # reference; value via the red /secrets area
  profiles:
    default:
      allow: ["ha_state_query", "ha_command"]
    admin:
      allow: ["ha_state_query", "ha_command"]
```

- The twin is the contractual **read SSOT** with verified commands
  (`ha_state_query` reads, `ha_command` writes with per-entity outcomes);
  propose it by default and then run the per-profile interview below.
  Both `websocket.url` and the MCP `url` are **secret-capable plain
  fields**: a literal stays in clear; `url: !secret <name>` resolves at
  boot and the value is collected by the red area like any secret. The
  operator decides per instance — never force either form.
- The HA **MCP server** is a separate, explicit step: propose it only when
  the operator asks for it (its read tools are denied under `default`
  anyway — the twin supersedes them).
- There is NO `twin:` key and NO flat `home_assistant.url`: those shapes
  are refused by the strict decoder, and the refusal names the offending
  field.

Two independent integrations exist, each with its own policy:

1. **MCP servers** (`mcp_servers:`) — classic MCP tool clients; one entry
   per server, connected at boot, all aggregated behind one composite
   runtime with "any" semantics (connected when at least one is live).
2. **The digital twin** (`home_assistant:`) — an always-on WebSocket that
   mirrors every entity state and registry in memory. It serves reads
   (`ha_state_query`) and verified writes (`ha_command`) on the same
   connection. It does NOT cross-check whether the MCP HA server is on:
   enabling one says nothing about the other.

## mcp_servers.<key>

Keys per server: `url`, `transport` (`streamable_http`, the default and the
one Home Assistant requires; `sse` legacy), `headers` (map; the bootstrap
Bearer token case), `timeout` (~30s bounds per request), `tool_prefix`,
`expected_tools`, `reconnect.*`, `health_check.*`, `oauth`.

Behaviour worth knowing:

- A server unreachable at boot is skipped with a WARN; the daemon continues
  with the other servers, and the skipped server's tools are absent for the
  current runtime generation. A later runtime reload rebuilds the catalog
  and can connect the server; an ordinary reconnect does not add missing tools.
- Tool names as seen by the LLM carry the configured prefix
  (`<prefix>__<wire_name>`; absent prefix = server key, `""` = no prefix).
- `headers` are applied only inside the configured server's own origin
  (same host or subdomain, and never an https→http downgrade). A cross-host
  redirect would NOT receive the header — a server-controlled redirect
  cannot exfiltrate the credential.
- `expected_tools` is a warn-only sanity check on the server's *wire* names
  (HA versions differ; a missing expected tool warns instead of failing).
- `reconnect`: `max_retries 0 = infinite`, exponential backoff; reconnect
  re-lists tools and logs catalog drift.
- `health_check.enabled` (default true) pings at `interval` (~30s); pings
  count errors, and an authorization failure state is logged once per
  transition rather than once per tick.
- The transport's own retries are disabled; the reconnect config above is
  the only retry authority.

### MCP OAuth (opt-in per server)

**At add time — ask before staging.** When adding a NEW MCP server, ask
whether the endpoint is OAuth-protected (enterprise/SaaS MCP endpoints
usually are; a `401` from a bare server is the tell). If yes — or the
operator says so without declaring it in config — stage the `oauth:` block
in the same edit, and ALWAYS close the proposal with the follow-up login
command (see `assets/config-manager.md`): `ready login <server-key>` (or
`ready login mcp:<server-key>`, with the quoted `--agent-dir` for a
non-default agent). The first login runs discovery, one-time Dynamic
Client Registration when needed, and persists the credential; an `oauth:`
block added while the daemon runs is activated by the post-save flow: the
local TUI runs the login (browser) and reloads the runtime in-process, so the
server's tools appear without a restart.

```yaml
mcp_servers:
  acme:
    url: https://acme.example.com/mcp
    oauth:
      grant: auto          # auto | authorization_code | client_credentials
      # client_id: "registered-client-id"
      # client_secret: !secret acme_client_secret  # or !secret_file /abs/path
```

- `oauth` and `headers.Authorization` on the same server are mutually
  exclusive; `oauth` + `transport: sse` is a boot error.
- There is no scopes key: the authorization server decides.
- Credentials persist as one encrypted entry per server in the agent's
  Secret store (`<agent-dir>/secrets.age`; `oauth.path` and
  `oauth.token_store` are removed keys). A release that kept `oauth/*.json`
  records needs a new `ready login`.
- First-time auth happens through `ready login [mcp:]<server>` (browser);
  `--status` checks, `--forget` deletes.
- **Runtime recovery without restart**: authorization failures do not tear
  the session down. Running `ready login <server>` while the daemon is up
  is enough — the next tool call or health ping picks up the fresh
  credential. A server skipped at boot (no credential yet) exposes its tools
  at the next in-process runtime reload (automatic after the TUI login) or
  restart. For a
  non-default agent use the printed command, which carries `--agent-dir`.
- `grant: client_credentials` is autonomous (no stored credential needed);
  `authorization_code` requires a once-per-credential login.
- **Credentials outlive the server section** (`mcp:<server-key>` in the
  agent's Secret store): removing `mcp_servers.<key>` does NOT delete its
  credential — the entry becomes orphaned, still visible in `ready
  secrets list` under CREDENTIALS (but not in `ready login --status`,
  which only lists targets present in the elected config). Re-adding a
  section with the SAME key reuses the stored credential AND the client
  registration — no new OAuth is asked, and an expired-but-refreshable
  token refreshes at the first tool call or health ping. `ready login
  --forget mcp:<key>` requires the target to exist in the elected config, so
  to scrub an orphan: re-add the section, run the `--forget`, then re-remove
  it. A rejected/expired refresh token is the only thing that reopens a
  browser: the daemon surfaces it as login-required and the next `ready
  login <server>` (with the section present) fixes it.
- **No concurrent removal of the credential**: the config-manager only ever
  writes config.yaml, never the Secret store, so a save cannot delete a
  credential in the same edit — removing a section and scrapping its
  credential are always two separate steps. The section's per-profile
  tool gates need no separate cleanup: they live inside the section and
  disappear with it.
- **First OAuth-capable component**: when this is the first `oauth:` block
  (or the first OAuth-capable twin/provider component) entering the
  configuration, also stage the top-level `oauth:` section in the same
  diff:

  ```yaml
  oauth:
    redirect_port: 7601   # fixed loopback callback port for `ready login`;
                          # frozen into the persisted client registration,
                          # so keep it stable across logins (adjacent to
                          # zmqcli's 7600)
  ```

  Requesting it explicitly keeps the operator aware of the port before
  their first browser login; later servers share the section.

## The digital twin (home_assistant:)

```yaml
home_assistant:
  enabled: true
  websocket:
    url: ws://homeassistant.local:8123/api/websocket
    token: !secret home_assistant_token   # or !secret_file /abs/path
  stale_threshold_states: 60s       # soft-stale marker
  hard_stale_threshold: 300s        # refuse-to-serve gate (reads AND writes)
  outstanding_pings_threshold: 2    # unacked pings before force-reconnect
  commands:
    verify_timeout: 5s
    max_targets: 20                 # broader selectors are refused
    # domains: [light, cover]       # absent = everything except siren
```

Store only the raw long-lived token, without a `Bearer ` prefix or quotes
(`ready secrets set home_assistant_token` takes it from a hidden prompt; the
value is stored exactly as entered, so a stray space or newline would be sent to
HA as part of the token and cause `auth_invalid`). An external
`!secret_file` has one trailing newline removed. After correcting the secret,
restart `ready serve`: invalid authentication stops the twin's reconnect loop
for that process.

- Reads (`ha_state_query`): entities/areas filtered, `group_by: area`
  supported; every response carries freshness signals — `twin_connected`,
  `last_ws_activity_ago_seconds`, `last_state_event_ago_seconds`, `stale`.
  Past the soft-stale threshold data still comes back with `stale: true`;
  past the hard threshold the tool refuses with a structured `twin_stale`
  error (the LLM is told the mirror is too old to trust).
- Writes (`ha_command`): modes `sync` (send + verify outcome, default),
  `send` (fire and return a call_id), `verify` (query outcomes for a
  call_id). Verification attributes HA's own `state_changed` events to the
  request and returns one outcome per entity from
  `applied | pending | already | unchanged | unavailable | error` —
  "verified, not just dispatched". There is no optimistic echo: the mirror
  changes only when HA says so.
- Command domains: light, switch, cover, climate, fan, media_player, scene,
  script, button, and siren only when explicitly listed in `commands.domains`.
  `lock` and `alarm_control_panel` are never available (PIN risk), by
  design, in every profile.
- The write barrier is deliberately non-configurable: commands go out only
  through a live, bootstrapped, fresh session within `hard_stale_threshold`;
  otherwise the model gets a typed refusal (`not connected`,
  `session not ready`, `stale`) and is told to use another tool from its
  catalog or report the action as unavailable — never to blind-retry.

## When enabling the twin: ask about tool access per profile

Treat enabling `home_assistant.enabled` and choosing its tool permissions
as one configuration discussion. Explain that `ha_state_query` reads home
state and `ha_command` controls devices with effect verification; a working
WebSocket connection alone does not make either tool available to a profile.

Inspect the selected agent's configuration to identify the active profile
and names declared by channels, top-level `profiles`, `builtin_tools`, MCP
servers and `home_assistant`. Discuss `default` and `admin`, plus any other
available or intended profiles such as `edit`; do not assume an `admin`
name automatically inherits access from another owner.

Ask one profile at a time whether the user wants no twin tools, read-only
access (`ha_state_query`), or reads and device control (`ha_state_query`
and `ha_command`). **Recommended starter decision** (apply unless the
operator chooses otherwise): both `default` and `admin` get reads and
device control — `allow: ["ha_state_query", "ha_command"]` under each —
matching the twin block `ready init`-era configs ship with. Explain the
choice, and wait for the user's answer before staging anything. Preserve existing
permissions unless the user asks to change them; do not silently grant
commands or copy one profile's decision to the others.

Stage the agreed wire names under `home_assistant.profiles.<name>.allow`.
**Allow and deny are mutually exclusive on one profile entry** — declare
one list only; to disable access entirely use `deny: ["*"]`, and to allow
everything except a few names use `deny:` alone (an absent allow means
allow-everything-except-denied). Show the resulting access for every
discussed profile before saving.
Top-level `profiles.<name>` and `builtin_tools.profiles.<name>` do not grant
twin tools; MCP server permissions are also independent.

When adding the first explicit `home_assistant.profiles` block, preserve
access the user wants to retain: implicit defaults exposing both tools to
`default` and `edit` disappear once that block exists. A profile omitted from
the explicit map gets zero twin tools, including `admin`. Explain any resulting
loss of access in the diff. Follow `assets/config-manager.md` for saving,
always report whether the changes are saved or awaiting confirmation, and
remind the user that activation is automatic (in-process runtime reload; no
`ready serve` restart).

## MCP profiles (tool gating)

Active profile precedence: `--profile` flag > `READY_DEFAULT_PROFILE` env >
`default_profile:` config > built-in `default`. A flag/env override needs a
restart to change; a saved `default_profile` applies on the in-process reload.

Ownership, not one global list: each owner declares its own gate against
**wire names** (unprefixed):

- `mcp_servers.<key>.profiles.<name>.{allow,deny}`
- `home_assistant.profiles.<name>.{allow,deny}` — twin tools only
  (`ha_state_query`, `ha_command`)
- `builtin_tools.profiles.<name>.{allow,deny}` — READY's own tools
  (`wait`, `skills-load`, `skills-unload`, `skills-read-asset`,
  `skills-list-assets`)

### Adding an owner: run the per-profile interview

Every time an MCP server is added — and whenever any other `profiles`
owner is touched (twin, `builtin_tools`) — ask, for EVERY profile already
defined in the configuration (`default`, `admin`, …), one profile at a
time, which of that owner's tools the profile may use (`allow:`) and
which it must never see (`deny:`). Offer a recommended starter access per
profile (think of who uses that channel: an admin chat may need write
tools; a wall-panel Telegram chat may need nothing) and stage the agreed
gate together with the server block in the same diff. An owner that does
not declare the active profile contributes zero tools (fail-closed), so a
server added without this interview silently exposes nothing.

### Pattern syntax (as implemented — glob, NOT regex)

Patterns live in code as `matchGlob` (`internal/profile/profile.go`) and
are deliberately glob-style, not regular expressions:

- `*` is the ONLY metacharacter: it matches any (possibly empty) sequence
  of characters INCLUDING `/` — e.g. `light/*` matches `light/turn_on` and
  `light/toggle`; `*/turn_on` matches `light/turn_on`.
- There is no `?`, no `[...]`, no alternation, no anchoring: every other
  character must match literally. `light/turn_on?` matches only a tool
  literally named `light/turn_on?`.
- Matching is case-sensitive and against the owner's UNPREFIXED wire name
  (`light/turn_on`, never `home_assistant__light/turn_on`), so patterns
  stay portable across `tool_prefix` changes.

Fail-closed everywhere: an owner that does
not declare the active profile contributes zero tools plus a boot WARN —
except that `builtin_tools` and the twin have built-in defaults applying
when their **entire `profiles` block is absent** (MCP servers have no such
default: forgetting them means zero tools, on purpose).

Cross-cutting `profiles.<name>` keys: `inject_skills` (extra skills in the
system prompt for that profile) and `disclose_denied_tools` (default false;
opt-in trade-off). With disclosure on, blocked calls are rewritten at the
model layer into a `policy_block` sentinel that answers with the fixed
policy-wall message — the turn continues and the refusal is recorded in the
transcript like any other tool call.

Diagnostics: `logging.profile_decisions: true` logs one DEBUG line per tool
naming the deciding pattern; dead patterns (allow/deny globs matching zero
tools) warn once at boot and re-run at DEBUG after MCP reconnects.

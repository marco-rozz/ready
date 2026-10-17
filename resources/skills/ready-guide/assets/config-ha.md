# Home Assistant: MCP servers, the digital twin, profiles, OAuth

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
  whole run — the tool slice is built once at boot and never mutated.
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

```yaml
mcp_servers:
  acme:
    url: https://acme.example.com/mcp
    oauth:
      grant: auto          # auto | authorization_code | client_credentials
      # client_id / client_secret / client_secret_file
```

- `oauth` and `headers.Authorization` on the same server are mutually
  exclusive; `oauth` + `transport: sse` is a boot error.
- There is no scopes key: the authorization server decides.
- Credentials persist one JSON record per server (`0600` records in a
  `0700` store) — the record anchors to the Install home unless an explicit
  `oauth.path` says otherwise.
- First-time auth happens through `ready login [mcp:]<server>` (browser);
  `--status` checks, `--forget` deletes.
- **Runtime recovery without restart**: authorization failures do not tear
  the session down. Running `ready login <server>` while the daemon is up
  is enough — the next tool call or health ping picks up the fresh
  credential.
- `grant: client_credentials` is autonomous (no stored credential needed);
  `authorization_code` requires a once-per-credential login.

## The digital twin (home_assistant:)

```yaml
home_assistant:
  enabled: true
  websocket:
    url: ws://homeassistant.local:8123/api/websocket
    token: "<long-lived token>"     # or token_file
  stale_threshold_states: 60s       # soft-stale marker
  hard_stale_threshold: 300s        # refuse-to-serve gate (reads AND writes)
  outstanding_pings_threshold: 2    # unacked pings before force-reconnect
  commands:
    verify_timeout: 5s
    max_targets: 20                 # broader selectors are refused
    # domains: [light, cover]       # absent = everything except siren
```

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

## MCP profiles (tool gating)

Active profile precedence: `--profile` flag > `READY_MCP_PROFILE` env >
`mcp_profile:` config > built-in `default`. Switching needs a restart.

Ownership, not one global list: each owner declares its own gate against
**wire names** (unprefixed):

- `mcp_servers.<key>.profiles.<name>.{allow,deny}`
- `home_assistant.profiles.<name>.{allow,deny}` — twin tools only
  (`ha_state_query`, `ha_command`)
- `builtin_tools.profiles.<name>.{allow,deny}` — READY's own tools
  (`wait`, `skills-load`, `skills-unload`, `skills-read-asset`,
  `skills-list-assets`)

Rules: `*` is the only wildcard (it crosses `/`), deny wins over allow,
empty allow = allow-everything. Fail-closed everywhere: an owner that does
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

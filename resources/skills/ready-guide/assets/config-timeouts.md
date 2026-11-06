# Timeouts — every clock, what it bounds, when to move it

READY. has dozens of time-related knobs. Almost all of them have a default
that is already right; the few that matter are easy to identify once you
understand the three families they fall into and the one ordering invariant
that ties them together. This asset is the map; `config-runtime-tuning.md`
covers the *token* budgets (context windows, compaction thresholds) that pair
with the clocks described here.

## The three families

**Turn clocks** bound how long one conversational turn may take. Moving them
changes what the user experiences directly (waiting, or a fast clean error).

**Link clocks** detect a dead transport — a crashed client, a dropped MCP
server, a zombie WebSocket. They never bound healthy work; they only decide
*how quickly a death is noticed*. Shorter = faster recovery, but too short
reaps healthy-but-quiet links.

**Background clocks** belong to healing and housekeeping that run outside
any turn: reconnect backoffs, recovery cooldowns and probes, session reset,
compaction. Their defaults encode how the system breathes; almost never
worth touching without a concrete log line pointing at them.

## The clocks of one turn, in order

Walk a single turn from the client's send to the final reply and every clock
appears exactly once:

1. **Per candidate LLM attempt** — `llm.failover_attempt_timeout` (45s).
   Bounds the wait for the *first chunk* of the first model call of that
   candidate; the first received chunk (even a tool-call iteration)
   disarms it permanently for that attempt. `0s` disables it.
2. **The whole ReAct loop** — agent deadline, **300s, fixed in code**
   (`cmd/serve.go` `agentRunTimeout`, not a config key). This is the hard
   ceiling of every turn; no candidate retry, backoff, or tool call may
   outwait it. With a long failover chain, per-candidate budgets get a
   proportional share of what is left — a chain of five candidates never
   gets five × 45s of patience.
3. **Each MCP tool call inside the loop** — `mcp_servers.<key>.timeout`
   (30s per JSON-RPC request). The twin's `ha_command` verification has its
   own finer clock: `home_assistant.commands.verify_timeout` (5s, clamped
   1–30s, and always bounded by the remaining turn deadline).
4. **The post-step formatter, if enabled** — 60s, fixed in code
   (`formatterTimeout`). A formatter failure never fails the turn.
5. **Delivery to the client** — `ready chat --reply-timeout` (900s client
   default). An *inactivity* deadline: armed at send, **re-armed by every
   progress chunk** of the in-flight turn, so a healthy long stream never
   trips it; only silence does.
6. **If the turn died silently** — the daemon-side watchdog
   `channels.zmqcli.reply_timeout` (400s) releases the pending slot of a
   client that is neither replying nor progressing. Its name is the release
   reason `reply_timeout` you may see in the log.

> **The invariant: keep the ordering.** client `--reply-timeout` ≥ daemon
> `zmqcli.reply_timeout` > agent deadline + formatter. The shipped defaults
> (900s > 400s > 300s + 60s + margin) are computed exactly this way. If you
> ever raise the agent deadline's environment (e.g. a much larger
> `failover_attempt_timeout` budget), raise the outer two to match — and
> remember the agent deadline itself is fixed, so the outer clocks need
> headroom above 300s, not equality.

## When idle (link clocks, ZMQ CLI)

- `channels.zmqcli.liveness_timeout` (45s) — a client that *used to ping*
  and stopped is declared dead quickly so a new client can attach at once.
  Clients that never ping (`--exec`, older clients) are exempt.
- `channels.zmqcli.connection_idle_timeout` (15m) — reaps a quiet
  connection (release reason `idle_timeout`). This is a transport expiry:
  it never ends the Session, and totals/usage survive the reconnect.

## Background clocks

| Key | Default | Bounds |
|---|---|---|
| `llm.failover_retries` / `failover_retry_backoff` | 2 / 2s (30s cap) | Same-candidate transient retries within one turn |
| `llm.recovery.soft.initial/max_cooldown` | 30s / 5m | Time out of rotation after a soft failure (429, 5xx, EOF) |
| `llm.recovery.hard.initial/max_cooldown` | 5m / 1h | Same, after a hard failure (auth, 404, quota, transport) |
| `llm.recovery.backoff_factor` | 2 | Cooldown multiplier per successive demotion |
| `llm.recovery.probe_timeout` | 10s | Each recovery probe's budget |
| `llm.recovery.idle_probe_window` | 15m | Probes only run if agent traffic was this recent |
| `mcp_servers.<key>.reconnect.*` | 1s → 60s, ×2 | Exponential reconnect curve after a server drop |
| `mcp_servers.<key>.health_check.interval` | 30s | Detection cadence for a dead MCP server |
| `home_assistant.monitoring.sync_timeout` | 15s | Boot wait for the twin's initial sync (non-blocking: it self-heals) |
| `home_assistant.monitoring.initial_backoff` / `max_backoff` / `backoff_factor` | 1s → 60s, ×2 | Twin WebSocket reconnect curve (flat under `monitoring:`) |
| `home_assistant.stale_threshold_states` / `hard_stale_threshold` | 60s / 300s | Soft-stale (annotate) and hard-stale (refuse + write gate) ages of the last WS activity |
| `home_assistant.outstanding_pings_threshold` | 2 (a count) | Unacked WS pings before a zombie half-open TCP is force-closed |
| `compaction.timeout` | 180s | The summariser call only; a failure delays nothing |
| `sessions.reset.idle_after` | none (required for idle modes) | Session ended after this much inactivity |

## Fixed by design (not in config.yaml)

- **300s** agent deadline (`cmd/serve.go`) — the turn ceiling everything
  else must fit under.
- **60s** post-step formatter LLM call.
- **30s** per inline MCP reconnect attempt (`internal/mcp/reconnect.go`).
- **1–300s** per `wait` tool call (the tool's own refusal is classified
  Transient; the ReAct loop continues).

## Symptom index — the log line tells you which clock fired

| Symptom | Clock that fired | Usual meaning |
|---|---|---|
| `release reason: client_unresponsive` | `liveness_timeout` | Client crashed or laptop slept; healthy system, nothing to tune |
| `release reason: idle_timeout` | `connection_idle_timeout` | Quiet client past 15m; shorten only to free the `busy` slot sooner |
| `release reason: reply_timeout` | `zmqcli.reply_timeout` | A turn hung past the watchdog; check the LLM log first, raise the key only if your (slow, local) provider legitimately needs it |
| Turn ends in `llm_unavailable` quickly | `failover_attempt_timeout` on every candidate | First chunk never arrived in 45s anywhere; local model loading? raise it or set `0s` |
| Turn ends in `llm_unavailable` after minutes | whole chain exhausted | Provider outage, not a timeout problem — look at `recovery.*` logs |
| `twin_stale` from `ha_state_query` | `hard_stale_threshold` | HA WebSocket quiet >5m; check HA, then consider raising only if silence is expected (long backups) |
| `stale: true` but data returned | `stale_threshold_states` | Informational; data is served anyway |
| MCP calls fail with timeout errors | `mcp_servers.<key>.timeout` | Tool genuinely slower than 30s (long histories); raise per-server |
| `/context` or compaction fails, session unchanged | `compaction.timeout` | Summariser slower than 180s; raise for very slow local models |

## Tuning by expectations (the guided pass)

Numbers mislead before you have runtime evidence; expectations do not. When
configuring a fresh deployment, answer four questions — each maps to at most
a couple of keys, and "keep the default" is the expected answer:

1. **When the AI cannot answer, do you prefer a fast error or a slow
   certainty?**
   *Fast error*: `failover_attempt_timeout: 15s`, `failover_retries: 1` —
   the user hears "unavailable" quickly and the recovery worker re-probes
   behind the scenes. *Wait it out*: keep 45s. The fixed 300s agent deadline
   is the backstop either way.
2. **Where does the LLM run?**
   *Local (Ollama)*: first-token latency includes model loading — give
   first chunks room: `failover_attempt_timeout: 120s` or `0s`. Keep
   `compaction.timeout: 180s`. *Cloud*: defaults everywhere; the tight
   clocks are an advantage, they fail over fast.
3. **How fast is your home?**
   *Slow integrations* (sleepy ZigBee, mesh lag): raise
   `home_assistant.commands.verify_timeout` to 10–15s so `sync` mode stops
   reporting `unchanged` for commands that are merely slow. *Wired/fast*:
   default 5s.
4. **Who consumes the channel?**
   *Scripts and cron* (`--exec`): timeouts are a contract — size
   `--reply-timeout` to your schedule, and branch on the
   `error_code: llm_unavailable` reply, not on exit codes. *Interactive
   human*: defaults; the TUI's progress re-arming makes long turns safe.

A fifth rule overrides all four: **defaults until a log line says
otherwise.** The symptom index above is the bridge — when a log line names
one of these clocks as the cause of a real problem, that is the moment to
move that key, and no earlier.

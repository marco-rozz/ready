# The channels block — how the user talks to READY.

Four channel slots exist in config: `cli`, `zmqcli`, `telegram`, `serial`.
Each has `enabled` plus the universal per-channel keys:

- `formatter_skill` — a skill that adapts responses for the channel; empty
  skips formatting entirely.
- `formatter_post_step` — `false` (default): the skill is injected into the
  system prompt and the model answers channel-aware in one streaming pass.
  `true`: the finished answer is reformatted by a separate LLM call with the
  skill as system prompt (non-streaming fallback; ~60s budget).
- `bootstrap` — skills preloaded into every session on the channel. A skill
  cannot serve as both `formatter_skill` and `bootstrap` (boot error).
- `session_reset` — optional per-channel override of `sessions.reset`
  (fields `mode`/`daily_at`/`idle_after`/`notify`; only set fields replace
  the global ones). Reset rules are registered only for enabled channels.

## cli vs zmqcli

Mutually exclusive — enabling both fails boot. They share the logical
channel name `cli`, so no other config changes when switching.

- `cli`: the plain stdin/stdout conversation, used mainly for development.
  Off by default.
- `zmqcli`: the primary operator interface. **zmq-first default**: when
  `channels.zmqcli.enabled` is not written and `channels.cli.enabled` is not
  true, it turns on as soon as an `auth_token` reference is set. An
  explicit `enabled` key always wins (`enabled: false` keeps it off; an
  explicit `cli.enabled: true` suppresses the default). With no secret and no
  explicit key it stays off, and `ready serve` then needs another enabled
  channel to start. `config.example.yaml` leaves these keys commented for that
  reason. This is the daemon side of the `ready chat` client (see
  `using-ready.md`). Keys:
  - `bind` — ROUTER endpoint; default `tcp://127.0.0.1:7600` (loopback only).
    For `ready chat` on other hosts bind a reachable interface, e.g.
    `tcp://0.0.0.0:7600`. HMAC authenticates peers but does not encrypt
    TCP traffic; prefer SSH forwarding for remote access, or use a trusted LAN.
  - `auth_token` — a reference, required when enabled:
    `!secret zmqcli_auth_token` (the value lives in the encrypted store;
    `serve` generates 64 hex characters when it is absent) or
    `!secret_file /absolute/path`. Minimum secret length 32 characters. Rotate
    it with `ready secrets set zmqcli_auth_token`: the next handshake adopts it
    without a restart; clients on other hosts need a fresh copy
    (`ready secrets reveal zmqcli_auth_token --to-file F`).
  - `connection_idle_timeout` — idle-Connection watchdog (default 15m).
    History note: an older `session_timeout` spelling was a renamed key and
    now fails boot naming the new one.
  - `reply_timeout` — ceiling for a pending reply (default 400s); a value
    too low to cover the agent+formatter deadlines is clamped up
    automatically with a WARN.
  - `liveness_timeout` — client-heartbeat window (default 45s); the TUI
    pings at interval/3, and a dead client (closed terminal, laptop sleep)
    frees the session in under a minute. A client that never pings
    (`--exec`) is exempt.
  - **Starter defaults written by `ready init`**: `formatter_skill:
    humanizer-formatter` (terminal-audience shaping, inline reinforcement;
    never the plain `humanizer` editor skill — as a formatter it makes the
    model emit its draft and anti-AI pass in the reply) and
    `bootstrap: ["home-topology"]` (home context preloaded into every
    session on the channel). Keep them unless the operator asks otherwise;
    a skill must not serve as both `formatter_skill` and `bootstrap`.

Operationally: one Connection per secret; a second concurrent client is
rejected `busy`; an idle/dead client frees its lock; a never-ending reply
is released by `reply_timeout`; after idle removal the client's next
request gets `no_active_session`.

## telegram

Recommended starter defaults when configuring this channel (match
`config.example.yaml`): `bootstrap: ["home-topology",
"humanizer-formatter"]` and `formatter_skill: telegram-formatter` — the
channel's own formatter (HTML adaptation for the model's side; the
mechanical escaping and 4096 splitting happen in the channel regardless).
Never bootstrap or set as formatter the plain `humanizer` editor skill:
its draft + anti-AI workflow leaks into the model's replies. A skill
cannot serve as both `formatter_skill` and `bootstrap` (boot error).

- `token` — bot token, required when enabled.
- `allow_from` — whitelist, required when enabled. Accepts chat IDs, user
  IDs, bare usernames, `@username`; an entry that is only whitespace is
  skipped; a NON-empty list that matches nothing rejects silently (log-side
  only). An empty list accepts everyone — but an empty list is a boot
  error since `allow_from` is required; to accept everyone legitimately you
  still need at least one entry, so prefer an explicit allow-list.
- Mechanics handled for you: long polling, HTML parse mode with sanitising
  (official Bot API tag whitelist, entity escaping, and tag balancing —
  unbalanced tags from streaming chunk boundaries or the 4096 truncation
  are repaired instead of being rejected, so the plain-text fallback stays
  a last resort), 4096-character splitting (newline-preferred), plain-text
  fallback when the API rejects HTML for a chunk, typing indicator
  refreshed every ~4s during a run, `setMyCommands` registering `/new` and
  `/compact`, and streaming edits of one message (send once, edit while
  chunks arrive).
- Per-chat conversation addresses: `telegram-<chat_id>` — each chat keeps
  an independent session.
- If another bot instance polls the same token, the 409 conflict retries on
  a ladder until released; the channel keeps running.

## serial (PETSCII, not yet implemented)

`port` is required when enabled; `baud_rate` defaults to 9600. There is no
serial channel implementation in the daemon yet — enabling it only
contributes to enabled-channel checks and defaults.

## Response routing facts

Channels receive from one goroutine each and send through the manager; the
dispatcher runs one FIFO worker per conversation address (inbox 16, agent
deadline 300s, panic-recovery). Telegram replies find their destination via
the `chat_id` stored on the incoming message's metadata.

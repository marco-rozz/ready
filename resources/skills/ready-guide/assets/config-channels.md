# The channels block — how the user talks to READY.

Five channel slots exist in config: `cli`, `zmqcli`, `telegram`, `serial`,
`whatsapp`. Each has `enabled` plus the universal per-channel keys:

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
- `zmqcli`: the daemon side of the `ready chat` client (see
  `using-ready.md`). Keys:
  - `bind` — ROUTER endpoint; default `tcp://127.0.0.1:7600`.
  - `auth_token` / `auth_token_file` — exactly one required when enabled;
    minimum secret length 32 chars (the error suggests
    `openssl rand -hex 16`); `_file` content is trimmed.
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

Operationally: one Connection per secret; a second concurrent client is
rejected `busy`; an idle/dead client frees its lock; a never-ending reply
is released by `reply_timeout`; after idle removal the client's next
request gets `no_active_session`.

## telegram

- `token` — bot token, required when enabled.
- `allow_from` — whitelist, required when enabled. Accepts chat IDs, user
  IDs, bare usernames, `@username`; an entry that is only whitespace is
  skipped; a NON-empty list that matches nothing rejects silently (log-side
  only). An empty list accepts everyone — but an empty list is a boot
  error since `allow_from` is required; to accept everyone legitimately you
  still need at least one entry, so prefer an explicit allow-list.
- Mechanics handled for you: long polling, HTML parse mode with sanitising,
  4096-character splitting (newline-preferred), plain-text fallback when
  the API rejects HTML for a chunk, typing indicator refreshed every ~4s
  during a run, `setMyCommands` registering `/new` and `/compact`, and
  streaming edits of one message (send once, edit while chunks arrive).
- Per-chat conversation addresses: `telegram-<chat_id>` — each chat keeps
  an independent session.
- If another bot instance polls the same token, the 409 conflict retries on
  a ladder until released; the channel keeps running.

## serial (PETSCII, planned Phase 4)

`port` is required when enabled; `baud_rate` defaults to 9600. There is no
serial channel implementation in the daemon yet — enabling it only
contributes to enabled-channel checks and defaults.

## whatsapp (planned Phase 5)

`session_path` configuration exists; the `whatsmeow` implementation is not
in the daemon yet. Enabling it behaves like serial: config-level only.

## Response routing facts

Channels receive from one goroutine each and send through the manager; the
dispatcher runs one FIFO worker per conversation address (inbox 16, agent
deadline 300s, panic-recovery). Telegram replies find their destination via
the `chat_id` stored on the incoming message's metadata.

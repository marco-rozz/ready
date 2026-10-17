# Tuning the numbers — what to set, what to leave alone

These five knobs are the ones worth tuning on a real deployment, and the
only ones where measured evidence beats rules of thumb:

## context_window (per provider instance)

Sets the denominator of the ctx% indicator in the chat status line and
feeds the compaction trigger. Set it to the model's real context capacity.
Ollama callers must keep `options.num_ctx` EQUAL to it (mismatch warns —
Ollama truncates to `num_ctx`, making the percentage a lie).

## compaction.reserve_tokens / keep_recent_tokens

- `reserve_tokens` — headroom kept free in the window; if the trigger
  fires, older turns are summarised so the prompt comes back under it.
- `keep_recent_tokens` — how much verbatim recent transcript survives a
  compaction. The cut lands at a turn boundary near that budget and never
  splits a tool call from its result.

Evidence to tune by: read `turn_usage` records in the session files
(`read_tokens` per turn); the median of your biggest normal turns tells you
whether the defaults overspend or waste.

## compaction.max_turns / keep_recent_turns

The fallback pair used only when NO provider in the chain declares a
context_window: compact after `max_turns` turns, keep the last
`keep_recent_turns` (must be < `max_turns`). If you can, declare
`context_window` and use token-based mode instead.

## failover_attempt_timeout

Budget for the first chunk of the first model call of a candidate. Local
models that spend tens of seconds loading/first-token benefit from a
generous value (or `0s` to disable and rely on the outer 300s turn
deadline). Cloud providers usually want the default. Note it only bounds
the first chunk; a healthy stream never gets cut by it.

## What NOT to tune blind

- `failover_retries` / `failover_retry_backoff` — the defaults (2 retries,
  2s base, 30s cap) already handle transient provider flaps; raising them
  lengthens failed turns on genuinely dead providers.
- `recovery.*` — the two ladders encode "how long a broken provider waits
  before being trusted again"; leave unless you have concrete evidence of
  flapping between two providers.
- `sessions.*` — defaults match the session-model design; tune only
  retention when disk space matters.

## Reading runtime evidence (operator's view)

Per-session evidence lives in `<sessions path>/<address>/<file>.jsonl`:

- one `turn_usage` line per turn: `read_tokens`, `written_tokens`,
  `cached_tokens` (omitted = provider reported none), `llm_calls`
  (ReAct iterations — diverges from `turns` exactly when tools ran),
  `aux_calls` (formatter post-step + compaction summariser);
- `cache_tokens` present means the provider reported a prompt cache — Ollama
  never does, so `C…%` stays absent there by honesty, not by bug;
- `compaction` lines record `tokens_before/after`; their count shows in
  `/session`.

These totals survive daemon restarts: the file is the source of truth, the
in-memory store is rebuilt from it.

# Sessions, Turns, compaction and usage — the model behind the chat

What actually happens between "I ask" and "I answer" — the part of READY.
that makes it trustworthy rather than merely chatty.

## Conversation → Session → Turn

- A **conversation address** is a channel participant: the terminal share
  one address (`cli`), every Telegram chat has its own
  (`telegram-<chat_id>`), so parallel chats never see each other's history.
- Each address owns one current **Session** — an append-only transcript
  file (`<sessions path>/<address>/<timestamp>_<uuidv7>.jsonl`), never
  rewritten. It records the verbatim user message, the frozen per-turn
  context line, every ReAct iteration (assistant thought + tool calls + the
  exact tool results), the final reply, and one usage line per turn.
- A **Turn** is one user request processed through the ReAct loop. A Turn
  that fails still records what happened: tool calls that completed keep
  their results visible, interrupted ones are closed with an explicit
  `[Transient] Interrupted: … it may or may not have executed` line, and
  usage is still counted.

## Why that matters for you

- **Verified outcomes, not vibes.** Device commands return per-entity
  outcomes (`applied`, `pending`, `already`, `unchanged`, `unavailable`,
  `error`) taken from Home Assistant's actual event stream — the assistant
  reports what it observed, not what it hoped.
- **A failed turn is honest.** If the attempt dies after some tool
  executions, the terminal message says so (and asks the user to check
  current state before repeating). If nothing ran, it says that instead.
  These two texts are fixed system messages, not model improvisation.
- **Failover never repeats, never re-runs.** If a provider dies mid-turn,
  the next candidate continues from the recorded transcript: tools already
  run never re-execute; if content had already started streaming, the
  failed turn simply ends rather than risk the user seeing a replay.

## /new and /compact

- `/new` ends the current Session and opens a fresh one
  (`New session started.`; `Session already new.` if nothing to end).
- `/compact [instructions]` summarises the older transcript now, same as
  automatic compaction (`Context compacted.`, `Nothing to compact.`, or
  `Compaction failed; the session is unchanged.`).
- Neither ever reaches the LLM; neither is recorded in the transcript.
- On the ZMQ client, `/new` also pushes a `session_changed` event (the TUI
  clears its usage segment) and compaction pushes `compacted`
  (`before → after tokens`).

## Automatic compaction, from the user's view

Sessions are transcripts, not malloc: the daemon never truncates. When the
prompt approaches the model context (window − reserve, or a turn-count
limit), the older part is summarised by an LLM call, a `compaction` line is
appended, and the model sees summary + verbatim recent tail afterwards. If
the summariser is down, the transcript is unchanged and the daemon just
backs off; a turn that arrives still over the limit runs on a reduced view
rather than being refused.

## Usage accounting

The tower of causes for "why is my response costing what it costs":

- Every candidate LLM call of every turn is attributed to the session:
  `read_tokens` (prompt), `written_tokens` (completion), `cached_tokens`
  where the provider reports them, split into `llm_calls` (agent
  iterations) vs `aux_calls` (formatter post-step, compaction summariser).
- One turn = one delivered reply (successes and failures both count).
- The totals ride on every final reply (and on streamed progress chunks),
  rendered in the TUI status line as
  `R… C…% O… · ctx …% [turns»llm calls]`, and survive daemon restarts
  because the session file is the source of truth.
- Boot-time readiness probes and recovery probes record nothing by design.

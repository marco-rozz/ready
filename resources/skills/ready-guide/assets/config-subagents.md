# Subagents — background delegation (`subagents:`)

Subagents are bounded helper agents the main agent (the "owner") can spawn
to work in the background: the owner continues talking to the user while a
subagent runs its task through the same LLM failover chain, then a
structured result ("handback") is delivered back into the conversation.
Definitions are `*.md` assets with YAML frontmatter in
`<agent-dir>/subagents/`, library-synced like skills (shipped library
provides `general-purpose` and the read-only `explore`).

```yaml
subagents:
  enabled: true               # default false — OFF means the tools are refused
  max_depth: 1                # nesting cap, 1-2 (at 2 a child may spawn grandchildren)
  max_concurrent: 4           # running dispatches per owner Session
  max_concurrent_jobs: 1      # daemon-wide concurrent internal jobs
  mailbox_buffer: 32          # per-child bounded FIFO of queued follow-ups
  wait_timeout: 60s           # subagent_wait's default blocking window
  handback_max_tokens: 4096   # one result's report cap
  handback_turn_budget_tokens: 16384  # results injected at the head of one Turn
                              # (also capped at 25% of the smallest context_window)
  path: subagents/            # definitions directory, inside the agent dir
  sync: auto                  # definition library sync (auto|notify|off)
  # library_path:             # shipped library; program storage by default
```

## What the agent gets

Five tools, registered under the `builtin_tools` policy owner (so the
per-profile `builtin_tools` rules gate them like any internal tool):

| Tool | What it does |
|---|---|
| `subagent_spawn` | Starts a subagent on a self-contained task and returns immediately. |
| `subagent_send` | Queues a follow-up for a running child; reopens an idle one with its context. |
| `subagent_wait` | Blocks (up to `wait_timeout`) for pending results. |
| `subagent_list` | Fleet status of the current Session. |
| `subagent_interrupt` | Cancels a running dispatch. |

Results reach the model two ways: blocking via `subagent_wait`, or
automatically at the head of the owner's next Turn. Results pass an
injection scan (a child's report cannot forge owner-side instruction tags);
the scan mutates, never withholds.

## Interview points

- **Enabled is the whole feature switch** (`enabled: false` is the shipped
  default): the tools are registered but every spawn answers
  "subagents feature is disabled". Propose enabling only when the user
  wants background work; it costs nothing when idle.
- **Depth**: `max_depth: 1` (default) keeps one level — a child cannot
  spawn. `2` allows dispatch-scoped grandchildren; ask only if fan-out is
  really needed.
- **Profiles**: ask the standard `builtin_tools` question for each profile
  — which profiles may call `subagent_*`? A deny on `subagent_*` hides
  delegation for that profile while the feature stays on for others.
- **Definitions**: a `*.md` file in `subagents/` defines a type
  (frontmatter: `tools` XOR `disallowed_tools` allowlist, `model: inherit`
  or a chain member, `visibility: internal` = daemon-jobs only, hidden from
  the model). A definition file that fails validation fails the boot
  naming the file; edits revalidate at reload and keep the previous set on
  error.
- **Library**: the shipped library installs `general-purpose` and
  `explore` into program storage; `sync: auto` mirrors it into the agent's
  runtime copy without touching operator edits, exactly like skills. A
  missing library is a WARN (`skill library absent`), not a boot failure.

## Lifecycle facts worth knowing when advising

- Child transcripts live under the owner Session's folder:
  `sessions/<address>/<folder>/subagents/<name>_<ts>_<id>/session.jsonl`.
  Ending the owner Session (any reason) cancels running dispatches and
  closes every child with the same closing reason; retention then deletes
  the whole tree like any ended Session.
- A daemon restart never resumes a dispatch: a finished-but-undelivered
  result is regenerated from the child's recorded reply; a cut-off one gets
  a fixed "interrupted" handback. Pending (undelivered) results survive
  restarts; queued mailbox messages do not.
- A child's token usage accrues to the owner Session's totals; the TUI
  shows a bounded fleet panel while at least one subagent is ACTIVE
  (running or idle) — a finished one-shot drops out of the panel, and every
  terminal transition also leaves a delivery-trace line (name, status,
  duration) in the transcript.

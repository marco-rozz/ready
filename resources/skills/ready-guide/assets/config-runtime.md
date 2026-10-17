# Runtime keys — sessions, compaction, logging, skills

## sessions

```yaml
sessions:
  path: sessions/               # default under the runtime home
  reset:
    mode: daily                 # never | daily | idle | daily+idle
    daily_at: "04:00"           # local HH:MM
    # idle_after: 6h            # required by the idle modes
    notify: true                # message-oriented channels get a notice
  retention:
    retention_days: 30          # ended Sessions, by the NAME's date
    max_total_size: 200MB       # ended Sessions, oldest first, across addresses
```

- A new Session can start in three ways: the `/new` command (reason
  `command`), a daily reset (default 04:00 local), or an idle reset
  (`idle_after` since the last Turn; under `daily+idle` the daily check
  wins). Reset is a pure function of last-Turn time — no timers, no model.
- Per-channel timing of the reset check differs by channel kind: Telegram
  checks before each Turn (with the `notify` notice
  `New session started; the previous one was archived.`); the zmqcli
  channel checks at Connection start, silently; the stdin CLI checks once
  at its first message.
- Compaction never counts as activity for reset or idle purposes.
- Retention touches only ended `.jsonl` Sessions and only files matching
  the naming scheme; the current Session is never deleted. A pass runs at
  boot and after every Session end. `retention_days: 0` deletes ended
  Sessions at the very next pass.
- Channel overrides: `channels.<name>.session_reset` with the same fields
  (only set fields replace the global ones). Invalid values (bad mode,
  malformed `daily_at`, idle mode without `idle_after`) fail boot.

## compaction

```yaml
compaction:
  enabled: true              # automatic; /compact works regardless
  skill: home-compaction     # hidden from the agent catalog
  reserve_tokens: 16384
  keep_recent_tokens: 20000
  max_turns: 20              # trigger only when no context_window anywhere
  keep_recent_turns: 5       # must be < max_turns
  timeout: 180s
```

- Random-access memory, in plain terms: after a reply lands, the daemon
  checks the prompt size (the largest declared `context_window` minus
  `reserve_tokens`; without any declared window, turn count vs
  `max_turns`).
- Over threshold: an LLM call on the aux chain summarises the older part of
  the transcript — a `compaction` record is appended to the session file;
  the model thereafter sees the frozen summary plus a verbatim recent tail.
  Nothing is ever rewritten: the file stays append-only.
- The cut never splits a tool call from its result, never touches the
  in-flight turn, and the summariser budget is `compaction.timeout`.
- Failure appends nothing, warns (`consumer=compaction`), and backs off 1,
  2, 4, 8 evaluations; any success (including manual `/compact`) resets the
  backoff. A session arriving over the threshold while a compaction would
  be late still runs — on a projection that omits its oldest whole Turns
  (the file is untouched; a WARN states this).
- `/compact [instructions]` runs the same machinery on demand.

## logging

```yaml
logging:
  level: info            # trace|debug|info|warn|error (file + console)
  # console_level: warn  # trace|debug|info|warn|error|off
  format: console        # console | json
  # file_path: ""        # optional JSON file, as dated segments
  # file_rotation:
  #   max_segment_size: 10MB   # >= constraint with max_total_size
  #   retention_days: 7
  #   max_total_size: 100MB
  # zmq_payloads_enabled: false  # DEBUG bodies (auth_token never logged)
  # thoughts: false              # ReAct thought records (privacy opt-in)
  # profile_decisions: false     # per-tool profile decision DEBUG lines
```

- `file_path` accepts a file (`logs/ready.log`) or a directory form
  (`logs/` → `logs/ready.log`); segments never rename
  (`ready-YYYY-MM-DD[.n].log`), rotate at local midnight or on size, keep
  browsing back `retention_days` days and cap total size (future-dated
  segments are trimmed first; pre-2025 dates are exempt from the day
  window).
- `<file_path>.lock` guarantees one daemon per log directory: a second
  `ready serve` on the same directory dies at boot.
- A failing file sink never blocks the console: records drop from the file
  only, one WARN announces the transition and an INFO the recovery, retries
  throttle to 30s.
- `READY_LOG_LEVEL` env overrides BOTH sinks at runtime, even a console set
  to `off`. No legacy alias is recognised. An unrecognised value keeps the
  YAML levels and warns.
- `console_level: off` silences the console except fatal records (and the
  file-failure notices mentioned above).

## skills

```yaml
skills:
  path: skills/                     # runtime copy, default under runtime home
  sync: auto                        # auto | notify | off
  # library_path: ../resources/skills/   # default <Install home>/library/skills/
  hot_reload: true                  # fsnotify → SKILL.md edits live
```

The boot sync is per file, three-way, against a recorded state file
(`.library-state.json` inside the runtime copy). Practical outcomes:

| Situation | Result |
|---|---|
| new shipped file | copied into the runtime copy |
| untouched runtime file + library changed | shipped fix applied |
| operator-edited runtime file + library changed | CONFLICT: left untouched, WARN tells you to diff the two copies |
| operator-deleted runtime file | deletion respected, never resurrected |
| operator-added extra file | left alone |
| `generated/` paths | never synced — the twin snapshotter owns them |
| shipped file removed from the library | left as an orphan, with a WARN |

`notify` does the same accounting but writes nothing (report only); `off`
skips the sync before touching the filesystem. The daemon never writes into
the library. A broken library sync fails boot; a missing library is a
silent no-op (binary-only deployments).

In-chat skills surface as metadata in the system prompt (progressive
disclosure); the agent loads full content on demand with `skills-load`,
releases it with `skills-unload`, and can read per-file assets with
`skills-read-asset` and list them with `skills-list-assets`.

# Troubleshooting — what each failure means

Organised by where you noticed it. Every entry says the logged line or the
user-visible text to look for, and the actionable next step.

## Config fails to load

- Missing/invalid `config_version` — the first supported schema is the integer
  `1`; check the layout against the shipped example before marking an old
  development file. There is no historical import or automatic version guess.
- `config_version … binary target …` — if older, `login` prints the exact
  `ready serve --config …` command to migrate and start the daemon; if newer,
  upgrade the binary. Do not lower the marker by hand.
- `configuration changed during migration` — stop concurrent editors and retry;
  the observed external edit was preserved. A migration requires an unlinked
  regular file and directory permissions for backup, temporary file and lock.

- `failed to read config file` / `failed to parse config file` — the YAML
  itself: check indentation/syntax; remember the unparser is non-strict, so
  a wrongly-spelled NEW key never errors, it is silently ignored. When a
  knob "does nothing", check the spelling in `config.example.yaml`.
- `config uses removed keys: …` — old key spellings fail loudly naming
  their replacement (e.g. `channels.zmqcli.session_timeout` →
  `connection_idle_timeout`, `persistence.checkpoints_path` →
  `sessions.path`, legacy `llm.provider/model/models` shapes).
- `channels.cli and channels.zmqcli cannot both be enabled` — pick one;
  they are the same logical channel.
- `channels.zmqcli.auth_token … required` — set a secret of at least 32
  chars on both daemon and client.
- `no channels enabled in configuration` — enable something. With the
  zmq-first default, setting `channels.zmqcli.auth_token_file` (or
  `auth_token`) is enough to enable `zmqcli`; an explicit
  `channels.zmqcli.enabled: false` or `channels.cli.enabled: true` disables
  that default.

## Boot warnings that do NOT block

- `MCP server connection failed — skipping (best-effort boot)` — that
  server's tools are absent until restart; check `url`, reachability and
  the Bearer header.
- `no usable oauth credential for this server — run 'ready login <server>'`
  — first-time OAuth; login out-of-band while the daemon runs is fine, the
  server re-reads on the next tool call without restart.
- `no LLM candidate ready at boot` — the daemon starts `unavailable` and
  recovers automatically once a provider answers; fix endpoints/keys in the
  background or let the recovery probes find them (probes run only while
  traffic is recent).
- `config file is readable by group or others` / same for secret files —
  `chmod 600`.
- `ollama options.num_ctx differs from context_window` — real truncation
  risk; align them.
- `policy owner does not declare the active profile` — an MCP server, the
  twin, or builtin_tools lacks the current profile block; that owner
  exposes zero tools (fail-closed). Add the profile to the owner.
- `profile gating: pattern matches zero tools` (dead pattern) — a
  `allow/deny` glob matched nothing; check wire-name spelling.

## LLM outage behaviour

- TUI status line red `LLM unavailable`, or terminal message
  `The AI service is temporarily unavailable. Please try again in a few minutes.`
  — nothing ran; the turn never started. Providers may still be cooling
  down from an earlier failure (soft 30s→5m, hard 5m→1h, doubling); check
  with `ready chat` later or restart the daemon to clear health state.
- `The response could not be completed. Some actions may already have run;
  check the current state before repeating the request.` — tools DID run;
  do not blind-repeat the request, verify device state first.
- `reply Content` with `error_code: llm_unavailable` — the whole candidate
  chain was attempted and failed (`llm candidate chain exhausted` in the
  daemon log); scripts should branch on this code, never the exit code.
- Per-candidate doubts → the classification: auth/404/quota demote hard
  (quota takes the whole provider instance down), 429/5xx/EOF demote soft
  (short cooldown), 400s and cancellations demote nothing.

## Tool-call errors (from the transcript)

The model sees a severity prefix inside the observation; knowing them helps
you decide what to fix:

- `[TRANSIENT]` — retryable: MCP timeout/connection, wait-tool budget
  refusals, the twin's barrier refusals (`not connected`,
  `session not ready`, `stale`), `twin_stale` reads.
- `[PERMANENT]` — needs user action: MCP auth, MCP tool-not-found, twin
  vocabulary/parameter refusals (bad domain, unknown entity, target over
  `max_targets`).

## Client-side codes and what triggers them

- `2` another `chat` session is attached; either wait for the other one to
  end (idle releases it after 15m) or restart the daemon.
- `3` token mismatch; check `--token`/`auth_token` equal values.
- `124` reply exceeded `--reply-timeout` (client) or turn watchdogs fired
  (daemon); for very long local-model first tokens, raise the daemon's
  `failover_attempt_timeout` and the client deadline.
- `0` + `daemon shut down` — normal; the daemon exited.
- `no_active_session` / `session expired` (exit 1) — the idle watchdog
  released this connection (default 15m of idle); reconnect.

## Twin stale / not connected

- `twin_stale` (JSON error on reads): the WS mirror is older than
  `hard_stale_threshold` — typically Home Assistant restarted. The twin
  reconnects automatically; control recovers within `verify_timeout` of a
  successful bootstrap. No restart needed.
- `not connected` / `session not ready` (writes): the same barrier in its
  pre-tolerance state; the model is instructed to fall back or report
  unavailable, not to retry blind.

## Logging and disk

- `another ready serve is already writing to this log directory` — a
  second daemon on the same `logging.file_path`. One per directory, by
  design.
- `log file is failing / recovered` notices — the file sink cannot write
  (permissions, disk). The console keeps working; fix the target and it
  self-repairs within the 30s retry loop.

## Sessions

- `skipped corrupt session file line` at restore — a crash cut a line; the
  rest of the session loads fine. Nothing to do.
- `session history restored with skipped corrupt lines` — same, aggregate.
- `session inbox saturated — message dropped` — one address pushed more
  than 16 messages while its worker was busy; slow down that flood source.
- `PANIC in session worker` + recovery — the turn died, the address keeps
  serving; capture the stack and report.

## First start on slow storage

- Tens of seconds of complete silence before the banner after installing on
  a Raspberry Pi 1-class board (slow SD card) — the statically linked binary
  is read entirely from a cold page cache before anything can execute or
  print. This is normal, not a hang: the banner appears within milliseconds
  once the ELF finishes loading. Subsequent starts are faster while the
  page cache is warm; a reboot puts the full read cost back.

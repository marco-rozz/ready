# Configuration versions and upgrades

Every READY configuration must declare its schema explicitly:

```yaml
config_version: 2
```

This integer describes the file format, independently of the READY release
version. The current schema is 2; the earliest supported migration source is 1.
There is no conversion from an unversioned file or an obsolete development layout. If a file already follows
the current example, add the marker manually; otherwise update its layout
using `config.example.yaml`. Merely changing the marker never converts fields.

## Starting and upgrading

`ready serve` selects the agent directory (`--agent-dir`, default `~/.ready`
independent of the working directory) and reads exactly `<agent-dir>/config.yaml`,
checking its version before starting services. A current configuration is read
without rewriting it or creating migration artifacts, even in a directory
without write permission. Installers place only a reference example in program
storage; `ready init` creates an agent configuration. Reinstalling preserves existing agent files.

When an older supported file requires a schema transformation, `serve` automatically
applies every intervening step, validates the final result, and only then
creates a backup and replaces the file. Skipping releases is supported. An
error before replacement leaves the original unchanged. Remote LLM, MCP and HA
availability and interactive OAuth login are not prerequisites for migration.
Structural configuration validation still applies. Secret values and external
secret files are resolved separately during runtime startup. Optional runtime
defaults are not written into the file.

All `ready login` modes, including `--status`, `--forget` and `--device-code`,
read configuration without migrating it. If an upgrade is needed, login prints
the current/target versions and a quoted command for that exact agent
directory, such as:

```sh
ready serve --agent-dir '/path/to/agents/casa'
```

That command also starts the daemon; stop an older daemon before using it.
On Windows, the displayed command uses PowerShell quoting. `ready chat` without token flags reads only
the `config_version`, `channels.zmqcli`, `agent` and `secrets` blocks of the selected agent (and fails with
upgrade guidance on another version). A config newer than the binary is rejected: use
a compatible newer READY binary, rather than lowering the marker by hand.

Schema 2 moves every credential out of the file. Loading decodes each sensitive
field only as a Secret reference (`!secret <name>` or `!secret_file <absolute
path>`); a literal is refused naming the field path, never the value. Validation
is purely structural: it reads no Secret store, no Store key and no secret file.
Value rules (non-empty; the chat secret at least 32 characters) are checked
when a value is written and again at boot, where a violation behaves as pending
(refusing boot only for the chat secret).

Serve boots degraded: components whose reference has no usable value stay off
until an in-process runtime reload or restart; unrelated components start normally.
The admin TUI triggers reload after its post-save secret/OAuth flow. Login refuses only
a target that owns a pending credential, skips such targets when none is
selected, and reports them in `--status` without network work.

The direct-entry wire protocol is separate from the file schema. ZMQ clients
and daemons exchange integer `protocol_version: 3` during both
`session_start` rounds. A mismatch is rejected before challenge/HMAC state is
created; install matching `ready` releases on both ends. The version changes
only when a strict peer could reject or misinterpret the wire contract.

## Files and recovery

Migration transforms the YAML document while preserving comments, untouched
values and key order. Current-schema validation rejects unknown or misspelled
keys, so an unknown key can prevent an upgrade from committing;
whitespace may normalize. The marker is placed first in migrated output.
Backups contain the exact original bytes. The 1→2 migration therefore seals
its backup (below); ordinary schema-2 commits write plaintext backups of a file
that holds no secret, and `ready secrets adopt` seals its backup because the
previous file still held a literal.

The sole accepted filename is `config.yaml`; migration files live beside it:

- `config.yaml.bak.<UTC timestamp>.<unique suffix>[.age]`: original backup (the
  `.age` form is encrypted to the Store key); the newest three recognized
  backups for this basename are retained after a successful migration. Other
  configs and manually named backups are excluded.
- `config.yaml.migration.lock`: stable advisory lock file, created only when
  migration is required and retained after release. Its existence does not
  mean a migration is active.
- `.config.yaml.migration-<unique suffix>`: temporary candidate; removed on
  ordinary pre-commit failure. A process crash can leave a temporary file.

On Unix backup and temporary files use 0600 or stricter source permissions;
the replacement config retains its original permission bits. On Windows the
source DACL is applied before writing content, and replacement carries those
access restrictions. Keep the whole directory access-controlled.

Required migrations need a writable directory for locks, backups and temporary
files. Migration refuses a symlink leaf or a file with multiple hard links:
replace the selected directory’s `config.yaml` with a regular file that has
only one hard link before retrying. Current
schema reads still support links. READY serializes its migration writers and
checks for external changes before replacement. Advisory locks cannot exclude
an arbitrary editor in the final check/replace interval; edit while stopped.

Failure to prune an old backup produces a warning and keeps the committed
config. A failure after replacement, such as logger startup, reports that the
new schema is already committed and names its backup; it does not silently
roll back a successful migration.

To restore manually, stop READY, inspect and copy the selected backup to the
config path with appropriate permissions, and use a binary compatible with
that backup's schema. For the 1→2 migration decrypt the sealed backup with
`age -d -i <agent-dir>/secrets.key config.yaml.bak.….age > config.yaml`, recreate
any deleted `*_file` it referenced with
`ready secrets reveal <name> --to-file <original path>`, then run the previous
binary. Starting the latest binary again may upgrade it again.
No automatic downgrade or runtime-data migration is provided.

## The 1→2 migration (secret import)

`ready serve` runs it automatically on a schema-1 config. Phases, under the
migration lock and the daemon's ownership:

1. **Plan** (`SecretImporter.PlanImport` + the pure `Up`): sensitive fields are
   rewritten to `!secret`/`!secret_file`; inline values and agent-local
   `*_file` contents (paths resolved against the agent directory exactly as
   v1 loading did, one trailing newline removed) are collected; external paths
   become absolute `!secret_file`; `*_file` keys and `oauth.token_store` are
   dropped; `agent.id` (UUIDv7, or the id of an already existing store, so a
   retry never mints a second identity) and `agent.name` are added. A
   `REDACTED-…` value anywhere refuses with path and line only; values are
   checked against the value rules before anything is written.
2. **Validate** the rewritten document.
3. **Store write**: the values enter `secrets.age` idempotently under the store
   lock (the same value is fine, a different value under the same name stops
   the migration) and the imported plaintext paths are recorded in the store's
   cleanup list.
4. **Sealed backup**: the original is written as
   `<config>.bak.<ts>.<n>.age`, encrypted to the Store key.
5. **Commit** (identity/mode/bytes recheck, atomic replace) — the point of no
   return.
6. **Cleanup**: the recorded plaintext files and READY's earlier plaintext
   config backups are deleted; a failure only WARNs and is retried at every
   boot, never rolled back.

A crash before step 5 leaves the schema-1 file and its files intact and the
next boot repeats the work; a crash after it leaves a valid schema-2 config and
the next boot finishes the cleanup. Existing `oauth/*.json` OAuth records are
not imported (no runtime-data migration): each target needs a new
`ready login`.

## Adding a migration

The engine and explicit production registry live in `internal/config`.

1. Determine whether a persisted transformation is necessary. A compatible
   optional field using a runtime default does not inherently require one.
2. Add a `Migration` implementing `Version()` (the next destination integer),
   `Description()` and `Up(*yaml.Node) error`, with a compile-time assertion.
   Transform only the authored document; perform no external actions. The
   engine owns `config_version` updates. (Only the 1→2 step implements
   `SecretImporter`; future steps stay pure.)
3. Register the step explicitly in `ProductionMigrations()` and increase
   `TargetConfigVersion` in the same change. Destinations must cover every
   integer from baseline+1 to target, in order. Keep released steps immutable.
4. Preserve comments, unknown keys and explicit false/zero/empty values. Define
   rename destination conflicts and changed-default behavior explicitly. An
   explicit value equal to an old default may be an intentional choice.
   Preserve anchor/alias/merge meaning or refuse the transformation safely.
5. Add fixtures/tests for supported source versions, skipped releases,
   idempotent current-version reads, conflicts, invalid candidates, authored
   YAML preservation and failures before/after commit. Validate using the
   original elected path; never serialize the runtime Config struct.
6. Update the example, operator docs and corresponding guide assets; run
   config/command tests, race checks and `make qa`. The CI migration job runs
   file transaction tests on Linux, macOS and Windows.

Test registries use synthetic steps to exercise future versions without
claiming to support an unreleased historical schema.

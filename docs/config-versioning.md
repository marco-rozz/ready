# Configuration versions and upgrades

Every READY configuration must declare its schema explicitly:

```yaml
config_version: 1
```

This integer describes the file format, independently of the READY release
version. The initial supported schema is 1. There is no conversion from an
unversioned file or an obsolete development layout. If a file already follows
the current example, add the marker manually; otherwise update its layout
using `config.example.yaml`. Merely changing the marker never converts fields.

## Starting and upgrading

`ready serve` elects exactly one config (`--config`, local `.ready/config.yaml`,
then the global Install-home file) and checks its version before starting
services. A current configuration is read without rewriting it or creating
migration artifacts, even in a directory without write permission. Installers
continue to copy config only when missing; reinstalling preserves your file.

When future versions require a schema transformation, `serve` automatically
applies every intervening step, validates the final result, and only then
creates a backup and replaces the file. Skipping releases is supported. An
error before replacement leaves the original unchanged. Remote LLM, MCP and HA
availability and interactive OAuth login are not prerequisites for migration.
Ordinary configuration validation, including required secret-file reads, still
applies. Optional runtime defaults are not written into the file.

All `ready login` modes, including `--status`, `--forget` and `--device-code`,
read configuration without migrating it. If an upgrade is needed, login prints
the current/target versions and a quoted command for that exact file, such as:

```sh
ready serve --config '/path/to/config.yaml'
```

That command also starts the daemon; stop an older daemon before using it.
On Windows, the displayed command uses PowerShell quoting. `ready chat` never
loads a configuration file. A config newer than the binary is rejected: use
a compatible newer READY binary, rather than lowering the marker by hand.

## Files and recovery

Migration preserves comments, unknown keys, untouched values and key order;
whitespace may normalize. The marker is placed first in migrated output.
Backups contain the exact original bytes, including any inline secrets.

For a config named `house.yaml`, migration files live beside it:

- `house.yaml.bak.<UTC timestamp>.<unique suffix>`: original backup; the
  newest three recognized backups for this basename are retained after a
  successful migration. Other configs and manually named backups are excluded.
- `house.yaml.migration.lock`: stable advisory lock file, created only when
  migration is required and retained after release. Its existence does not
  mean a migration is active.
- `.house.yaml.migration-<unique suffix>`: temporary candidate; removed on
  ordinary pre-commit failure. A process crash can leave a temporary file.

On Unix backup and temporary files use 0600 or stricter source permissions;
the replacement config retains its original permission bits. On Windows the
source DACL is applied before writing content, and replacement carries those
access restrictions. Keep the whole directory access-controlled.

Required migrations need a writable directory for locks, backups and temporary
files. Migration refuses a symlink leaf or a file with multiple hard links:
copy it to a regular unlinked file and select that file explicitly. Current
schema reads still support links. READY serializes its migration writers and
checks for external changes before replacement. Advisory locks cannot exclude
an arbitrary editor in the final check/replace interval; edit while stopped.

Failure to prune an old backup produces a warning and keeps the committed
config. A failure after replacement, such as logger startup, reports that the
new schema is already committed and names its backup; it does not silently
roll back a successful migration.

To restore manually, stop READY, inspect and copy the selected backup to the
config path with appropriate permissions, and use a binary compatible with
that backup's schema. Starting the latest binary again may upgrade it again.
No automatic downgrade or runtime-data migration is provided.

## Adding a migration

The engine and explicit production registry live in `internal/config`.

1. Determine whether a persisted transformation is necessary. A compatible
   optional field using a runtime default does not inherently require one.
2. Add a `Migration` implementing `Version()` (the next destination integer),
   `Description()` and `Up(*yaml.Node) error`, with a compile-time assertion.
   Transform only the authored document; perform no external actions. The
   engine owns `config_version` updates.
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

The initial production registry is intentionally empty. Test registries use
synthetic steps to exercise future versions without claiming to support an
unreleased historical schema.

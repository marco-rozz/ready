# Install, upgrade, and where READY. keeps its files

## Install

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | bash
```

Windows (PowerShell 5.1+):

```powershell
irm https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.ps1 | iex
```

Options cannot ride on a piped command directly; use the parameter-passing
forms (`curl … | bash -s -- --no-modify-path`, or
`& ([scriptblock]::Create((irm <url>))) -Uninstall`).

Installer options (same meanings on both platforms):

- `--no-modify-path` / `-NoModifyPath` — skip the persistent PATH edit.
  Manual instructions are printed either way. Default behaviour is a
  conservative, shell-aware edit: the Unix installer edits at most one rc
  file of the user's login shell in an idempotent `# >>> ready >>>` block;
  Windows appends only to the user registry PATH, preserving the value kind
  and the original bytes (start a new shell to see it).
- `--uninstall` / `-Uninstall` — removes the owned program, launcher and
  shipped skill library. PATH entries and ALL agent data are never touched.
- `--purge` / `-Purge` (with `--uninstall`) — additionally deletes the
  DEFAULT agent directory `~/.ready` (config, transcripts, OAuth
  credentials, runtime skills), after explicit confirmation (`--yes`
  skips it when there is no TTY). It refuses while a daemon or mutating
  login is active on that directory, and never discovers or removes
  explicit `--agent-dir` directories.

Pin the downloaded release by passing `VERSION` to the installer process:

```sh
curl -fsSL https://raw.githubusercontent.com/marco-rozz/ready/main/scripts/install.sh | VERSION=v0.2.0 bash
```

The tag must start with `v`. In PowerShell, set `$env:VERSION = 'v0.2.0'`
before invoking the installer; remove it afterwards with `Remove-Item Env:VERSION`.
The version is an example of an existing tag, not a recommendation to downgrade.

Platform notes an installer will enforce:

- Linux amd64/arm64 binaries are dynamically linked and need the glibc
  loader; Alpine/musl, NixOS and distroless hosts are unsupported. `armv6`
  is fully static.
- Windows ships an AMD64 build (ARM64 Windows runs it under emulation).
- SHA256 verification guards integrity (corruption, truncation), not
  authenticity: the trust root is the GitHub repository and TLS.
- Root (Unix) or an elevated token (Windows) is refused for install,
  uninstall and purge: READY installs per-user only.

Upgrade is idempotent: re-running replaces the program and the shipped skill
library wholesale, **keeps every agent's `config.yaml`**, never touches
runtime skill copies or transcripts, and ends with the fixed line
`restart ready serve to use the new version` (`ready.exe` on Windows). The installer never stops or
starts a daemon.

The installer never creates an agent config; it keeps the example in program
storage. After installation, open a new terminal and run ready init
(ready.exe init on Windows). A new default agent gets the full wizard;
existing configs are preserved and init offers offline repair.

## How READY. selects its agent directory

`ready serve` and every `ready login` mode accept `--agent-dir <directory>`
and read exactly `<agent-dir>/config.yaml` — the sole accepted filename.
Without the flag the default is `~/.ready` (`%USERPROFILE%\.ready` on
Windows), ALWAYS, independent of the working directory: no cwd discovery, no
merging, no fallback. An explicitly selected directory must exist and hold
`config.yaml`, otherwise the run fails naming that path — no other agent is
adopted and nothing is created automatically.

The agent directory is the root of every runtime path: `sessions/`, the
runtime skills copy, the log segments under `logging.file_path`, and the agent's
encrypted Secret store (`secrets.age` + its key; `oauth.path` is a removed key
and is rejected at load). Configured writable
paths must stay inside the agent directory after resolving links. Origins
are reported as `flag` / `default` in the boot INFO.

`ready serve` logs the directory at INFO (and warns when the config is
group/other-readable on unix); `ready login` prints `using agent directory
<path>`, so a login run against the wrong agent is visible immediately. The
interactive `ready chat` TUI shows the directory too — reported by the
daemon itself after authentication. See `agent-directories.md` for the full
isolation contract.

## Config schema version

Every complete configuration requires top-level `config_version: 2`, independent
of the READY release version. A schema-1 file is upgraded by `ready serve`: the
credentials it held (inline or in agent-local `*_file` files) move into the
encrypted Secret store, the fields become `!secret` references, the previous
config is kept as an encrypted `.bak….age` and the imported plaintext files are
deleted. An unversioned file must be checked against the current example and
marked manually; adding the marker does not convert an obsolete layout.

Future schema migrations run automatically only in `ready serve`, before runtime
startup: all steps are applied in memory and validated before a protected backup
and atomic replacement. Comments and untouched YAML values survive; whitespace
may normalize. Current-version files are never rewritten. All `login` modes
remain read-only for config and print a safely quoted same-directory
`serve --agent-dir` command when migration is needed; that command also starts
the daemon. A newer schema requires a compatible newer binary; never lower the
marker to force a downgrade.

Backups are siblings named `<config-name>.bak.<UTC timestamp>.<unique suffix>[.age]`;
the newest three recognized backups for that config are retained after success.
The sibling `<config-name>.migration.lock` persists but only holds an OS lock
during migration. Required upgrades need directory write access and a regular
file with one hard link (no symlink leaf). Stop READY while editing or restoring
a selected backup, and restore a binary compatible with that schema as needed.
Backup-pruning failures warn without undoing a committed migration. A later boot
failure reports the committed state and backup path rather than reverting it.

## Install layout

```
~/.local/share/ready/bin/ready        # the program body (Unix)
~/.local/bin/ready                    # launcher → ../share/ready/bin/ready
~/.local/share/ready/config.example.yaml # installed reference, not agent state
~/.local/share/ready/library/skills   # shipped skill library (installer-owned)

~/.ready/                             # the DEFAULT agent directory
├── config.yaml                       # sole config (created by init, 0600)
├── skills/                           # runtime skill copy (prepared by init, updated by serve)
├── sessions/                         # transcripts
├── secrets.age / secrets.key         # encrypted Secret store and its key
└── oauth/                            # per-target coordination locks only
```

Windows installs `ready.exe` at `%USERPROFILE%\.local\bin\ready.exe` and the
library at `%USERPROFILE%\.local\share\ready\library\skills` — the agent
directory and its state are identical in shape.

The library is the source of truth, replaced on every install. Runtime
copies are first prepared by init after saving the configuration. They live
under each agent directory and are reconciled **per file** at
each boot (see `config-runtime.md`, `skills:`): shipped fixes land, your
edits are never overwritten (conflicts are reported, not clobbered, in
`skills.sync: auto`; with `notify` nothing is written at all), your
deletions are respected, and `generated/` assets always belong to the twin
snapshotter, never to the library.

## Runtime data sensitivity

Session transcripts carry device states and conversations; logging
directories, OAuth records and the log lock file are access-controlled
(0700 / 0600, enforced even on pre-existing directories). Keep all of these
on a local filesystem: the logging advisory lock is unreliable on NFS/SMB.

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
forms (`curl … | bash -s -- --path`, or
`& ([scriptblock]::Create((irm <url>))) -Uninstall`).

Installer options (same meanings on both platforms):

- `--prefix <dir>` — binary directory. Default `$HOME/.ready/bin`
  (Windows: `%LOCALAPPDATA%\Programs\ready\bin`).
- `--global` — sh only: `/usr/local/bin` via sudo; mutually exclusive with
  `--prefix`.
- `--path` — sh only: default is print-only; opting in appends an idempotent
  PATH block to the user's rc file. Windows writes the user PATH registry
  automatically (start a new shell to see it).
- `--uninstall` — removes the binary and PATH entries, keeps data.
- `--purge` (with `--uninstall`) — also deletes the install home, asking for
  confirmation (`--yes` skips it when there is no TTY).

Pin a version: `VERSION=v0.1.2 curl … | bash -s --` (must start with `v`).

Platform notes an installer will enforce:

- Linux amd64/arm64 binaries are dynamically linked and need the glibc
  loader; Alpine/musl, NixOS and distroless hosts are unsupported. `armv6`
  is fully static.
- Windows ships an AMD64 build (ARM64 Windows runs it under emulation).
- SHA256 verification guards integrity (corruption, truncation), not
  authenticity: the trust root is the GitHub repository and TLS.
- Running as root without `--global` aborts (sh installer).

Upgrade is idempotent: re-running replaces the binary and the shipped skill
library wholesale, **keeps the user's `config.yaml`**, never touches runtime
skill copies or transcripts, and ends with the fixed line
`restart ready serve to use the new version`. The installer never stops or
starts a daemon.

## How READY. finds its config

Exactly one config file is elected, in strict order, no merging:

1. explicit `--config <path>` (a missing flagged file fails the run naming
   that path);
2. `./.ready/config.yaml` relative to the working directory;
3. `~/.ready/config.yaml` (the Install home; `%USERPROFILE%\.ready\config.yaml`
   on Windows).

Origins are reported as `flag`, `local`, `global` in the boot INFO.

**Runtime home rule.** The directory containing the elected config is the
root of every runtime path: `sessions/`, `skills/`, any relative
`logging.file_path`, `skills.library_path` overrides. A deployment-local run
keeps its data next to its config; a global-config run keeps everything
inside the Install home.

The one carve-out: the OAuth credential store anchors to the Install home
(`~/.ready/oauth/`) by default, because a credential is per-user identity,
not deployment data. An explicitly configured `oauth.path` (even relative)
follows the runtime home like everything else.

`ready serve` logs the elected config at INFO (and warns when it is
group/other-readable on unix); `ready login` prints `using config <path>`,
so a login run from the wrong directory is visible immediately.

## Install home layout

```
<install home>/            # ~/.ready  (or %USERPROFILE%\.ready)
├── bin/ready              # the binary (unless --prefix/--global)
├── config.yaml            # global config fallback (copy-if-missing, 0600)
├── library/skills/        # shipped skill library (installer-owned)
├── skills/                # runtime skill copy (daemon-written at boot sync)
├── sessions/              # transcripts (global-config runs)
└── oauth/                 # default OAuth credential store
```

The library is the source of truth, replaced on every install. Runtime
copies live under the runtime home and are reconciled **per file** at each
boot (see `config-runtime.md`, `skills:`): shipped fixes land, your edits
are never overwritten (conflicts are reported, not clobbered, in
`skills.sync: auto`; with `notify` nothing is written at all), your
deletions are respected, and `generated/` assets always belong to the twin
snapshotter, never to the library.

## Runtime data sensitivity

Session transcripts carry device states and conversations; logging
directories, OAuth records and the log lock file are access-controlled
(0700 / 0600, enforced even on pre-existing directories). Keep all of these
on a local filesystem: the logging advisory lock is unreliable on NFS/SMB.

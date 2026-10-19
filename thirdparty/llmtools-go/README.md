# LLM Tools for Go

[![License: MIT](https://img.shields.io/badge/License-MIT-brightgreen.svg)](https://opensource.org/licenses/MIT)
[![lint](https://github.com/flexigpt/llmtools-go/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/flexigpt/llmtools-go/actions/workflows/lint.yml)
[![test](https://github.com/flexigpt/llmtools-go/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/flexigpt/llmtools-go/actions/workflows/test.yml)

Go-native, cross-platform tools for common local tasks, plus a small registry that makes them easy to expose to LLM tool-calling systems.

## Table of contents <!-- omit in toc -->

- [Features at a glance](#features-at-a-glance)
  - [File system tools](#file-system-tools)
  - [Execute tools](#execute-tools)
  - [Text processing tools](#text-processing-tools)
  - [Image tools](#image-tools)
  - [Git tools](#git-tools)
  - [Web fetching tools](#web-fetching-tools)
- [Package overview](#package-overview)
- [Registry](#registry)
- [Tool outputs](#tool-outputs)
- [Path policy](#path-policy)
- [Examples](#examples)
- [Exec tool notes](#exec-tool-notes)
  - [Quick model](#quick-model)
  - [Shell selection and bootstrap defaults](#shell-selection-and-bootstrap-defaults)
  - [Working directory behavior](#working-directory-behavior)
  - [Environment behavior](#environment-behavior)
  - [Shell sessions: what persists and what does not](#shell-sessions-what-persists-and-what-does-not)
  - [Command lookup and login-shell behavior](#command-lookup-and-login-shell-behavior)
  - [Run Script behavior](#run-script-behavior)
  - [Execution limits and blocking](#execution-limits-and-blocking)
  - [Important behavioral notes](#important-behavioral-notes)
- [Development](#development)
- [License](#license)

## Features at a glance

- Cross-platform implementations for Linux, macOS, and Windows.
- Instance-owned tools with configurable path policy.
- Deterministic text-editing and unified-diff patching tools for line-oriented workflows.
- Shell command and script execution with timeouts, output caps, and command blocking.
- Structured tool manifests and normalized outputs for text, files, and images.

### File system tools

Grouped under `fstool`.

- `readfile`
  - `encoding=text`: reads UTF-8 text and supports extracted text for PDFs.
  - `encoding=binary`: returns base64, emitting `image` outputs for `image/*` MIME types and `file` outputs otherwise.
  - Safety: size caps and symlink hardening.

- `writefile`
  - `encoding=text`: write UTF-8 content.
  - `encoding=binary`: write base64-decoded bytes.
  - Options: `overwrite`, `createParents` (bounded), atomic writes, size caps, symlink hardening.

- `deletefile`
  - Safe delete by moving to trash.
  - `trashDir=auto` tries system trash when possible and otherwise falls back to a local `.trash` directory.

- `searchfiles`: recursively search file paths and UTF-8 text content using RE2 regex.
- `listdirectory`: list entries under a directory, optionally filtered by glob.
- `statpath`: inspect a path.
- `mimeforpath`: best-effort MIME type detection.
- `mimeforextension`: MIME lookup for an extension.

### Execute tools

Grouped under `exectool`.

- `shellcommand`
  - Execute one or more shell commands via a selected shell.
  - Supports session-like persistence for `workDir` and `env` across calls.
  - Sessions are not persistent shell processes.

- `runscript`
  - Run an existing script from disk with arguments and environment overrides.
  - Uses extension-based interpreter selection via `RunScriptPolicy`.

### Text processing tools

Grouped under `texttool`.

- `readtextrange`: read `n` text lines from a line number.
- `findtext`: find matches with context.
- `inserttext`: insert text at start/end/between text blocks.
- `replacetext`: replace text, with optional disambiguation using above/below text.
- `deletetext`: delete text, with optional disambiguation using above/below text.
- `applyunifieddiff`
  - Apply or dry-run Git-style unified diffs against local UTF-8 text files.
  - Supports multi-file patches, create/delete edits, mode-only changes, no-newline-at-EOF markers, and safe fuzzy hunk matching.
  - Accepts explicit `fileTargets` when diff paths are missing, wrong, relative to a different base, or ambiguous.
  - Accepts optional `candidatePaths` to help resolve patch targets by exact path, suffix, or unique basename.
  - Tracks already-applied changes and returns structured per-file results plus reusable `fileTargets`.
  - Set `dryRun=true` to inspect applicability without writing files.
  - Set `strict=true` to require exact hunk matching only.
  - The parser is intentionally tolerant of common malformed or LLM-generated diffs.

### Image tools

Grouped under `imagetool`.

- `readimage`: read intrinsic metadata and optionally include base64 content.

### Git tools

- `gittool` provides pure-Go local Git repository inspection and editing.
- Read-oriented tools include `gitstatus`, `gitdiff`, `gitlog`, `gitshow`, `gitbranch`, `gittag`, `gitchangedfiles`, `gitlisttree`, `gitreadblob`, `gitfindrepos`, `gitblame`, `gitfilehistory`, `gitrepoinfo`, and `gitgrep`.
- Write-oriented tools include `gitadd`, `gitreset`, `gitcommit`, `gitinit`, `gitcreatebranch`, `gitcheckout`, `gitcreatetag`, and `gitdeletetag`.

### Web fetching tools

- `webtool` provides `fetchurl` for HTTP and HTTPS URLs.
- In `auto` mode, HTML, text, and PDF responses return compact text, while binary responses return `image` or `file` outputs.
- `fetchurl` also supports `encoding=text` and `encoding=binary`, `startIndex` for truncated text continuation, and `raw=true` for decoded text without Markdown extraction.
- By default, localhost, private, link-local, multicast, and reserved addresses are blocked and redirects are revalidated.

## Package overview

- `llmtools`: registry and tool registration helpers.
- `spec`: tool manifests and output union types.
- `fstool`: filesystem tools.
- `exectool`: shell command execution and script execution.
- `texttool`: safe, deterministic line-based text editing and unified-diff patching tools.
- `imagetool`: image tools.
- `gittool`: pure-Go local Git repository tools for reading, editing, and history inspection.
- `webtool`: HTTP/HTTPS fetching with compact text extraction and binary/file outputs.

## Registry

The registry provides:

- tool registration and lookup by `spec.FuncID`
- stable manifest ordering (`Tools()` sorted by slug + funcID)
- per-registry default call timeout via `WithDefaultCallTimeout`
- per-call timeout override via `llmtools.WithCallTimeout(...)`
- panic-to-error recovery around tool execution
- builtin registration for filesystem, image, execute, git, text, and web fetch tools via `NewBuiltinRegistry()`

## Tool outputs

- `Registry.Call` returns `[]spec.ToolOutputUnion`.
- Most tools are registered via `RegisterTypedAsTextTool`, which wraps the Go result as JSON and returns it as a single `text` output item.
- Tools that naturally return binary or image data can emit typed `image` or `file` outputs.

## Path policy

All tools are instance-owned. Hosts can configure:

- `workBaseDir`: base directory for resolving relative paths
- `allowedRoots`: optional allowlist roots; when set, resolved tool paths must stay within these roots
- `blockSymlinks`: optional symlink hardening for supported operations

This is path policy, not a general OS sandbox for child processes.

## Examples

All examples are provided as end-to-end integration tests that:

- start from a registry
- register tools scoped to a temp directory
- execute realistic read/modify loops, text edits, shell sessions, script execution, and binary/image workflows

Examples:

- Text read/modify loop: [`text test`](internal/integration/text_test.go)
- Filesystem, MIME, safe delete, binary, and image flows: [`fs + image test`](internal/integration/fs_image_test.go)
- Shell sessions, environment persistence, and `runscript`: [`exec test`](internal/integration/exec_test.go)

## Exec tool notes

### Quick model

For `exectool`, the most useful mental model is:

- `workBaseDir` chooses the default base for relative paths.
- `shellcommand.workDir` chooses the actual child-process working directory.
- `shellcommand.env` and `runscript.env` add per-call environment overrides.
- Shell sessions persist tool-managed `workDir` and `env`, not shell process state.
- Commands are executed in fresh child processes, not in a long-lived interactive terminal.

### Shell selection and bootstrap defaults

`shellcommand` supports explicit shell selection with:

- `auto`
- `bash`, `zsh`, `sh`, `dash`, `ksh`, `fish`
- `pwsh`, `powershell`, `cmd`

`ExecTool` also supports tool-level defaults:

- `WithDefaultShell(...)`
- `WithBaseEnv(...)`

If the caller does not provide them, `NewExecTool(...)` best-effort bootstraps defaults:

- detects a preferred shell
- captures a narrow, execution-oriented base environment

Auto shell detection order is:

- Unix/macOS:
  1. `$SHELL`
  2. account login shell from the host
  3. fallback by platform/tool availability
- Windows:
  1. `pwsh`
  2. `powershell`
  3. `cmd`

You can also call `BootstrapDefaults(ctx)` directly and pass the results yourself.

```go
ctx := context.Background()
defs, _ := exectool.BootstrapDefaults(ctx)

tool, err := exectool.NewExecTool(
   exectool.WithWorkBaseDir(projectDir),
   exectool.WithDefaultShell(defs.DefaultShell),
   exectool.WithBaseEnv(defs.BaseEnv),
)
```

### Working directory behavior

`shellcommand` resolves working directory in this order:

1. `args.workDir`
2. session workdir
3. tool `workBaseDir`

Key points:

- If `workDir` is omitted and there is no session workdir, execution starts in the tool `workBaseDir`.
- If `workBaseDir` is omitted, the path policy falls back to the current process working directory unless `allowedRoots` is set, in which case it defaults to the first canonical allowed root.
- Relative paths are resolved against `workBaseDir` unless a more specific absolute/derived path is supplied by the tool logic.

For stable desktop-app behavior, hosts should usually:

- set `workBaseDir` explicitly
- pass an absolute per-call `workDir` for the active project/workspace

### Environment behavior

Effective environment merge order is:

- `shellcommand`
  1. current process environment
  2. tool base env (`WithBaseEnv` or bootstrapped defaults)
  3. session env
  4. per-call `env`

- `runscript`
  1. current process environment
  2. tool base env (`WithBaseEnv` or bootstrapped defaults)
  3. per-call `env`

Bootstrapped base env is intentionally filtered. It keeps variables that are usually relevant to command and interpreter resolution, for example:

- command lookup: `PATH`, `PATHEXT`
- home/temp/runtime basics: `HOME`, `USERPROFILE`, `TMP`, `TEMP`, `TMPDIR`, `SHELL`, `COMSPEC`, app-data dirs
- locale basics: `LANG`, `LC_*`, `TERM`
- common toolchain/version-manager variables: `ASDF_*`, `PYENV_*`, `NVM_*`, `VOLTA_*`, `SDKMAN_*`, `GOPATH`, `GOBIN`, `JAVA_HOME`, `PNPM_HOME`, `CARGO_HOME`, `CONDA_*`

This avoids importing unrelated prompt/theme variables while still making command lookup and common toolchains behave more like the user’s shell.

### Shell sessions: what persists and what does not

`shellcommand` sessions are persistent tool state, not persistent shell processes.

What persists across calls when you reuse `sessionID`:

- `workDir` passed as a tool argument
- `env` passed as a tool argument

What does not persist:

- `cd ...` issued inside a command string
- `export FOO=...` issued inside a command string
- aliases, functions, shell options, sourced files
- any in-memory shell state

So this works predictably:

```json
{
  "workDir": "/repo",
  "commands": ["go test ./..."]
}
```

But this is not persistent across separate command entries or later calls:

```json
{
  "commands": ["cd /repo", "go test ./..."]
}
```

Each command entry is executed in a fresh shell process.

### Command lookup and login-shell behavior

Working directory and command lookup are separate concerns.

- Working directory controls where the child process starts.
- Command lookup for bare names like `golangci-lint` depends on `PATH` and shell behavior.

Important nuance:

- Commands are executed using non-persistent shell invocations.
- You should not assume they behave exactly like an interactive terminal.
- On Unix, `zsh -c`/`bash -c` behavior differs from an interactive login terminal.
- On PowerShell, execution uses explicit non-interactive flags.

That is why `BootstrapDefaults(...)` exists: it captures a stable, tool-useful environment once, instead of relying on every command to run as an interactive login shell.

For predictable command execution, hosts should prefer:

- explicit `workDir`
- explicit or bootstrapped base env
- explicit `shell` or `WithDefaultShell(...)`
- absolute executable paths when needed

### Run Script behavior

`runscript`:

- executes a pre-existing script from disk
- validates the script path through path policy
- selects how to invoke the script from `RunScriptPolicy`

Default policy includes mappings such as:

- `.sh` -> shell mode with `sh`
- `.bash` -> shell mode with `bash`
- `.zsh` -> shell mode with `zsh`
- `.ps1` -> direct PowerShell execution
- `.py` -> interpreter mode with `python3` on Unix and `python` on Windows

Behavioral notes:

- `runscript` inherits tool base env and per-call env.
- Interpreter availability still depends on the effective environment, especially `PATH`.

### Execution limits and blocking

`ExecutionPolicy` controls:

- timeout
- output caps
- max commands
- max command length

There is also a non-overridable hard blocklist of command names and some heuristic blocking for obviously unsafe shell patterns.

The registry may also impose a call timeout. If you use both registry-level and tool-level timeouts, ensure the registry timeout is not shorter than the exec-tool timeout.

### Important behavioral notes

- `executeParallel=true` does not run commands concurrently. It means commands are treated as independent and execution does not stop on the first non-zero result.
- `shellcommand` executes each command entry separately. If you need shell chaining semantics, put them in one command string, for example `cd repo && go test ./...`.
- `workBaseDir` is a default anchor, not a persistent current-directory concept by itself.
- If you omit both `workBaseDir` and `allowedRoots`, relative/default behavior falls back to the host process working directory.
- If you set `allowedRoots` but omit `workBaseDir`, the base defaults to the first canonical allowed root.
- `allowedRoots` constrains tool-resolved paths; it does not turn child processes into a full OS sandbox.
- For packaged desktop apps, relying on ambient process cwd or ambient process env is usually less stable than passing explicit values.

## Development

- Formatting follows `gofumpt` and `golines` via `golangci-lint`. Rules are in [.golangci.yml](.golangci.yml).
- Useful scripts are defined in `taskfile.yml`; requires [Task](https://taskfile.dev/).
- Bug reports and PRs are welcome:
  - Keep the public API (`package llmtools` and `spec`) small and intentional.
  - Avoid leaking provider-specific types through the public surface; put them under `internal/`.
  - Please run tests and linters before sending a PR.

## License

Copyright (c) 2026 - Present - Pankaj Pipada

All source code in this repository, unless otherwise noted, is licensed under the MIT License.
See [LICENSE](./LICENSE) for details.

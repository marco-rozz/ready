# thirdparty — vendored-patch dependencies

In-repo copies of upstream modules that need a local patch, wired in via
`replace` directives in the root `go.mod`. Each subdirectory is a full copy of
the upstream module at a pinned version, with the patch kept as small as
possible so upstream diffs stay reviewable.

## llmtools-go

- **Upstream**: `github.com/flexigpt/llmtools-go` at **v0.24.5**
- **Why patched**: the upstream `RegisterBuiltins` registry imports and
  registers the `webtool` (pulls `go-dateparser`/`go-trafilatura`) and
  `gittool` (pulls `go-git`) packages. READY never registers those tools as
  agent tools, but linking them costs ~12% of the release binary's size
  (61,866,144 → 54,460,568 bytes on the armv6 stripped build — the motivating
  measurement for Raspberry Pi 1 cold-start).
- **Exact patch** (`registry.go`):
  1. Remove the `gittool` and `webtool` imports.
  2. Remove the `webtool.NewWebTool()` block and its `FetchURLTool`
     registration.
  3. Remove the entire `gittool.NewGitTool()` block and its 19
     `RegisterTypedAsTextTool` registrations.
  Nothing else changes. The `webtool/` and `gittool/` package directories are
  left in place (unimported, therefore never linked); the upstream `LICENSE`
  is retained.
- **Refresh procedure** (upstream upgrade):
  1. Copy the new upstream version over this directory
     (`cp -R $(go env GOMODCACHE)/github.com/flexigpt/llmtools-go@<ver>/ thirdparty/llmtools-go/`).
  2. Re-apply the three-step patch above to `registry.go`.
  3. Update the pinned version here and the `replace` line in `go.mod`.
  4. `go mod tidy` in the repo root; run `go test ./...` and the release
     cross-build gate.
  5. If upstream ships a composable registry or build tags that make the patch
     unnecessary (see the filed upstream issue), drop the `replace` and delete
     this tree.

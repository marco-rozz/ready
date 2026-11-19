# thirdparty — vendored-patch dependencies

In-repo copies of upstream modules that need a local patch, wired in via
`replace` directives in the root `go.mod`. Each subdirectory is a full copy of
the upstream module at a pinned version, with the patch kept as small as
possible so upstream diffs stay reviewable.

## Refreshing a vendored module

`replace`-pinned modules do not update with `go get` — the `require` line is
the pin's documentation, resolution always goes through the directory. An
upgrade is a refresh, not a bump:

1. **Pin** — pick the target upstream version; update it here and in the
   module's entry below.
2. **Copy (mechanical)** — replace the directory's tree with the upstream
   module at the pinned version, e.g.
   `cp -R $(go env GOMODCACHE)/<module>@<ver>/ thirdparty/<module>/`.
3. **Re-apply the patches (judgement, not replay)** — for each documented
   patch below, read the *new* upstream file and re-evaluate the patch's
   intent against it: adapt it to refactored code, or drop it entirely when
   upstream now provides the equivalent. Never replay the old diff blindly.
4. **Verify (deterministic gate)** — `go mod tidy`, `go build ./...`, the
   full Go test suite, plus any patch-specific tests named below. Upstream
   API drift is caught here and fixed in the READY-side call sites.
5. **Document** — record any patch that changed shape, was dropped, or was
   superseded by an upstream feature.

One commit per module, so each diff stays reviewable. A refresh may be run
in an agentic session: steps 1–2 and 4–5 are mechanical and gated by tests;
step 3 is the only judgement call and must be re- reasoned against the
current upstream code, with the test suite as the final authority.

## llmtools-go

- **Upstream**: `github.com/flexigpt/llmtools-go` at **v0.25.0**
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

## Eino

- **Upstream**: `github.com/cloudwego/eino` at **v0.9.22**.
- **Why patched**: READY needs to end tool activity as soon as the consumer
  closes a streaming tool result, including when the upstream stream is idle.
- **Exact patch** (`schema/stream.go`): add `StreamReader.OnClose` (callbacks compose in registration order) and invoke
  its callbacks from `Close`. Existing reader and tool interfaces are otherwise
  unchanged. The callback is registered before the reader is handed off.
- **Refresh**: copy the pinned upstream module, reapply the patch, run the
  activity lifecycle tests and full Go test suite. Remove the replacement when
  upstream exposes an equivalent close notification.

## go-zeromq/zmq4

- **Upstream**: `github.com/go-zeromq/zmq4` at **v0.17.0**.
- **Why patched**: a stalled peer must not hold a READY activity frame open
  indefinitely or receive a later frame after an incomplete one.
- **Exact patch** (`router.go`): add bounded `SendActivity` and guarded
  `SendChecked` methods. Both serialize with the ROUTER writer. Activity
  validates the request immediately before writing; a write failure closes
  the peer before releasing the writer lock, and checked final replies reject
  that failed request. `ready_activity_test.go` exercises a stalled peer.
- **Refresh**: copy the pinned upstream module, reapply the patch, run
  `go test -race -run TestReadyActivity` in `thirdparty/zmq4`, followed by the
  READY ZMQ integration tests. Remove the replacement if upstream provides
  equivalent bounded writes and guarded final delivery.

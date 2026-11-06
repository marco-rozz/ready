package oauth

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Cross-process generation semantics of the coordinated store, verified
// against a re-exec'd helper process (independent processes, not only
// goroutines).
func TestCoordinatedStoreGenerations(t *testing.T) {
	if os.Getenv("READY_OAUTH_HELPER") != "" {
		runOAuthHelper(os.Getenv("READY_OAUTH_HELPER"))

		return // unreachable
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}

	ctx := context.Background()
	store, err := newTestStore(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cred := NewCredential(testServerKey)
	cred.AccessToken = "at"
	if saveErr := store.Save(ctx, cred); saveErr != nil {
		t.Fatal(saveErr)
	}

	// Login (or refresh) bumps the generation; Snapshot observes it.
	if cred.Generation != 1 {
		t.Fatalf("generation after first save = %d, want 1", cred.Generation)
	}

	// Snapshot/compare-commit: a commit from an older snapshot conflicts
	// instead of overwriting newer state.
	snapshot, snapErr := store.Snapshot(ctx, testServerKey)
	if snapErr != nil {
		t.Fatal(snapErr)
	}

	fresher := NewCredential(testServerKey)
	fresher.AccessToken = "newer"
	if saveErr := store.Save(ctx, fresher); saveErr != nil {
		t.Fatal(saveErr)
	}
	if fresher.Generation != 2 {
		t.Fatalf("generation after second save = %d, want 2", fresher.Generation)
	}

	stale := NewCredential(testServerKey)
	stale.AccessToken = "stale"
	if commitErr := store.SaveCommitted(ctx, stale, snapshot); commitErr == nil || !strings.Contains(commitErr.Error(), "changed under us") {
		t.Fatalf("stale commit = %v, want ErrGenerationConflict", err)
	}
	current, loadErr := store.Load(ctx, testServerKey)
	if loadErr != nil || current == nil || current.AccessToken != "newer" {
		t.Fatalf("stored record after stale commit = %+v, %v — the newer record must survive", current, err)
	}

	// Cross-process forget wins over stale work: the helper forgets the
	// target while the parent holds a snapshot; the parent's commit fails
	// and the record stays forgotten.
	snapshot, err = store.Snapshot(ctx, testServerKey)
	if err != nil {
		t.Fatal(err)
	}

	helper := exec.CommandContext(ctx, exe, "-test.run=TestCoordinatedStoreGenerations", "-test.v")
	helper.Env = append(os.Environ(), "READY_OAUTH_HELPER=forget", "READY_OAUTH_DIR="+store.dir)
	if out, runErr := helper.CombinedOutput(); runErr != nil {
		t.Fatalf("forget helper: %v out=%s", runErr, out)
	}

	if commitErr := store.SaveCommitted(ctx, stale, snapshot); commitErr == nil || !strings.Contains(commitErr.Error(), "changed under us") {
		t.Fatalf("post-forget commit = %v, want the conflict refusal (no resurrection)", err)
	}
	if cur, loadErr := store.Load(ctx, testServerKey); loadErr != nil || cur != nil {
		t.Fatalf("record after forget = %+v, %v — forgotten records must not resurrect", cur, loadErr)
	}

	// A stale load must not serve a forgotten record as current: a fresh
	// token source over a tombstoned target reports login required.
	ts := NewTokenSourceFromCredential(store, nil, stale)
	if _, tokErr := ts.Token(); tokErr == nil || !strings.Contains(tokErr.Error(), "login required") {
		t.Fatalf("token over a forgotten record = %v, want login-required", tokErr)
	}
}

// runOAuthHelper is the re-exec body: forget = delete the named target.
func runOAuthHelper(mode string) {
	dir := os.Getenv("READY_OAUTH_DIR")
	ctx := context.Background()
	if mode == "forget" {
		store, err := openTestStore(dir)
		if err != nil {
			os.Exit(3)
		}
		if err := store.Delete(ctx, testServerKey); err != nil {
			os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(4)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

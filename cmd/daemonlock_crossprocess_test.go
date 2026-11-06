package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/daemonlock"
)

// Cross-process lock races are verified against real independent processes:
// the test binary re-execs itself in a helper mode that holds (or attempts)
// a lock, so goroutine-free contention semantics — flock/LockFileEx across
// processes, aliases included — are exercised the way production hits them.
func TestDaemonLockCrossProcess(t *testing.T) {
	if os.Getenv("READY_LOCK_HELPER") != "" {
		runLockHelper(os.Getenv("READY_LOCK_HELPER"))

		return // unreachable: runLockHelper exits
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	helper := func(mode, dir string) *exec.Cmd {
		cmd := exec.CommandContext(context.Background(), exe, "-test.run=TestDaemonLockCrossProcess", "-test.v")
		cmd.Env = append(os.Environ(), "READY_LOCK_HELPER="+mode, "READY_LOCK_DIR="+dir)

		return cmd
	}

	dir := t.TempDir()

	// 1. startup/startup: the first process owns; the second refuses.
	first := helper("hold", dir)
	first.Stdout, first.Stderr = &testWriter{t}, &testWriter{t}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Process.Kill(); _ = first.Wait() }()

	// Wait until the helper reports ownership through the marker file.
	waitForMarker(t, dir, "OWNED")

	second := helper("try", dir)
	out, runErr := second.CombinedOutput()
	if runErr == nil || !strings.Contains(string(out), "owned by another ready process") {
		t.Fatalf("independent duplicate start = %v, out=%s", runErr, out)
	}

	// 2. startup via an absolute alias path contends on the same owner.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	third := helper("try", alias)
	out, runErr = third.CombinedOutput()
	if runErr == nil || !strings.Contains(string(out), "owned by another ready process") {
		t.Fatalf("alias duplicate start = %v, out=%s", runErr, out)
	}

	// 3. crash/restart without deleting lock files: kill the holder hard,
	// then a fresh acquisition succeeds with the stale file still present.
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()

	if _, statErr := os.Stat(filepath.Join(dir, daemonlock.OwnershipFile)); statErr != nil {
		t.Fatalf("the stable lock file must survive the crash: %v", statErr)
	}

	fourth := helper("try", dir)
	if out, runErr = fourth.CombinedOutput(); runErr != nil {
		t.Fatalf("acquisition after a hard kill failed (stale file must not block): %v, out=%s", runErr, out)
	}
}

// runLockHelper is the re-exec body: hold = acquire + signal the marker +
// block until killed; try = acquire once + print the outcome; exit.
func runLockHelper(mode string) {
	dir := os.Getenv("READY_LOCK_DIR")
	ctx := context.Background()

	switch mode {
	case "hold":
		lock, err := daemonlock.AcquireOwnership(ctx, dir)
		if err != nil {
			os.Stderr.WriteString("HOLD-FAIL " + err.Error() + "\n")
			os.Exit(3)
		}
		marker := filepath.Join(dir, "owned.marker")
		//nolint:gosec // G703: dir is a test temp directory
		if err := os.WriteFile(marker, []byte("OWNED"), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(30 * time.Second) // killed by the test
		_ = lock.Release()
		os.Exit(0)
	case "try":
		lock, err := daemonlock.AcquireOwnership(ctx, dir)
		if err != nil {
			os.Stderr.WriteString("TRY-FAIL " + err.Error() + "\n")
			os.Exit(3)
		}
		_ = lock.Release()
		os.Stdout.WriteString("TRY-OK\n")
		os.Exit(0)
	}
	os.Exit(2)
}

func waitForMarker(t *testing.T, dir, content string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(dir, "owned.marker"))
		if err == nil && strings.Contains(string(data), content) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the ownership helper never signalled its marker")
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Helper()

	return len(p), nil
}

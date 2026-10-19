package fspolicy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mkdirAll(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", p, err)
	}
	return p
}

func writeFile(t *testing.T, p string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", p, err)
	}
	return p
}

func mustNewPolicy(t *testing.T, workBaseDir string, allowedRoots []string, blockSymlinks bool) FSPolicy {
	t.Helper()
	p, err := New(workBaseDir, allowedRoots, blockSymlinks)
	if err != nil {
		t.Fatalf("New(%q, %v, %v) error: %v", workBaseDir, allowedRoots, blockSymlinks, err)
	}
	return p
}

// trySymlink attempts to create a symlink and returns whether it succeeded.
// On Windows, symlink creation may require privileges; on Unix it should work.
func trySymlink(t *testing.T, target, link string) bool {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		// If the platform or environment disallows symlinks, callers can skip.
		t.Logf("symlink not supported/allowed here: os.Symlink(%q, %q): %v (GOOS=%s)", target, link, err, runtime.GOOS)
		return false
	}
	return true
}

func requireExistsDir(t *testing.T, p string) {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%q): %v", p, err)
	}
	if !st.IsDir() {
		t.Fatalf("expected directory at %q", p)
	}
}

func requireErrorIs(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %v, got nil", want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("expected errors.Is(err, %v)=true, got err=%v", want, err)
	}
}

func pathAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("Abs(%q): %v", p, err)
	}
	abs = applySystemRootAliases(abs)
	abs = evalSymlinksBestEffort(abs)
	return abs
}

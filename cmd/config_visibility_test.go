package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// warnConfigPermissions is advisory: a 0644 elected config warns, a 0600 one
// is silent, Windows never triggers the check, and an unstatable file is
// silent too. (The three boot origins of the elected config — flag, local,
// global — are pinned by TestElect_ChainPrecedence in internal/config; Serve
// logs that Resolution's path and origin at INFO.)
func TestWarnConfigPermissions(t *testing.T) {
	dir := t.TempDir()
	path0644 := filepath.Join(dir, "open.yaml")
	if err := os.WriteFile(path0644, []byte("channels:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fixture under test IS an open config — open the permission after
	// the write so gosec stays happy about the creator.
	if err := os.Chmod(path0644, 0o644); err != nil {
		t.Fatal(err)
	}
	path0600 := filepath.Join(dir, "closed.yaml")
	if err := os.WriteFile(path0600, []byte("channels:\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("0644 warns naming the mode", func(t *testing.T) {
		warn := warnConfigPermissions(path0644)
		if warn == "" || !strings.Contains(warn, "readable by group or others") || !strings.Contains(warn, path0644) {
			t.Errorf("warning = %q, want the file and the group/other readability named", warn)
		}
	})

	t.Run("0600 is silent", func(t *testing.T) {
		if warn := warnConfigPermissions(path0600); warn != "" {
			t.Errorf("warning = %q, want empty for a 0600 config", warn)
		}
	})

	t.Run("windows skips the check", func(t *testing.T) {
		oldSeam := onWindows
		onWindows = func() bool { return true }
		t.Cleanup(func() { onWindows = oldSeam })

		if warn := warnConfigPermissions(path0644); warn != "" {
			t.Errorf("warning = %q, want none on Windows", warn)
		}
	})

	t.Run("missing file is silent", func(t *testing.T) {
		if warn := warnConfigPermissions(filepath.Join(dir, "absent.yaml")); warn != "" {
			t.Errorf("warning = %q, want none for an unstatable file", warn)
		}
	})
}

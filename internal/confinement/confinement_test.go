package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates files under dir.
func writeTree(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoot(t *testing.T) {
	t.Run("existing root resolves links", func(t *testing.T) {
		physicalHome := t.TempDir()
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(physicalHome, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		root, err := Root(alias)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := filepath.EvalSymlinks(physicalHome)
		if root != want {
			t.Errorf("Root(alias) = %q, want %q", root, want)
		}
	})

	t.Run("missing leaf resolves through the parent", func(t *testing.T) {
		base := t.TempDir()
		missing := filepath.Join(base, ".ready")
		root, err := Root(missing)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(phys(t, base), ".ready"); root != want {
			t.Errorf("Root = %q, want %q", root, want)
		}
	})
}

func TestCheck(t *testing.T) {
	home := t.TempDir()
	writeTree(t,
		filepath.Join(home, "sessions", "a.jsonl"),
		filepath.Join(home, "skills", "s", "SKILL.md"),
	)

	t.Run("accepts existing descendants and the root itself", func(t *testing.T) {
		if err := Check(home, filepath.Join(home, "sessions", "a.jsonl")); err != nil {
			t.Errorf("existing descendant: %v", err)
		}
		if err := Check(home, home); err != nil {
			t.Errorf("root itself: %v", err)
		}
	})

	t.Run("accepts missing descendants", func(t *testing.T) {
		if err := Check(home, filepath.Join(home, "sessions", "new", "sub", "f.jsonl")); err != nil {
			t.Errorf("missing descendant: %v", err)
		}
	})

	t.Run("rejects absolute destinations outside", func(t *testing.T) {
		outside := t.TempDir()
		err := Check(home, filepath.Join(outside, "evil.jsonl"))
		if err == nil || !strings.Contains(err.Error(), "outside the agent directory") {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("rejects parent traversal", func(t *testing.T) {
		// Construct a sibling path — the equivalent of a resolved ".." escape.
		sibling := filepath.Join(filepath.Dir(home), "sibling", "x.jsonl")
		if err := Check(home, sibling); err == nil {
			t.Error("a sibling destination via traversal must be rejected")
		}
	})

	t.Run("rejects symlinked ancestor resolving outside", func(t *testing.T) {
		outside := t.TempDir()
		writeTree(t, filepath.Join(outside, "real", "f.jsonl"))
		link := filepath.Join(home, "sessions", "out-link")
		if err := os.Symlink(filepath.Join(outside, "real"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := Check(home, filepath.Join(link, "f.jsonl")); err == nil {
			t.Error("a link pointing out of the boundary must be rejected")
		}
	})

	t.Run("accepts a link chain that stays inside", func(t *testing.T) {
		inner := filepath.Join(home, "skills", "alias-target")
		writeTree(t, filepath.Join(inner, "keep.md"))
		link := filepath.Join(home, "sessions", "in-link")
		if err := os.Symlink(inner, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := Check(home, filepath.Join(link, "keep.md")); err != nil {
			t.Errorf("an internal alias must be accepted: %v", err)
		}
	})
}

func phys(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}

	return filepath.Clean(resolved)
}

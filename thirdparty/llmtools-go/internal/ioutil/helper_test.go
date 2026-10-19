package ioutil

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

type searchCoverageTree struct {
	root    string
	alpha   string
	both    string
	content string
	nested  string
	hidden  string
}

func createSearchCoverageTree(t *testing.T) searchCoverageTree {
	t.Helper()

	root := t.TempDir()

	alpha := filepath.Join(root, "Alpha.txt")
	writeFile(t, alpha, "plain alpha content")

	both := filepath.Join(root, "bothneedle.txt")
	writeFile(t, both, "needle")

	content := filepath.Join(root, "content.txt")
	writeFile(t, content, "needle")

	notes := filepath.Join(root, "notes.log")
	writeFile(t, notes, "needle")

	subdir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	nested := filepath.Join(subdir, "nested.txt")
	writeFile(t, nested, "needle")

	hidden := filepath.Join(root, ".hidden.txt")
	writeFile(t, hidden, "needle")

	return searchCoverageTree{
		root:    root,
		alpha:   alpha,
		both:    both,
		content: content,
		nested:  nested,
		hidden:  hidden,
	}
}

func mustWriteBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("failed to write test file %q: %v", path, err)
	}
}

func mustUnmarshalJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("failed to unmarshal %q: %v", s, err)
	}
}

func searchMatchKinds(matches []SearchFileMatch) map[string]SearchFileMatchKind {
	out := make(map[string]SearchFileMatchKind, len(matches))
	for _, match := range matches {
		out[match.Path] = match.MatchKind
	}
	return out
}

func mustWriteFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	full := filepath.Join(dir, name)
	data := bytes.Repeat([]byte("x"), size)
	if err := os.WriteFile(full, data, 0o600); err != nil {
		t.Fatalf("failed to write file %q: %v", full, err)
	}
	return full
}

func mustSymlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		// Often EPERM in CI environments.
		t.Skipf("symlink not supported/allowed: %v", err)
	}
}

// Helper to write text files in tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write test file %q: %v", path, err)
	}
}

// Helper to compare string slices as sets (order-independent).
func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func canceledContext(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx
}

func mustTestPolicy(t *testing.T) fspolicy.FSPolicy {
	t.Helper()
	p, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error creating policy: %v", err)
	}
	return p
}

package texttool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	testTextAOnly   = "A\n"
	testTextAB      = "A\nB\n"
	testTextAC      = "A\nC\n"
	testTextAXB     = "A\nX\nB\n"
	testTextABC     = "A\nB\nC\n"
	testTextAEmptyB = "A\n\nB\n"
	testTextABCRLF  = "A\r\nB\r\n"
	testTextAXBXC   = "A\nX\nB\nX\nC\n"
	testTextAXYC    = "A\nX\nY\nC\n"
	testTextHitTrio = "hit ghit fhit\n"
	testTextEmpty   = ""

	testNameContextCanceled = "context_canceled"

	testErrLineHintMustBeGe1         = "lineHint must be >= 1"
	testErrDeleteMatchCountMismatch  = "delete match count mismatch"
	testErrReplaceMatchCountMismatch = "replace match count mismatch"
	testQueryHit                     = "hit"
	testQueryNope                    = "NOPE"
	testQueryLowerX                  = "x"
	testMissingFileName              = "nope-does-not-exist.txt"
)

// Small OS wrappers to keep this file stdlib-only without extra imports in header
// (and to avoid unused imports in non-symlink environments).
func filepathSep() rune { r, _ := utf8.DecodeRuneInString(string(os.PathSeparator)); return r }

func osSymlink(oldname, newname string) error { return os.Symlink(oldname, newname) }

func filepathAbs(p string) (string, error) { return filepath.Abs(p) }

func newWorkDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	return abs
}

func writeTempTextFile(t *testing.T, dir, pattern, content string) string {
	t.Helper()

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	name := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("WriteString(%q): %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}

	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("Abs(%q): %v", name, err)
	}
	return abs
}

func writeTempBytesFile(t *testing.T, dir, pattern string, b []byte) string {
	t.Helper()

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	name := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		t.Fatalf("Write(%q): %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}

	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("Abs(%q): %v", name, err)
	}
	return abs
}

func canonicalizePath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

func readFileString(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(b)
}

func mustErrContains(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", sub)
	}
	if sub != "" && !strings.Contains(err.Error(), sub) {
		t.Fatalf("expected error containing %q, got %q", sub, err.Error())
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func makeNLines(n int, line func(i int) string, sep string, finalNewline bool) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		sb.WriteString(line(i))
		if i != n || finalNewline {
			sb.WriteString(sep)
		}
	}
	return sb.String()
}

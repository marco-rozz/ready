package fspolicy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	type tc struct {
		name    string
		in      string
		want    string
		wantErr error
	}

	sep := string(os.PathSeparator)

	cases := []tc{
		{
			name: "trims_and_cleans",
			in:   "  a" + sep + "." + sep + "b" + sep + ".." + sep + "c  ",
			want: filepath.Clean("a" + sep + "c"),
		},
		{
			name:    "empty_rejected",
			in:      "   ",
			wantErr: ErrInvalidPath,
		},
		{
			name:    "nul_rejected",
			in:      "a\x00b",
			wantErr: ErrInvalidPath,
		},
		{
			name: "fromslash_then_clean",
			in:   "a/b/../c",
			want: filepath.Clean(filepath.FromSlash("a/b/../c")),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizePath(c.in)
			if c.wantErr != nil {
				requireErrorIs(t, err, c.wantErr)
				return
			}
			if err != nil {
				t.Fatalf("normalizePath(%q) error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("normalizePath(%q)=%q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsPathWithinRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	root = pathAbs(t, root)

	type tc struct {
		name  string
		p     string
		want  bool
		isAbs bool
	}

	cases := []tc{
		{
			name:  "same_dir",
			p:     root,
			want:  true,
			isAbs: true,
		},
		{
			name:  "child",
			p:     filepath.Join(root, "child", "x"),
			want:  true,
			isAbs: true,
		},
		{
			name:  "sibling_outside",
			p:     filepath.Join(filepath.Dir(root), filepath.Base(root)+"-other"),
			want:  false,
			isAbs: true,
		},
		{
			name:  "ancestor_outside",
			p:     filepath.Dir(root),
			want:  false,
			isAbs: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := isPathWithinRoot(root, c.p)
			if err != nil {
				t.Fatalf("isPathWithinRoot(%q,%q) error: %v", root, c.p, err)
			}
			if got != c.want {
				t.Fatalf("isPathWithinRoot(%q,%q)=%v, want %v", root, c.p, got, c.want)
			}
		})
	}
}

func TestEnsureWithinRoots(t *testing.T) {
	t.Parallel()

	root := pathAbs(t, t.TempDir())
	inside := filepath.Join(root, "inside")
	outside := pathAbs(t, t.TempDir())

	type tc struct {
		name      string
		p         string
		roots     []string
		wantError error
	}

	cases := []tc{
		{
			name:      "no_roots_allows_any",
			p:         outside,
			roots:     nil,
			wantError: nil,
		},
		{
			name:      "inside_root_ok",
			p:         inside,
			roots:     []string{root},
			wantError: nil,
		},
		{
			name:      "outside_root_err",
			p:         outside,
			roots:     []string{root},
			wantError: ErrOutsideAllowedRoots,
		},
		{
			name:      "multiple_roots_one_matches",
			p:         outside,
			roots:     []string{pathAbs(t, t.TempDir()), outside},
			wantError: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ensureWithinRoots(c.p, c.roots)
			if c.wantError == nil {
				if err != nil {
					t.Fatalf("ensureWithinRoots(%q,%v) error: %v", c.p, c.roots, err)
				}
				return
			}
			requireErrorIs(t, err, c.wantError)
		})
	}
}

func TestEvalSymlinksBestEffort(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	base = pathAbs(t, base)

	// layout:
	// base/
	//   real/
	//   link -> real/
	realTxt := mkdirAll(t, filepath.Join(base, "real"))
	link := filepath.Join(base, "link")

	if !trySymlink(t, realTxt, link) {
		t.Skip("symlinks not available")
	}

	type tc struct {
		name string
		in   string
		want string
	}

	cases := []tc{
		{
			name: "existing_symlink_resolves",
			in:   link,
			want: filepath.Clean(realTxt),
		},
		{
			name: "nonexistent_child_under_symlink_parent_resolves_parent_then_joins",
			in:   filepath.Join(link, "nope", "file.txt"),
			want: filepath.Join(filepath.Clean(realTxt), "nope", "file.txt"),
		},
		{
			name: "nonexistent_under_normal_dir_returns_cleaned",
			in:   filepath.Join(base, "does-not-exist", "x"),
			want: filepath.Clean(filepath.Join(base, "does-not-exist", "x")),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := evalSymlinksBestEffort(c.in)
			if got != c.want {
				t.Fatalf("evalSymlinksBestEffort(%q)=%q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestDedupeSorted(t *testing.T) {
	t.Parallel()

	type tc struct {
		name string
		in   []string
		want []string
	}

	cases := []tc{
		{
			name: "nil",
			in:   nil,
			want: nil,
		},
		{
			name: "one",
			in:   []string{"a"},
			want: []string{"a"},
		},
		{
			name: "already_unique",
			in:   []string{"a", "b", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "dedupes",
			in:   []string{"a", "a", "b", "b", "b", "c"},
			want: []string{"a", "b", "c"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// Make a copy to avoid aliasing test tables.
			in := append([]string(nil), c.in...)
			got := dedupeSorted(in)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("dedupeSorted(%v)=%v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestCanonicalizeExistingDir_Errors(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	filePath := filepath.Join(base, "file")
	writeFile(t, filePath, []byte("x"))

	type tc struct {
		name      string
		in        string
		wantError string
		checkIs   error
	}

	cases := []tc{
		{
			name:      "nonexistent",
			in:        filepath.Join(base, "nope"),
			wantError: "no such dir",
			checkIs:   os.ErrNotExist,
		},
		{
			name:      "is_file_not_dir",
			in:        filePath,
			wantError: "path is not a directory",
		},
		{
			name:      "nul_rejected",
			in:        base + "\x00",
			wantError: "NUL",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := canonicalizeExistingDir(c.in)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if c.wantError != "" && !strings.Contains(err.Error(), c.wantError) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantError)
			}
			if c.checkIs != nil && !errors.Is(err, c.checkIs) {
				t.Fatalf("expected errors.Is(err,%v)=true, err=%v", c.checkIs, err)
			}
		})
	}
}

func TestCanonicalizeAllowedRoots_DedupesAndSorts(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rootA := mkdirAll(t, filepath.Join(tmp, "a-root"))
	rootB := mkdirAll(t, filepath.Join(tmp, "b-root"))

	roots, err := canonicalizeAllowedRoots([]string{"  ", rootB, rootA, rootA, "", rootB})
	if err != nil {
		t.Fatalf("canonicalizeAllowedRoots error: %v", err)
	}

	want := []string{pathAbs(t, rootA), pathAbs(t, rootB)}
	if len(roots) != len(want) {
		t.Fatalf("len=%d, want %d (roots=%v)", len(roots), len(want), roots)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("roots[%d]=%q, want %q (all=%v)", i, roots[i], want[i], roots)
		}
	}
}

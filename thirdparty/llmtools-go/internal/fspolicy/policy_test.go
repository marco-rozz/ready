package fspolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const errInvalidBaseDirStr = "invalid work base dir"

func TestNew_CanonicalizesSortsAndDefaultsBase(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	rootA := mkdirAll(t, filepath.Join(tmp, "a-root"))
	rootB := mkdirAll(t, filepath.Join(tmp, "b-root"))

	// Intentionally unsorted and with whitespace to exercise trimming.
	p, err := New("", []string{"  " + rootB + "  ", rootA}, false)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	roots := p.AllowedRoots()
	if len(roots) != 2 {
		t.Fatalf("AllowedRoots len=%d, want 2", len(roots))
	}
	if roots[0] != pathAbs(t, rootA) || roots[1] != pathAbs(t, rootB) {
		t.Fatalf("AllowedRoots=%v, want [%q %q]", roots, pathAbs(t, rootA), pathAbs(t, rootB))
	}

	// Default base should be roots[0] after canonicalizeAllowedRoots (sorted).
	if p.WorkBaseDir() != roots[0] {
		t.Fatalf("WorkBaseDir=%q, want %q", p.WorkBaseDir(), roots[0])
	}
}

func TestNew_Errors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	other := mkdirAll(t, filepath.Join(tmp, "other"))

	filePath := filepath.Join(tmp, "file")
	writeFile(t, filePath, []byte("x"))

	type tc struct {
		name    string
		base    string
		roots   []string
		block   bool
		wantIs  error
		wantSub string
	}

	cases := []tc{
		{
			name:    "base_does_not_exist",
			base:    filepath.Join(tmp, "nope"),
			roots:   []string{root},
			wantIs:  os.ErrNotExist,
			wantSub: errInvalidBaseDirStr,
		},
		{
			name:    "base_is_file",
			base:    filePath,
			roots:   []string{root},
			wantSub: errInvalidBaseDirStr,
		},
		{
			name:    "allowed_root_is_file",
			base:    root,
			roots:   []string{filePath},
			wantSub: "invalid allowed root",
		},
		{
			name:    "base_outside_allowed_roots",
			base:    other,
			roots:   []string{root},
			wantIs:  ErrOutsideAllowedRoots,
			wantSub: "work base dir",
		},
		{
			name:    "allowed_root_missing",
			base:    root,
			roots:   []string{filepath.Join(tmp, "missing-root")},
			wantIs:  os.ErrNotExist,
			wantSub: "invalid allowed root",
		},
		{
			name:    "base_contains_nul",
			base:    root + "\x00",
			roots:   []string{root},
			wantSub: errInvalidBaseDirStr,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(c.base, c.roots, c.block)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantSub)
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Fatalf("expected errors.Is(err,%v)=true, err=%v", c.wantIs, err)
			}
		})
	}
}

func TestNew_DefaultsBaseToCWDWhenNoRoots(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	p, err := New("", nil, false)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}

	wantBase := pathAbs(t, cwd)
	if p.WorkBaseDir() != wantBase {
		t.Fatalf("WorkBaseDir=%q, want %q", p.WorkBaseDir(), wantBase)
	}
	if p.HasAllowedRoots() {
		t.Fatalf("HasAllowedRoots()=%v, want false", p.HasAllowedRoots())
	}
	if roots := p.AllowedRoots(); roots != nil {
		t.Fatalf("AllowedRoots()=%v, want nil", roots)
	}

	got, err := p.ResolvePath(filepath.FromSlash("child/grand"), "")
	if err != nil {
		t.Fatalf("ResolvePath error: %v", err)
	}
	want := filepath.Join(wantBase, "child", "grand")
	if got != want {
		t.Fatalf("ResolvePath=%q, want %q", got, want)
	}
}

func TestNew_BlockSymlinksRejectsSymlinkedConfigPaths(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	realRoot := mkdirAll(t, filepath.Join(tmp, "root"))
	realBase := mkdirAll(t, filepath.Join(realRoot, "base"))

	baseLink := filepath.Join(tmp, "base-link")
	rootLink := filepath.Join(tmp, "root-link")

	okBase := trySymlink(t, realBase, baseLink)
	okRoot := trySymlink(t, realRoot, rootLink)
	if !okBase || !okRoot {
		t.Skip("symlinks not available")
	}

	type tc struct {
		name    string
		base    string
		roots   []string
		wantSub string
	}

	cases := []tc{
		{
			name:    "base_symlink_rejected",
			base:    baseLink,
			roots:   []string{realRoot},
			wantSub: "work base dir",
		},
		{
			name:    "allowed_root_symlink_rejected",
			base:    realBase,
			roots:   []string{rootLink},
			wantSub: "allowed root",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(c.base, c.roots, true)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			requireErrorIs(t, err, ErrSymlinkDisallowed)
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestAllowedRoots_ReturnsCopy(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))

	p := mustNewPolicy(t, root, []string{root}, false)

	r1 := p.AllowedRoots()
	if len(r1) != 1 {
		t.Fatalf("len=%d want 1", len(r1))
	}
	r1[0] = "mutated"

	r2 := p.AllowedRoots()
	if r2[0] == "mutated" {
		t.Fatalf("AllowedRoots appears to expose internal slice; got=%v", r2)
	}
}

func TestResolvePath_HappyAndErrors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	base := mkdirAll(t, filepath.Join(root, "base"))

	p := mustNewPolicy(t, base, []string{root}, false)

	other := pathAbs(t, t.TempDir())

	type tc struct {
		name      string
		in        string
		def       string
		want      string
		wantIsErr error
	}

	cases := []tc{
		{
			name: "relative_resolves_against_base",
			in:   filepath.FromSlash("sub/../x"),
			want: filepath.Join(p.WorkBaseDir(), "x"),
		},
		{
			name: "absolute_inside_root_ok",
			in:   filepath.Join(root, "abs", "y"),
			want: filepath.Join(root, "abs", "y"),
		},
		{
			name: "blank_uses_default",
			in:   "   ",
			def:  "d1/d2",
			want: filepath.Join(p.WorkBaseDir(), filepath.FromSlash("d1/d2")),
		},
		{
			name:      "blank_and_default_blank_invalid",
			in:        " ",
			def:       "  ",
			wantIsErr: ErrInvalidPath,
		},
		{
			name:      "nul_invalid",
			in:        "a\x00b",
			wantIsErr: ErrInvalidPath,
		},
		{
			name:      "outside_roots_rejected",
			in:        other,
			wantIsErr: ErrOutsideAllowedRoots,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := p.ResolvePath(c.in, c.def)
			if c.wantIsErr != nil {
				requireErrorIs(t, err, c.wantIsErr)
				return
			}
			if err != nil {
				t.Fatalf("ResolvePath(%q,%q) error: %v", c.in, c.def, err)
			}
			abs, err := filepath.Abs(c.want)
			if err != nil {
				t.Fatalf("Abs(%q): %v", c.want, err)
			}
			abs = applySystemRootAliases(abs)

			if got != abs {
				t.Fatalf("ResolvePath(%q)=%q, want %q", c.in, got, abs)
			}
		})
	}
}

func TestVerifyDir_SymlinksAllowed(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, false)

	existingDir := mkdirAll(t, filepath.Join(root, "d"))
	existingFile := writeFile(t, filepath.Join(root, "f.txt"), []byte("x"))
	missing := filepath.Join(root, "missing")

	type tc struct {
		name      string
		in        string
		wantIsErr error
		wantSub   string
	}

	cases := []tc{
		{name: "existing_dir_ok", in: existingDir},
		{name: "missing_err", in: missing, wantIsErr: os.ErrNotExist},
		{name: "file_err", in: existingFile, wantSub: "not a directory"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := p.VerifyDirResolved(c.in)
			if c.wantIsErr == nil && c.wantSub == "" {
				if err != nil {
					t.Fatalf("VerifyDir(%q) error: %v", c.in, err)
				}
				return
			}
			if c.wantIsErr != nil {
				if !errors.Is(err, c.wantIsErr) {
					t.Fatalf("expected errors.Is(err,%v)=true; err=%v", c.wantIsErr, err)
				}
			}
			if c.wantSub != "" && (err == nil || !strings.Contains(err.Error(), c.wantSub)) {
				t.Fatalf("expected error containing %q; got %v", c.wantSub, err)
			}
		})
	}
}

func TestEnsureDir_SymlinksAllowed_Creates(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, false)

	target := filepath.Join(root, "a", "b", "c")
	created, err := p.EnsureDirResolved(target, 0)
	if err != nil {
		t.Fatalf("EnsureDir error: %v", err)
	}
	if created != 0 {
		t.Fatalf("created=%d, want 0 when symlinks allowed", created)
	}
	requireExistsDir(t, target)
}

func TestEnsureDir_BlockSymlinks_CountAndMax(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, true)

	// Pre-create one parent to make "created" count deterministic.
	_ = mkdirAll(t, filepath.Join(root, "a"))

	type tc struct {
		name      string
		targetRel string
		maxNew    int
		wantMade  int
		wantErr   string
	}

	cases := []tc{
		{
			name:      "creates_two_missing_components",
			targetRel: filepath.FromSlash("a/b/c"),
			maxNew:    0,
			wantMade:  2, // b and c
		},
		{
			name:      "maxNewDirs_blocks_before_second_create",
			targetRel: filepath.FromSlash("a/d/e"),
			maxNew:    1,
			wantMade:  1,
			wantErr:   "too many parent directories to create",
		},
		{
			name:      "maxNewDirs_exact_allows",
			targetRel: filepath.FromSlash("a/f/g"),
			maxNew:    2,
			wantMade:  2,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			target := filepath.Join(root, c.targetRel)
			made, err := p.EnsureDirResolved(target, c.maxNew)
			if made != c.wantMade {
				t.Fatalf("EnsureDir made=%d, want %d", made, c.wantMade)
			}
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("EnsureDir error: %v", err)
				}
				requireExistsDir(t, target)
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("expected error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestRequireExistingRegularFile_SymlinksAllowed(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, false)

	regular := writeFile(t, filepath.Join(root, "file.txt"), []byte("x"))
	dir := mkdirAll(t, filepath.Join(root, "dir"))
	missing := filepath.Join(root, "missing.txt")

	type tc struct {
		name       string
		in         string
		wantErrSub string
		wantIs     error
	}

	cases := []tc{
		{name: "regular_ok", in: regular},
		{name: "missing_err", in: missing, wantIs: os.ErrNotExist},
		{name: "dir_err", in: dir, wantErrSub: "expected file but got directory"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.RequireExistingRegularFileResolved(c.in)
			if c.wantIs == nil && c.wantErrSub == "" {
				if err != nil {
					t.Fatalf("RequireExistingRegularFile(%q) error: %v", c.in, err)
				}
				return
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Fatalf("expected errors.Is(err,%v)=true, err=%v", c.wantIs, err)
			}
			if c.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), c.wantErrSub)) {
				t.Fatalf("expected error containing %q, got %v", c.wantErrSub, err)
			}
		})
	}
}

func TestRequireExistingRegularFileResolved_BlockSymlinksRefusesSymlinkParent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	realDir := mkdirAll(t, filepath.Join(root, "real"))
	regular := writeFile(t, filepath.Join(realDir, "file.txt"), []byte("x"))
	linkDir := filepath.Join(root, "link")

	if !trySymlink(t, realDir, linkDir) {
		t.Skip("symlinks not available")
	}

	p := mustNewPolicy(t, root, []string{root}, true)

	t.Run("real_file_ok", func(t *testing.T) {
		t.Parallel()
		if _, err := p.RequireExistingRegularFileResolved(regular); err != nil {
			t.Fatalf("RequireExistingRegularFileResolved(%q) error: %v", regular, err)
		}
	})

	t.Run("symlink_parent_refused", func(t *testing.T) {
		t.Parallel()
		resolved, err := p.ResolvePath(filepath.Join("link", "file.txt"), "")
		if err != nil {
			t.Fatalf("ResolvePath error: %v", err)
		}

		_, err = p.RequireExistingRegularFileResolved(resolved)
		requireErrorIs(t, err, ErrSymlinkDisallowed)
		if !strings.Contains(err.Error(), "refusing to traverse symlink path component") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestAllowedRootsEmpty_AllowsAnyAbsolutePath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	base := mkdirAll(t, filepath.Join(tmp, "base"))
	p := mustNewPolicy(t, base, nil, false)

	outside := pathAbs(t, t.TempDir())

	got, err := p.ResolvePath(outside, "")
	if err != nil {
		t.Fatalf("ResolvePath(outside) error: %v", err)
	}
	if got != filepath.Clean(outside) {
		t.Fatalf("ResolvePath=%q want %q", got, filepath.Clean(outside))
	}
}

func TestConcurrency_ResolvePathAndEnsureDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, true)

	// Pre-create shared parent to avoid goroutine races on the same mkdir.
	shared := mkdirAll(t, filepath.Join(root, "shared"))

	const goroutines = 40
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()

			// Resolve concurrently.
			rel := filepath.Join("shared", fmt.Sprintf("d-%d", i), "..", fmt.Sprintf("d-%d", i), "nested")
			abs, err := p.ResolvePath(rel, "")
			if err != nil {
				errCh <- fmt.Errorf("ResolvePath: %w", err)
				return
			}
			checkPath := applySystemRootAliases(shared + string(os.PathSeparator))
			// Canonicalize for allowed-root comparisons (symlinks/junctions/8.3 names).
			checkPath = evalSymlinksBestEffort(checkPath)
			if !strings.HasPrefix(abs, checkPath) && abs != shared {
				errCh <- fmt.Errorf("resolved path not under shared: %q", abs)
				return
			}

			// EnsureDir concurrently on distinct targets.
			_, err = p.EnsureDirResolved(abs, 0)
			if err != nil {
				errCh <- fmt.Errorf("EnsureDir: %w", err)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		t.Fatalf("concurrency test failed (GOOS=%s)", runtime.GOOS)
	}
}

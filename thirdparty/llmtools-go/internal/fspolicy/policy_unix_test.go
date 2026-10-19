//go:build !windows

package fspolicy

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestBlockSymlinks_VerifyDirRefusesSymlinkComponent(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	realTxt := mkdirAll(t, filepath.Join(root, "real"))
	link := filepath.Join(root, "link")

	if !trySymlink(t, realTxt, link) {
		t.Skip("symlinks not available")
	}

	p := mustNewPolicy(t, root, []string{root}, true)

	// Allowed-roots check should pass (symlink points within root), but traversal should be refused.
	err := p.VerifyDirResolved(link)
	requireErrorIs(t, err, ErrSymlinkDisallowed)
	if !strings.Contains(err.Error(), "refusing to traverse symlink path component") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBlockSymlinks_RequireExistingRegularFileRefusesSymlinkFile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	realFile := writeFile(t, filepath.Join(root, "real.txt"), []byte("x"))
	linkFile := filepath.Join(root, "link.txt")

	if !trySymlink(t, realFile, linkFile) {
		t.Skip("symlinks not available")
	}

	p := mustNewPolicy(t, root, []string{root}, true)

	_, err := p.RequireExistingRegularFileResolved(linkFile)
	requireErrorIs(t, err, ErrSymlinkDisallowed)
	if !strings.Contains(err.Error(), "refusing to operate on symlink file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAllowedRoots_SymlinkEscapeRejectedEvenWhenSymlinksAllowed(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	outside := mkdirAll(t, filepath.Join(tmp, "outside"))

	escape := filepath.Join(root, "escape")
	if !trySymlink(t, outside, escape) {
		t.Skip("symlinks not available")
	}

	// BlockSymlinks=false: still must not be able to escape allowed roots after symlink resolution.
	p := mustNewPolicy(t, root, []string{root}, false)

	_, err := p.ResolvePath(filepath.Join("escape", "nonexistent.txt"), "")
	requireErrorIs(t, err, ErrOutsideAllowedRoots)
}

func TestApplySystemRootAliases(t *testing.T) {
	t.Parallel()

	type tc struct {
		name string
		in   string
		want string
	}

	// Only darwin should rewrite these.
	wantVar := filepath.Clean("/var/log")
	if runtime.GOOS == toolutil.GOOSDarwin {
		wantVar = filepath.Clean("/private/var/log")
	}

	cases := []tc{
		{
			name: "cleaning_always_happens",
			in:   "/a/b/../c",
			want: filepath.Clean("/a/c"),
		},
		{
			name: "darwin_alias_var",
			in:   "/var/log",
			want: wantVar,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := applySystemRootAliases(c.in)
			if got != c.want {
				t.Fatalf("applySystemRootAliases(%q)=%q, want %q (GOOS=%s)", c.in, got, c.want, runtime.GOOS)
			}
		})
	}
}

func TestAllowSystemSymlink_DarwinAliases(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != toolutil.GOOSDarwin {
		t.Skip("darwin only")
	}

	tested := false
	for alias, expected := range darwinSystemSymlinkAliases {
		if _, err := os.Readlink(alias); err != nil {
			continue
		}

		resolved, ok, err := allowSystemSymlink(alias)
		if err != nil {
			t.Fatalf("allowSystemSymlink(%q) unexpected error: %v", alias, err)
		}
		if !ok {
			t.Fatalf("allowSystemSymlink(%q) returned ok=false", alias)
		}
		if resolved != filepath.Clean(expected) {
			t.Fatalf("allowSystemSymlink(%q)=%q, want %q", alias, resolved, filepath.Clean(expected))
		}

		tested = true
		break
	}

	if !tested {
		t.Skip("no known system symlink aliases available")
	}
}

func TestWalkDirNoSymlinkAbs_RequiresAbsolute(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, true)

	_, err := p.walkDirNoSymlinkAbs("relative/path", false, 0)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, errPathMustBeAbsolute) {
		t.Fatalf("expected errPathMustBeAbsolute, got %v", err)
	}
}

func TestWalkDirNoSymlinkAbs_DotIsNoop(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, true)

	created, err := p.walkDirNoSymlinkAbs(".", false, 0)
	if err != nil {
		t.Fatalf("walkDirNoSymlinkAbs(.) error: %v", err)
	}
	if created != 0 {
		t.Fatalf("created=%d want 0", created)
	}
}

func TestWalkDirNoSymlinkAbs_FileComponentIsRejected(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	filePath := writeFile(t, filepath.Join(root, "file"), []byte("x"))
	p := mustNewPolicy(t, root, []string{root}, true)

	_, err := p.walkDirNoSymlinkAbs(filepath.Join(filePath, "child"), false, 0)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "path component is not a directory") {
		t.Fatalf("expected file-component error, got %v", err)
	}
}

func TestVerifyDirNoSymlinkAbs_MissingDirErrors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	root := mkdirAll(t, filepath.Join(tmp, "root"))
	p := mustNewPolicy(t, root, []string{root}, true)

	missing := filepath.Join(root, "nope")
	err := p.verifyDirNoSymlinkAbs(missing)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist; got %v", err)
	}
}

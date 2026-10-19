package gittool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

func TestInitCreatesRepositoryWithRelativePath(t *testing.T) {
	base := t.TempDir()
	repoRel := filepath.FromSlash(testRepoDirName)
	repoAbs := filepath.Join(base, repoRel)

	if err := os.MkdirAll(repoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", repoAbs, err)
	}

	tool := newTestGitTool(t, base)
	ctx := t.Context()
	out, err := tool.Init(ctx, InitArgs{
		RepoPath: repoRel,
	})
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	assertSamePath(t, out.RepoPath, repoAbs)
	assertSamePath(t, out.GitDir, filepath.Join(repoAbs, ".git"))
	if out.Bare {
		t.Fatal("Init().Bare = true, want false")
	}
	if out.Action != "initialized" {
		t.Fatalf("Init().Action = %q, want %q", out.Action, "initialized")
	}

	repo, err := git.PlainOpen(repoAbs)
	if err != nil {
		t.Fatalf("PlainOpen(%q) error = %v", repoAbs, err)
	}
	if _, err := repo.Worktree(); err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if _, err := repo.Head(); err == nil {
		t.Fatal("repo.Head() error = nil, want unborn HEAD error")
	}
	if _, err := os.Stat(filepath.Join(repoAbs, ".git")); err != nil {
		t.Fatalf("Stat(.git) error = %v", err)
	}
}

func TestInitCreatesRepositoryWithParents(t *testing.T) {
	base := t.TempDir()
	repoRel := filepath.FromSlash(testNestedRepoDirName)
	repoAbs := filepath.Join(base, repoRel)

	tool := newTestGitTool(t, base)
	ctx := t.Context()
	out, err := tool.Init(ctx, InitArgs{
		RepoPath:      repoRel,
		CreateParents: true,
	})
	if err != nil {
		t.Fatalf("Init(createParents=true) error = %v", err)
	}
	assertSamePath(t, out.RepoPath, repoAbs)
	assertSamePath(t, out.GitDir, filepath.Join(repoAbs, ".git"))

	repo, err := git.PlainOpen(repoAbs)
	if err != nil {
		t.Fatalf("PlainOpen(%q) error = %v", repoAbs, err)
	}
	if _, err := repo.Worktree(); err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
}

func TestInitCreatesBareRepository(t *testing.T) {
	base := t.TempDir()
	repoRel := filepath.FromSlash(testBareRepoDirName)
	repoAbs := filepath.Join(base, repoRel)

	tool := newTestGitTool(t, base)
	ctx := t.Context()
	out, err := tool.Init(ctx, InitArgs{
		RepoPath:      repoRel,
		CreateParents: true,
		Bare:          true,
	})
	if err != nil {
		t.Fatalf("Init(bare=true) error = %v", err)
	}
	if !out.Bare {
		t.Fatal("Init().Bare = false, want true")
	}
	assertSamePath(t, out.GitDir, repoAbs)

	repo, err := git.PlainOpen(repoAbs)
	if err != nil {
		t.Fatalf("PlainOpen(%q) error = %v", repoAbs, err)
	}
	if _, err := repo.Worktree(); err == nil {
		t.Fatal("Worktree() error = nil, want bare repository error")
	}
	if _, err := os.Stat(filepath.Join(repoAbs, ".git")); !os.IsNotExist(err) {
		t.Fatalf("Stat(%q) error = %v, want os.IsNotExist", filepath.Join(repoAbs, ".git"), err)
	}
}

func TestInitMissingDirectoryWithoutCreateParents(t *testing.T) {
	base := t.TempDir()
	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Init(ctx, InitArgs{
		RepoPath: filepath.FromSlash("missing/repo"),
	})
	if err == nil {
		t.Fatal("Init() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "createParents=false") {
		t.Fatalf("Init() error = %q, want createParents=false message", err)
	}
}

func TestInitRequiresRepoPath(t *testing.T) {
	base := t.TempDir()
	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Init(ctx, InitArgs{})
	if err == nil {
		t.Fatal("Init() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "repoPath is required") {
		t.Fatalf("Init() error = %q, want repoPath required message", err)
	}
}

package gittool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRejectsSymlinkTraversalWhenSupported(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, testCommitMessage)
	mustWriteFile(t, repoAbs, "target.txt", "target\n")
	if err := os.Symlink("target.txt", filepath.Join(repoAbs, "link.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	tool, err := NewGitTool(WithWorkBaseDir(base), WithAllowedRoots([]string{base}), WithBlockSymlinks(true))
	if err != nil {
		t.Fatalf("NewGitTool() error = %v", err)
	}

	_, err = tool.Add(t.Context(), AddArgs{RepoPath: repoRel, Paths: []string{"link.txt"}})
	if err == nil {
		t.Fatal("Add(symlink path with blocking) error = nil, want error")
	}
}

func TestCommitUsesFallbackAuthorWhenConfigIsEmpty(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "initial commit")
	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)

	tool, err := NewGitTool(
		WithWorkBaseDir(base),
		WithAllowedRoots([]string{base}),
		WithDefaultAuthor("Fallback Name", "fallback@example.invalid"),
	)
	if err != nil {
		t.Fatalf("NewGitTool() error = %v", err)
	}

	if _, err := tool.Add(t.Context(), AddArgs{RepoPath: repoRel, Paths: []string{testFileName}}); err != nil {
		t.Fatalf("Add(before fallback-author commit) error = %v", err)
	}

	out, err := tool.Commit(t.Context(), CommitArgs{
		RepoPath: repoRel,
		Message:  "second commit",
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if out.AuthorName != "Fallback Name" || out.AuthorEmail != "fallback@example.invalid" {
		t.Fatalf("Commit().author = (%q, %q), want fallback author", out.AuthorName, out.AuthorEmail)
	}
	if out.All {
		t.Fatal("Commit().All = true, want false")
	}
	if out.Hash == "" || out.ShortHash == "" {
		t.Fatalf("Commit().hashes = (%q, %q), want populated hashes", out.Hash, out.ShortHash)
	}
	if !strings.HasPrefix(out.Hash, out.ShortHash) {
		t.Fatalf("Commit().ShortHash = %q, want prefix of %q", out.ShortHash, out.Hash)
	}
}

func TestAddStagesExplicitPathsAndAll(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, testCommitMessage)

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	explicit, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, Paths: []string{testFileName}})
	if err != nil {
		t.Fatalf("Add(explicit) error = %v", err)
	}
	if explicit.All {
		t.Fatal("Add(explicit).All = true, want false")
	}
	if len(explicit.Paths) != 1 || explicit.Paths[0] != testFileName {
		t.Fatalf("Add(explicit).Paths = %#v, want [%q]", explicit.Paths, testFileName)
	}

	staged, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after explicit add) error = %v", err)
	}
	if len(staged.Entries) != 2 {
		t.Fatalf("Status(after explicit add).Entries len = %d, want 2", len(staged.Entries))
	}
	if staged.Entries[0].Path != testFileName || staged.Entries[0].Staging != "M" || staged.Entries[0].Worktree != " " {
		t.Fatalf("Status(after explicit add) staged file = %#v, want staged modification", staged.Entries[0])
	}
	if staged.Entries[1].Path != testSecondFileName || staged.Entries[1].Worktree != "?" {
		t.Fatalf("Status(after explicit add) untracked file = %#v, want untracked", staged.Entries[1])
	}

	all, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, All: true})
	if err != nil {
		t.Fatalf("Add(all) error = %v", err)
	}
	if !all.All {
		t.Fatal("Add(all).All = false, want true")
	}
	if len(all.Paths) != 1 || all.Paths[0] != "." {
		t.Fatalf("Add(all).Paths = %#v, want [\".\"]", all.Paths)
	}

	stagedAll, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after all add) error = %v", err)
	}
	if len(stagedAll.Entries) != 2 {
		t.Fatalf("Status(after all add).Entries len = %d, want 2", len(stagedAll.Entries))
	}
	if stagedAll.Entries[0].Path != testFileName || stagedAll.Entries[0].Staging != "M" ||
		stagedAll.Entries[0].Worktree != " " {
		t.Fatalf("Status(after all add) modified file = %#v, want staged modification", stagedAll.Entries[0])
	}
	if stagedAll.Entries[1].Path != testSecondFileName || stagedAll.Entries[1].Staging != "A" ||
		stagedAll.Entries[1].Worktree != " " {
		t.Fatalf("Status(after all add) new file = %#v, want staged addition", stagedAll.Entries[1])
	}
}

func TestAddRequiresPathsUnlessAll(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, testCommitMessage)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Add(ctx, AddArgs{RepoPath: repoRel})
	if err == nil {
		t.Fatal("Add(missing paths) error = nil, want error")
	}
}

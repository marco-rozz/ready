package gittool

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

func TestCommitCreatesCommitFromStagedChangesAndUsesRepoConfig(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	initialHash := mustCommitAll(t, repo, "initial commit")
	mustSetRepoUserConfig(t, repo, "Configured Name", "configured@example.invalid")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, Paths: []string{testFileName}}); err != nil {
		t.Fatalf("Add(before commit) error = %v", err)
	}

	out, err := tool.Commit(ctx, CommitArgs{RepoPath: repoRel, Message: "update tracked file"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if out.All {
		t.Fatal("Commit().All = true, want false")
	}
	if out.AuthorName != "Configured Name" || out.AuthorEmail != "configured@example.invalid" {
		t.Fatalf("Commit().author = (%q, %q), want configured repo author", out.AuthorName, out.AuthorEmail)
	}
	if out.ShortHash != out.Hash[:12] {
		t.Fatalf("Commit().ShortHash = %q, want prefix of %q", out.ShortHash, out.Hash)
	}
	assertSamePath(t, out.RepoPath, repoAbs)
	if out.When.IsZero() {
		t.Fatal("Commit().When = zero, want commit timestamp")
	}

	reopened, err := git.PlainOpen(repoAbs)
	if err != nil {
		t.Fatalf("PlainOpen(%q) error = %v", repoAbs, err)
	}
	headRef, err := reopened.Head()
	if err != nil {
		t.Fatalf("Head() error = %v", err)
	}
	if headRef.Hash().String() == initialHash.String() {
		t.Fatal("Commit() did not advance HEAD")
	}
	headCommit, err := reopened.CommitObject(headRef.Hash())
	if err != nil {
		t.Fatalf("CommitObject(HEAD) error = %v", err)
	}
	info := commitInfo(headCommit)
	if info.Subject != "update tracked file" {
		t.Fatalf("HEAD subject = %q, want %q", info.Subject, "update tracked file")
	}

	status, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after commit) error = %v", err)
	}
	if len(status.Entries) != 1 || status.Entries[0].Path != testSecondFileName || status.Entries[0].Worktree != "?" {
		t.Fatalf("Status(after commit).Entries = %#v, want only untracked file remaining", status.Entries)
	}
}

func TestCommitAllLeavesUntrackedFilesAndRejectsEmptyMessage(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "initial commit")
	mustSetRepoUserConfig(t, repo, "Configured Name", "configured@example.invalid")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Commit(ctx, CommitArgs{RepoPath: repoRel, Message: "auto stage tracked", All: true})
	if err != nil {
		t.Fatalf("Commit(all=true) error = %v", err)
	}
	if !out.All {
		t.Fatal("Commit().All = false, want true")
	}
	if out.AuthorName != "Configured Name" || out.AuthorEmail != "configured@example.invalid" {
		t.Fatalf("Commit().author = (%q, %q), want configured repo author", out.AuthorName, out.AuthorEmail)
	}

	status, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after all commit) error = %v", err)
	}
	if len(status.Entries) != 1 || status.Entries[0].Path != testSecondFileName || status.Entries[0].Worktree != "?" {
		t.Fatalf("Status(after all commit).Entries = %#v, want only untracked file remaining", status.Entries)
	}

	_, err = tool.Commit(ctx, CommitArgs{RepoPath: repoRel, Message: "   "})
	if err == nil {
		t.Fatal("Commit(empty message) error = nil, want error")
	}
}

func TestCommitUsesExplicitAuthorAndRejectsOversizedMessage(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "initial commit")
	mustSetRepoUserConfig(t, repo, "Configured Name", "configured@example.invalid")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, Paths: []string{testFileName}}); err != nil {
		t.Fatalf("Add(before explicit author commit) error = %v", err)
	}

	out, err := tool.Commit(ctx, CommitArgs{
		RepoPath:    repoRel,
		Message:     "commit with explicit author",
		AuthorName:  "Override Name",
		AuthorEmail: "override@example.invalid",
	})
	if err != nil {
		t.Fatalf("Commit(explicit author) error = %v", err)
	}
	if out.AuthorName != "Override Name" || out.AuthorEmail != "override@example.invalid" {
		t.Fatalf("Commit(explicit author).author = (%q, %q), want explicit args", out.AuthorName, out.AuthorEmail)
	}
	if out.All {
		t.Fatal("Commit(explicit author).All = true, want false")
	}

	_, err = tool.Commit(ctx, CommitArgs{RepoPath: repoRel, Message: strings.Repeat("x", hardCommitMessageBytes+1)})
	if err == nil {
		t.Fatal("Commit(oversized message) error = nil, want error")
	}
}

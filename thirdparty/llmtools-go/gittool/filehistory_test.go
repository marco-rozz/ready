package gittool

import (
	"path/filepath"
	"testing"
)

func TestFileHistoryTracksFirstParentAndAllParents(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, "one\n")
	_ = mustCommitAll(t, repo, "first commit")

	mustWriteFile(t, repoAbs, testFileName, "two\n")
	_ = mustCommitAll(t, repo, "second commit")

	mustWriteFile(t, repoAbs, testFileName, "three\n")
	_ = mustCommitAll(t, repo, "third commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	firstParent, err := tool.FileHistory(ctx, FileHistoryArgs{RepoPath: repoRel, Path: testFileName})
	if err != nil {
		t.Fatalf("FileHistory(firstParent) error = %v", err)
	}
	if !firstParent.FirstParent {
		t.Fatal("FileHistory(firstParent).FirstParent = false, want true")
	}
	if firstParent.Count != 3 || len(firstParent.Commits) != 3 {
		t.Fatalf("FileHistory(firstParent).Commits = %#v, want 3 commits", firstParent.Commits)
	}
	if firstParent.Commits[0].Commit.Subject != "third commit" || firstParent.Commits[0].Status != "modified" {
		t.Fatalf("FileHistory(firstParent)[0] = %#v, want third commit modified", firstParent.Commits[0])
	}
	if firstParent.Commits[2].Commit.Subject != "first commit" || firstParent.Commits[2].Status != "added" {
		t.Fatalf("FileHistory(firstParent)[2] = %#v, want first commit added", firstParent.Commits[2])
	}

	allParents, err := tool.FileHistory(
		ctx,
		FileHistoryArgs{RepoPath: repoRel, Path: testFileName, FirstParent: new(false)},
	)
	if err != nil {
		t.Fatalf("FileHistory(allParents) error = %v", err)
	}
	if allParents.FirstParent {
		t.Fatal("FileHistory(allParents).FirstParent = true, want false")
	}
	if allParents.Count != 3 || len(allParents.Commits) != 3 {
		t.Fatalf("FileHistory(allParents).Commits = %#v, want 3 commits", allParents.Commits)
	}

	truncated, err := tool.FileHistory(
		ctx,
		FileHistoryArgs{RepoPath: repoRel, Path: testFileName, MaxWalk: 1, MaxCommits: 50},
	)
	if err != nil {
		t.Fatalf("FileHistory(truncated) error = %v", err)
	}
	if !truncated.Truncated {
		t.Fatal("FileHistory(truncated).Truncated = false, want true")
	}
	if truncated.Walked != 1 || len(truncated.Commits) != 1 {
		t.Fatalf("FileHistory(truncated) = %#v, want one walked commit", truncated)
	}

	o, err := tool.FileHistory(ctx, FileHistoryArgs{RepoPath: repoRel, Path: "missing.txt"})
	if err == nil {
		t.Fatalf("FileHistory(missing path) error = nil, want error, out %#v", o)
	}
}

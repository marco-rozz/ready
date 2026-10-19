package gittool

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLogDefaultCountAndInvalidRevision(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, "one\n")
	_ = mustCommitAll(t, repo, "first commit")
	mustWriteFile(t, repoAbs, testFileName, "two\n")
	_ = mustCommitAll(t, repo, "second commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Log(ctx, LogArgs{RepoPath: repoRel, MaxCount: 0})
	if err != nil {
		t.Fatalf("Log(maxCount=0) error = %v", err)
	}
	if out.Revision != revisionHead {
		t.Fatalf("Log(maxCount=0).Revision = %q, want %q", out.Revision, revisionHead)
	}
	if out.MaxCount != defaultLogMaxCount {
		t.Fatalf("Log(maxCount=0).MaxCount = %d, want %d", out.MaxCount, defaultLogMaxCount)
	}
	if len(out.Commits) != 2 {
		t.Fatalf("Log(maxCount=0).Commits len = %d, want 2", len(out.Commits))
	}

	_, err = tool.Log(ctx, LogArgs{RepoPath: repoRel, Revision: "-bad"})
	if err == nil {
		t.Fatal("Log(invalid revision) error = nil, want error")
	}
}

func TestLogFiltersByCountAndTimeBounds(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, "one\n")
	firstHash := mustCommitAllAt(t, repo, "first commit", time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC))
	_ = firstHash

	mustWriteFile(t, repoAbs, testFileName, "two\n")
	secondHash := mustCommitAllAt(t, repo, "second commit", time.Date(2024, time.January, 2, 12, 0, 0, 0, time.UTC))
	_ = secondHash

	mustWriteFile(t, repoAbs, testFileName, "three\n")
	_ = mustCommitAllAt(t, repo, "third commit", time.Date(2024, time.January, 3, 12, 0, 0, 0, time.UTC))

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	latest, err := tool.Log(ctx, LogArgs{RepoPath: repoRel, MaxCount: 2})
	if err != nil {
		t.Fatalf("Log(maxCount=2) error = %v", err)
	}
	if latest.MaxCount != 2 {
		t.Fatalf("Log(maxCount=2).MaxCount = %d, want 2", latest.MaxCount)
	}
	if len(latest.Commits) != 2 {
		t.Fatalf("Log(maxCount=2).Commits len = %d, want 2", len(latest.Commits))
	}
	if latest.Commits[0].Subject != "third commit" || latest.Commits[1].Subject != "second commit" {
		t.Fatalf("Log(maxCount=2).Commits = %#v, want latest two commits", latest.Commits)
	}

	since, err := tool.Log(ctx, LogArgs{RepoPath: repoRel, Since: "2024-01-02"})
	if err != nil {
		t.Fatalf("Log(since) error = %v", err)
	}
	if len(since.Commits) != 2 {
		t.Fatalf("Log(since).Commits len = %d, want 2", len(since.Commits))
	}
	if since.Commits[0].Subject != "third commit" || since.Commits[1].Subject != "second commit" {
		t.Fatalf("Log(since).Commits = %#v, want commits from Jan 2 onward", since.Commits)
	}

	until, err := tool.Log(ctx, LogArgs{RepoPath: repoRel, Until: "2024-01-02"})
	if err != nil {
		t.Fatalf("Log(until) error = %v", err)
	}
	if len(until.Commits) != 1 || until.Commits[0].Subject != "first commit" {
		t.Fatalf("Log(until).Commits = %#v, want only first commit", until.Commits)
	}

	_, err = tool.Log(ctx, LogArgs{RepoPath: repoRel, Since: "bad-time"})
	if err == nil {
		t.Fatal("Log(invalid since) error = nil, want error")
	}
}

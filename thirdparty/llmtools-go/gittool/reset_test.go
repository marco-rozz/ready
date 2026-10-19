package gittool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResetMixedUnbornHeadUnstagesPaths(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	if _, err := tool.Add(
		ctx,
		AddArgs{RepoPath: repoRel, Paths: []string{testFileName, testSecondFileName}},
	); err != nil {
		t.Fatalf("Add(before unborn reset) error = %v", err)
	}

	resetOut, err := tool.Reset(ctx, ResetArgs{RepoPath: repoRel, Paths: []string{testFileName}})
	if err != nil {
		t.Fatalf("Reset(unborn mixed) error = %v", err)
	}
	if resetOut.Mode != ResetModeMixed {
		t.Fatalf("Reset(unborn mixed).Mode = %q, want %q", resetOut.Mode, ResetModeMixed)
	}
	if resetOut.Revision != revisionHead {
		t.Fatalf("Reset(unborn mixed).Revision = %q, want %q", resetOut.Revision, revisionHead)
	}
	if resetOut.Action != "mixed-reset-unborn-index" {
		t.Fatalf("Reset(unborn mixed).Action = %q, want %q", resetOut.Action, "mixed-reset-unborn-index")
	}
	if resetOut.HeadHash != "" {
		t.Fatalf("Reset(unborn mixed).HeadHash = %q, want empty", resetOut.HeadHash)
	}
	if len(resetOut.Paths) != 1 || resetOut.Paths[0] != testFileName {
		t.Fatalf("Reset(unborn mixed).Paths = %#v, want [%q]", resetOut.Paths, testFileName)
	}

	idx, err := repo.Storer.Index()
	if err != nil {
		t.Fatalf("Index(after unborn reset) error = %v", err)
	}
	if idx == nil {
		t.Fatal("Index(after unborn reset) = nil, want index")
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("Index(after unborn reset).Entries len = %d, want 1", len(idx.Entries))
	}
	if idx.Entries[0] == nil {
		t.Fatal("Index(after unborn reset).Entries[0] = nil, want staged entry")
	}
	if idx.Entries[0].Name != testSecondFileName {
		t.Fatalf(
			"Index(after unborn reset).Entries[0].Name = %q, want %q",
			idx.Entries[0].Name,
			testSecondFileName,
		)
	}

	status, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after unborn reset) error = %v", err)
	}
	if len(status.Entries) != 2 {
		t.Fatalf("Status(after unborn reset).Entries len = %d, want 2", len(status.Entries))
	}

	got := make(map[string]StatusEntry, len(status.Entries))
	for _, entry := range status.Entries {
		got[entry.Path] = entry
	}

	first, ok := got[testFileName]
	if !ok {
		t.Fatalf("Status(after unborn reset) missing %q entry: %#v", testFileName, status.Entries)
	}
	if first.Worktree != "?" || first.Staging != "?" {
		t.Fatalf("Status(after unborn reset)[%q] = %#v, want untracked entry", testFileName, first)
	}

	second, ok := got[testSecondFileName]
	if !ok {
		t.Fatalf("Status(after unborn reset) missing %q entry: %#v", testSecondFileName, status.Entries)
	}
	if second.Worktree != " " || second.Staging != "A" {
		t.Fatalf("Status(after unborn reset)[%q] = %#v, want staged addition", testSecondFileName, second)
	}
}

func TestResetHardRequiresConfirmDiscardAndClearsWorktree(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	initialHash := mustCommitAll(t, repo, "initial commit")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Reset(ctx, ResetArgs{RepoPath: repoRel, Mode: ResetModeHard})
	if err == nil {
		t.Fatal("Reset(hard without confirmDiscard) error = nil, want error")
	}

	out, err := tool.Reset(ctx, ResetArgs{RepoPath: repoRel, Mode: ResetModeHard, ConfirmDiscard: true})
	if err != nil {
		t.Fatalf("Reset(hard) error = %v", err)
	}
	if out.Mode != ResetModeHard {
		t.Fatalf("Reset(hard).Mode = %q, want %q", out.Mode, ResetModeHard)
	}
	if out.Revision != revisionHead {
		t.Fatalf("Reset(hard).Revision = %q, want %q", out.Revision, revisionHead)
	}
	if out.HeadHash != initialHash.String() {
		t.Fatalf("Reset(hard).HeadHash = %q, want %q", out.HeadHash, initialHash.String())
	}
	if !out.DiscardedWorktree {
		t.Fatal("Reset(hard).DiscardedWorktree = false, want true")
	}
	if out.Action != "hard-reset" {
		t.Fatalf("Reset(hard).Action = %q, want %q", out.Action, "hard-reset")
	}

	status, err := tool.Status(ctx, StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(after hard reset) error = %v", err)
	}
	if !status.IsClean {
		t.Fatalf("Status(after hard reset).IsClean = false, want true: %#v", status)
	}
	if len(status.Entries) != 0 {
		t.Fatalf("Status(after hard reset).Entries = %#v, want empty", status.Entries)
	}

	contents, err := os.ReadFile(filepath.Join(repoAbs, testFileName))
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", filepath.Join(repoAbs, testFileName), err)
	}
	if string(contents) != testFileContent {
		t.Fatalf("file contents after hard reset = %q, want %q", string(contents), testFileContent)
	}
}

func TestResetRejectsInvalidModeAndPathLimitedHard(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "initial commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Reset(ctx, ResetArgs{RepoPath: repoRel, Mode: ResetMode("bogus")})
	if err == nil {
		t.Fatal("Reset(invalid mode) error = nil, want error")
	}

	_, err = tool.Reset(ctx, ResetArgs{
		RepoPath:       repoRel,
		Mode:           ResetModeHard,
		Paths:          []string{testFileName},
		ConfirmDiscard: true,
	})
	if err == nil {
		t.Fatal("Reset(path-limited hard) error = nil, want error")
	}
}

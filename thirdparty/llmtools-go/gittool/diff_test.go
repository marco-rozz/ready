package gittool

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffWorkingStagedAndRefs(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	firstHash := mustCommitAll(t, repo, "first commit")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	working, err := tool.Diff(ctx, DiffArgs{
		RepoPath:     repoRel,
		Kind:         DiffKindWorking,
		ContextLines: new(1),
	})
	if err != nil {
		t.Fatalf("Diff(working) error = %v", err)
	}
	if !strings.Contains(working.Diff, "diff --git a/"+testFileName+" b/"+testFileName) {
		t.Fatalf("Diff(working) missing tracked file diff: %q", working.Diff)
	}
	if !strings.Contains(working.Diff, "diff --git a/"+testSecondFileName+" b/"+testSecondFileName) {
		t.Fatalf("Diff(working) missing untracked file diff: %q", working.Diff)
	}
	if working.Truncated || working.Bytes == 0 {
		t.Fatalf(
			"Diff(working) truncation/bytes = (%v, %d), want non-truncated non-empty",
			working.Truncated,
			working.Bytes,
		)
	}

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, Paths: []string{testFileName}}); err != nil {
		t.Fatalf("Add(before staged diff) error = %v", err)
	}
	staged, err := tool.Diff(ctx, DiffArgs{
		RepoPath:     repoRel,
		Kind:         DiffKindStaged,
		ContextLines: new(1),
	})
	if err != nil {
		t.Fatalf("Diff(staged) error = %v", err)
	}
	if !strings.Contains(staged.Diff, "diff --git a/"+testFileName+" b/"+testFileName) {
		t.Fatalf("Diff(staged) missing staged tracked file: %q", staged.Diff)
	}
	if strings.Contains(staged.Diff, testSecondFileName) {
		t.Fatalf("Diff(staged) unexpectedly contains untracked file: %q", staged.Diff)
	}

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, All: true}); err != nil {
		t.Fatalf("Add(all before refs diff) error = %v", err)
	}
	secondHash := mustCommitAll(t, repo, "second commit")

	refs, err := tool.Diff(ctx, DiffArgs{
		RepoPath:     repoRel,
		Kind:         DiffKindRefs,
		Base:         firstHash.String(),
		Target:       secondHash.String(),
		ContextLines: new(1),
	})
	if err != nil {
		t.Fatalf("Diff(refs) error = %v", err)
	}
	if !strings.Contains(refs.Diff, "diff --git a/"+testFileName+" b/"+testFileName) {
		t.Fatalf("Diff(refs) missing tracked file diff: %q", refs.Diff)
	}
	if !strings.Contains(refs.Diff, "diff --git a/"+testSecondFileName+" b/"+testSecondFileName) {
		t.Fatalf("Diff(refs) missing added file diff: %q", refs.Diff)
	}

	_, err = tool.Diff(ctx, DiffArgs{
		RepoPath: repoRel,
		Kind:     DiffKindRefs,
		Target:   secondHash.String(),
	})
	if err == nil {
		t.Fatal("Diff(refs missing base) error = nil, want error")
	}
}

func TestDiffDefaultsUnknownKind(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "diff fixtures")
	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Diff(ctx, DiffArgs{
		RepoPath: repoRel,
		Kind:     DiffKind("unexpected"),
	})
	if err != nil {
		t.Fatalf("Diff(default kind) error = %v", err)
	}
	if out.Kind != DiffKindWorking {
		t.Fatalf("Diff(default kind).Kind = %q, want %q", out.Kind, DiffKindWorking)
	}
	if !strings.Contains(out.Diff, "diff --git a/"+testFileName+" b/"+testFileName) {
		t.Fatalf("Diff(default kind).Diff = %q, want tracked file diff", out.Diff)
	}
}

func TestDiffTruncatesByMaxBytes(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "first commit")
	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Diff(ctx, DiffArgs{
		RepoPath: repoRel,
		Kind:     DiffKindWorking,
		MaxBytes: 80,
	})
	if err != nil {
		t.Fatalf("Diff(truncated) error = %v", err)
	}
	if !out.Truncated {
		t.Fatalf("Diff(truncated).Truncated = false, want true. Diff: %s", out.Diff)
	}
	if !strings.Contains(out.Diff, "[truncated]") {
		t.Fatalf("Diff(truncated).Diff = %q, want truncation marker", out.Diff)
	}
}

func TestDiffHonorsPathFiltersAndCountsBinaryFiles(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	mustWriteBinaryFile(t, repoAbs, testBinaryFileName, []byte(testBinaryContent))
	_ = mustCommitAll(t, repo, "diff fixtures")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteBinaryFile(t, repoAbs, testBinaryFileName, []byte{0xff, 0xfe, 0x01})

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	filtered, err := tool.Diff(ctx, DiffArgs{
		RepoPath: repoRel,
		Kind:     DiffKindWorking,
		Paths:    []string{testFileName},
	})
	if err != nil {
		t.Fatalf("Diff(path filtered) error = %v", err)
	}
	if len(filtered.Paths) != 1 || filtered.Paths[0] != testFileName {
		t.Fatalf("Diff(path filtered).Paths = %#v, want [%q]", filtered.Paths, testFileName)
	}
	if !strings.Contains(filtered.Diff, testFileName) {
		t.Fatalf("Diff(path filtered) missing tracked file diff: %q", filtered.Diff)
	}
	if strings.Contains(filtered.Diff, testBinaryFileName) {
		t.Fatalf("Diff(path filtered) unexpectedly contains binary file: %q", filtered.Diff)
	}
	if filtered.OmittedBinaryFiles != 0 {
		t.Fatalf("Diff(path filtered).OmittedBinaryFiles = %d, want 0", filtered.OmittedBinaryFiles)
	}

	all, err := tool.Diff(ctx, DiffArgs{
		RepoPath: repoRel,
		Kind:     DiffKindWorking,
	})
	if err != nil {
		t.Fatalf("Diff(all) error = %v", err)
	}
	if all.OmittedBinaryFiles != 1 {
		t.Fatalf("Diff(all).OmittedBinaryFiles = %d, want 1", all.OmittedBinaryFiles)
	}
	if !strings.Contains(all.Diff, "Binary files a/"+testBinaryFileName) {
		t.Fatalf("Diff(all) missing binary diff marker: %q", all.Diff)
	}
}

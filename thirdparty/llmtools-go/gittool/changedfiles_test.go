package gittool

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestChangedFilesDefaultsKindAndTruncatesByMaxCount(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "changed-files fixtures")
	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.ChangedFiles(ctx, ChangedFilesArgs{
		RepoPath: repoRel,
		Kind:     DiffKind("unexpected"),
		MaxCount: 1,
	})
	if err != nil {
		t.Fatalf("ChangedFiles(default kind) error = %v", err)
	}
	if out.Kind != DiffKindWorking {
		t.Fatalf("ChangedFiles(default kind).Kind = %q, want %q", out.Kind, DiffKindWorking)
	}
	if !out.Truncated {
		t.Fatal("ChangedFiles(maxCount=1).Truncated = false, want true")
	}
	if out.Count != 1 || len(out.Files) != 1 {
		t.Fatalf("ChangedFiles(maxCount=1).Files = %#v, want 1 entry", out.Files)
	}
	if out.Files[0].Path != testFileName || out.Files[0].Status != "modified" {
		t.Fatalf("ChangedFiles(maxCount=1)[0] = %#v, want modified tracked file", out.Files[0])
	}
}

func TestChangedFilesWorkingStagedAndRefs(t *testing.T) {
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

	working, err := tool.ChangedFiles(ctx, ChangedFilesArgs{RepoPath: repoRel, Kind: DiffKindWorking})
	if err != nil {
		t.Fatalf("ChangedFiles(working) error = %v", err)
	}
	if working.Count != 2 || len(working.Files) != 2 {
		t.Fatalf("ChangedFiles(working).Files = %#v, want 2 entries", working.Files)
	}
	if working.Files[0].Path != testFileName || working.Files[0].Status != "modified" {
		t.Fatalf("ChangedFiles(working)[0] = %#v, want modified tracked file", working.Files[0])
	}
	if working.Files[1].Path != testSecondFileName || working.Files[1].Status != "untracked" {
		t.Fatalf("ChangedFiles(working)[1] = %#v, want untracked file", working.Files[1])
	}

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, Paths: []string{testFileName}}); err != nil {
		t.Fatalf("Add(before staged changed-files) error = %v", err)
	}
	staged, err := tool.ChangedFiles(ctx, ChangedFilesArgs{RepoPath: repoRel, Kind: DiffKindStaged})
	if err != nil {
		t.Fatalf("ChangedFiles(staged) error = %v", err)
	}
	if !reflect.DeepEqual([]string{staged.Files[0].Path}, []string{testFileName}) {
		t.Fatalf("ChangedFiles(staged).Files = %#v, want only staged tracked file", staged.Files)
	}
	if staged.Files[0].Status != "modified" {
		t.Fatalf("ChangedFiles(staged)[0] = %#v, want modified", staged.Files[0])
	}

	if _, err := tool.Add(ctx, AddArgs{RepoPath: repoRel, All: true}); err != nil {
		t.Fatalf("Add(all before refs changed-files) error = %v", err)
	}
	secondHash := mustCommitAll(t, repo, "second commit")

	refs, err := tool.ChangedFiles(ctx, ChangedFilesArgs{
		RepoPath: repoRel,
		Kind:     DiffKindRefs,
		Base:     firstHash.String(),
		Target:   secondHash.String(),
	})
	if err != nil {
		t.Fatalf("ChangedFiles(refs) error = %v", err)
	}
	if refs.Count != 2 || len(refs.Files) != 2 {
		t.Fatalf("ChangedFiles(refs).Files = %#v, want 2 entries", refs.Files)
	}
	if refs.Files[0].Status != "modified" || refs.Files[0].Path != testFileName {
		t.Fatalf("ChangedFiles(refs)[0] = %#v, want modified tracked file", refs.Files[0])
	}
	if refs.Files[1].Status != "added" || refs.Files[1].Path != testSecondFileName {
		t.Fatalf("ChangedFiles(refs)[1] = %#v, want added file", refs.Files[1])
	}
}

func TestChangedFilesPathFilterAndUnknownKindDefaultToWorking(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	mustWriteFile(t, repoAbs, testSecondFileName, testSecondFileContents)
	_ = mustCommitAll(t, repo, "first commit")

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	mustWriteFile(t, repoAbs, testSecondFileName, "another change\n")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.ChangedFiles(ctx, ChangedFilesArgs{
		RepoPath: repoRel,
		Kind:     DiffKind("unexpected"),
		Paths:    []string{testFileName},
	})
	if err != nil {
		t.Fatalf("ChangedFiles(unknown kind) error = %v", err)
	}
	if out.Kind != DiffKindWorking {
		t.Fatalf("ChangedFiles(unknown kind).Kind = %q, want %q", out.Kind, DiffKindWorking)
	}
	if len(out.Files) != 1 || out.Files[0].Path != testFileName || out.Files[0].Status != "modified" {
		t.Fatalf("ChangedFiles(path filtered) = %#v, want only modified tracked file", out.Files)
	}
	if len(out.Paths) != 1 || out.Paths[0] != testFileName {
		t.Fatalf("ChangedFiles(path filtered).Paths = %#v, want [%q]", out.Paths, testFileName)
	}
}

func TestChangedFilesRequiresBaseForRefs(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "first commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.ChangedFiles(ctx, ChangedFilesArgs{RepoPath: repoRel, Kind: DiffKindRefs})
	if err == nil {
		t.Fatal("ChangedFiles(refs missing base) error = nil, want error")
	}
}

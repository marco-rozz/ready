package gittool

import (
	"path/filepath"
	"testing"
)

func TestBlameEmptyFileReturnsNoLines(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, "empty.txt", "")
	_ = mustCommitAll(t, repo, "empty file commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Blame(ctx, BlameArgs{RepoPath: repoRel, Path: "empty.txt"})
	if err != nil {
		t.Fatalf("Blame(empty file) error = %v", err)
	}
	if len(out.Lines) != 0 {
		t.Fatalf("Blame(empty file).Lines len = %d, want 0", len(out.Lines))
	}
	if out.StartLine != 1 || out.EndLine != 0 {
		t.Fatalf("Blame(empty file) range = (%d, %d), want (1, 0)", out.StartLine, out.EndLine)
	}
	if out.Note != "best-effort first-parent blame" {
		t.Fatalf("Blame(empty file).Note = %q, want best-effort note", out.Note)
	}
	if out.MaxCommits != 200 {
		t.Fatalf("Blame(empty file).MaxCommits = %d, want 200", out.MaxCommits)
	}
}

func TestBlameAssignsLinesAndRespectsRanges(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, "line1\nline2\nline3\n")
	rootHash := mustCommitAll(t, repo, "root commit")
	_ = rootHash

	mustWriteFile(t, repoAbs, testFileName, "line1\nline2b\nline3\n")
	secondHash := mustCommitAll(t, repo, "second commit")
	_ = secondHash

	mustWriteFile(t, repoAbs, testFileName, "line1\nline2b\nline3c\n")
	_ = mustCommitAll(t, repo, "third commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Blame(ctx, BlameArgs{RepoPath: repoRel, Path: testFileName})
	if err != nil {
		t.Fatalf("Blame() error = %v", err)
	}
	if len(out.Lines) != 3 {
		t.Fatalf("Blame().Lines len = %d, want 3", len(out.Lines))
	}
	if out.Lines[0].LineNumber != 1 || out.Lines[0].Commit.Subject != "root commit" {
		t.Fatalf("Blame().Lines[0] = %#v, want root commit", out.Lines[0])
	}
	if out.Lines[1].LineNumber != 2 || out.Lines[1].Commit.Subject != "second commit" {
		t.Fatalf("Blame().Lines[1] = %#v, want second commit", out.Lines[1])
	}
	if out.Lines[2].LineNumber != 3 || out.Lines[2].Commit.Subject != "third commit" {
		t.Fatalf("Blame().Lines[2] = %#v, want third commit", out.Lines[2])
	}
	if out.Note != "best-effort first-parent blame; duplicate or moved lines may differ from git CLI blame" {
		t.Fatalf("Blame().Note = %q, want best-effort note", out.Note)
	}

	rangeOut, err := tool.Blame(ctx, BlameArgs{
		RepoPath:  repoRel,
		Path:      testFileName,
		StartLine: 2,
		EndLine:   3,
	})
	if err != nil {
		t.Fatalf("Blame(range) error = %v", err)
	}
	if len(rangeOut.Lines) != 2 || rangeOut.Lines[0].LineNumber != 2 || rangeOut.Lines[1].LineNumber != 3 {
		t.Fatalf("Blame(range).Lines = %#v, want lines 2 and 3", rangeOut.Lines)
	}

	_, err = tool.Blame(ctx, BlameArgs{RepoPath: repoRel, Path: testFileName, StartLine: 3, EndLine: 2})
	if err == nil {
		t.Fatal("Blame(startLine > endLine) error = nil, want error")
	}
}

func TestBlameRejectsBinaryFiles(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteBinaryFile(t, repoAbs, testBinaryFileName, []byte(testBinaryContent))
	_ = mustCommitAll(t, repo, "binary commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Blame(ctx, BlameArgs{RepoPath: repoRel, Path: testBinaryFileName})
	if err == nil {
		t.Fatal("Blame(binary) error = nil, want error")
	}
}

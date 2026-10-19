package gittool

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShowIncludesCommitMetadataPatchAndRootNote(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, strings.Repeat("alpha\n", 20))
	rootHash := mustCommitAll(t, repo, "root commit")

	mustWriteFile(t, repoAbs, testFileName, strings.Repeat("beta\n", 20))
	secondHash := mustCommitAll(t, repo, "second commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	shown, err := tool.Show(ctx, ShowArgs{
		RepoPath:     repoRel,
		Revision:     secondHash.String(),
		IncludePatch: new(true),
		MaxBytes:     140,
	})
	if err != nil {
		t.Fatalf("Show(second) error = %v", err)
	}
	if shown.Revision != secondHash.String() {
		t.Fatalf("Show(second).Revision = %q, want %q", shown.Revision, secondHash.String())
	}
	if shown.Commit.Subject != "second commit" {
		t.Fatalf("Show(second).Commit.Subject = %q, want %q", shown.Commit.Subject, "second commit")
	}
	if len(shown.ParentHashes) != 1 || shown.ParentHashes[0] != rootHash.String() {
		t.Fatalf("Show(second).ParentHashes = %#v, want root hash", shown.ParentHashes)
	}
	if !shown.PatchTruncated {
		t.Fatalf("Show(second).PatchTruncated = false, want true, %#v", shown)
	}
	if !strings.Contains(shown.Patch, "[truncated]") {
		t.Fatalf("Show(second).Patch = %q, want truncation marker", shown.Patch)
	}

	noPatch, err := tool.Show(ctx, ShowArgs{
		RepoPath:     repoRel,
		Revision:     secondHash.String(),
		IncludePatch: new(false),
	})
	if err != nil {
		t.Fatalf("Show(no patch) error = %v", err)
	}
	if noPatch.Patch != "" || noPatch.PatchBytes != 0 {
		t.Fatalf("Show(no patch) patch fields = %#v, want omitted patch", noPatch)
	}

	rootOut, err := tool.Show(ctx, ShowArgs{
		RepoPath:     repoRel,
		Revision:     rootHash.String(),
		IncludePatch: new(true),
	})
	if err != nil {
		t.Fatalf("Show(root) error = %v", err)
	}
	if rootOut.Note != "root commit patch is not emitted by this tool version" {
		t.Fatalf("Show(root).Note = %q, want root note", rootOut.Note)
	}
	if rootOut.Patch != "" {
		t.Fatalf("Show(root).Patch = %q, want empty", rootOut.Patch)
	}
}

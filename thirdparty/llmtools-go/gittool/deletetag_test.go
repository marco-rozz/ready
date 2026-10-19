package gittool

import (
	"path/filepath"
	"testing"
)

func TestDeleteTagDeletesAnnotatedTagAndErrorsForMissingTag(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	targetHash := mustCommitAll(t, repo, "release target")
	mustSetRepoUserConfig(t, repo, "Tagger Name", "tagger@example.invalid")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	created, err := tool.CreateTag(ctx, CreateTagArgs{
		RepoPath: repoRel,
		Name:     "v2.0.0",
		Target:   targetHash.String(),
		Message:  "Release 2.0.0",
	})
	if err != nil {
		t.Fatalf("CreateTag(before delete) error = %v", err)
	}

	before, err := tool.Tags(ctx, TagsArgs{RepoPath: repoRel, Pattern: "v2*"})
	if err != nil {
		t.Fatalf("Tags(before delete) error = %v", err)
	}
	if len(before.Tags) != 1 {
		t.Fatalf("Tags(before delete).Tags len = %d, want 1", len(before.Tags))
	}

	deleted, err := tool.DeleteTag(ctx, DeleteTagArgs{RepoPath: repoRel, Name: "v2.0.0"})
	if err != nil {
		t.Fatalf("DeleteTag() error = %v", err)
	}
	if deleted.Name != "v2.0.0" {
		t.Fatalf("DeleteTag().Name = %q, want %q", deleted.Name, "v2.0.0")
	}
	if deleted.Action != "deleted" {
		t.Fatalf("DeleteTag().Action = %q, want %q", deleted.Action, "deleted")
	}
	if deleted.Hash != created.TagHash {
		t.Fatalf("DeleteTag().Hash = %q, want %q", deleted.Hash, created.TagHash)
	}

	after, err := tool.Tags(ctx, TagsArgs{RepoPath: repoRel, Pattern: "v2*"})
	if err != nil {
		t.Fatalf("Tags(after delete) error = %v", err)
	}
	if len(after.Tags) != 0 {
		t.Fatalf("Tags(after delete).Tags len = %d, want 0", len(after.Tags))
	}

	_, err = tool.DeleteTag(ctx, DeleteTagArgs{RepoPath: repoRel, Name: "v2.0.0"})
	if err == nil {
		t.Fatal("DeleteTag(missing) error = nil, want error")
	}
}

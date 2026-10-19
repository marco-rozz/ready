package gittool

import (
	"path/filepath"
	"testing"
)

func TestCreateTagCreatesLightweightAndAnnotatedTags(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	targetHash := mustCommitAll(t, repo, "release target")
	mustSetRepoUserConfig(t, repo, "Tagger Name", "tagger@example.invalid")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	lightweight, err := tool.CreateTag(ctx, CreateTagArgs{
		RepoPath: repoRel,
		Name:     "v1.0.0",
		Target:   targetHash.String(),
	})
	if err != nil {
		t.Fatalf("CreateTag(lightweight) error = %v", err)
	}
	if lightweight.Annotated {
		t.Fatal("CreateTag(lightweight).Annotated = true, want false")
	}
	if lightweight.TagHash != targetHash.String() {
		t.Fatalf("CreateTag(lightweight).TagHash = %q, want %q", lightweight.TagHash, targetHash.String())
	}
	if lightweight.TargetHash != targetHash.String() {
		t.Fatalf("CreateTag(lightweight).TargetHash = %q, want %q", lightweight.TargetHash, targetHash.String())
	}

	tags, err := tool.Tags(ctx, TagsArgs{RepoPath: repoRel, Pattern: "v1*"})
	if err != nil {
		t.Fatalf("Tags(after lightweight create) error = %v", err)
	}
	if len(tags.Tags) != 1 {
		t.Fatalf("Tags(after lightweight create).Tags len = %d, want 1", len(tags.Tags))
	}
	if tags.Tags[0].Name != "v1.0.0" || tags.Tags[0].Annotated {
		t.Fatalf("Tags(after lightweight create)[0] = %#v, want lightweight v1.0.0", tags.Tags[0])
	}

	annotated, err := tool.CreateTag(ctx, CreateTagArgs{
		RepoPath: repoRel,
		Name:     "v1.0.1",
		Target:   targetHash.String(),
		Message:  "Release 1.0.1",
	})
	if err != nil {
		t.Fatalf("CreateTag(annotated) error = %v", err)
	}
	if !annotated.Annotated {
		t.Fatal("CreateTag(annotated).Annotated = false, want true")
	}
	if annotated.TaggerName != "Tagger Name" || annotated.TaggerEmail != "tagger@example.invalid" {
		t.Fatalf(
			"CreateTag(annotated).tagger = (%q, %q), want config tagger",
			annotated.TaggerName,
			annotated.TaggerEmail,
		)
	}
	if annotated.Message != "Release 1.0.1" {
		t.Fatalf("CreateTag(annotated).Message = %q, want %q", annotated.Message, "Release 1.0.1")
	}
	if annotated.TagHash == annotated.TargetHash {
		t.Fatal("CreateTag(annotated).TagHash should differ from target hash")
	}

	tagsAfter, err := tool.Tags(ctx, TagsArgs{RepoPath: repoRel, Pattern: "v1*"})
	if err != nil {
		t.Fatalf("Tags(after annotated create) error = %v", err)
	}
	if len(tagsAfter.Tags) != 2 {
		t.Fatalf("Tags(after annotated create).Tags len = %d, want 2", len(tagsAfter.Tags))
	}
	if tagsAfter.Tags[0].Name != "v1.0.0" || tagsAfter.Tags[1].Name != "v1.0.1" {
		t.Fatalf("Tags(after annotated create).Tags = %#v, want sorted tag names", tagsAfter.Tags)
	}
	if !tagsAfter.Tags[1].Annotated || tagsAfter.Tags[1].TaggerName != "Tagger Name" {
		t.Fatalf("Tags(after annotated create)[1] = %#v, want annotated tag with tagger", tagsAfter.Tags[1])
	}

	_, err = tool.CreateTag(ctx, CreateTagArgs{RepoPath: repoRel, Name: "v1.0.0", Target: targetHash.String()})
	if err == nil {
		t.Fatal("CreateTag(duplicate) error = nil, want error")
	}

	_, err = tool.Tags(ctx, TagsArgs{RepoPath: repoRel, Pattern: "["})
	if err == nil {
		t.Fatal("Tags(invalid pattern) error = nil, want error")
	}
}

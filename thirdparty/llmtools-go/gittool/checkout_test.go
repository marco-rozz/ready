package gittool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutCreatesAndSwitchesBranches(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	firstHash := mustCommitAll(t, repo, "first commit")
	baseBranch, _, _ := headInfo(repo)

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	secondHash := mustCommitAll(t, repo, "second commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	created, err := tool.Checkout(ctx, CheckoutArgs{
		RepoPath:   repoRel,
		Name:       "topic",
		Create:     true,
		StartPoint: firstHash.String(),
	})
	if err != nil {
		t.Fatalf("Checkout(create=true) error = %v", err)
	}
	if !created.Created {
		t.Fatal("Checkout(create=true).Created = false, want true")
	}
	if created.Name != "topic" {
		t.Fatalf("Checkout(create=true).Name = %q, want %q", created.Name, "topic")
	}
	if created.StartPoint != firstHash.String() {
		t.Fatalf("Checkout(create=true).StartPoint = %q, want %q", created.StartPoint, firstHash.String())
	}
	if created.CurrentHead != "topic" {
		t.Fatalf("Checkout(create=true).CurrentHead = %q, want %q", created.CurrentHead, "topic")
	}
	if created.HeadHash != firstHash.String() {
		t.Fatalf("Checkout(create=true).HeadHash = %q, want %q", created.HeadHash, firstHash.String())
	}

	branches, err := tool.Branches(ctx, BranchesArgs{RepoPath: repoRel, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(after checkout create) error = %v", err)
	}
	if branches.Current != "topic" {
		t.Fatalf("Branches(after checkout create).Current = %q, want %q", branches.Current, "topic")
	}

	switchedBack, err := tool.Checkout(ctx, CheckoutArgs{
		RepoPath: repoRel,
		Name:     baseBranch,
	})
	if err != nil {
		t.Fatalf("Checkout(back) error = %v", err)
	}
	if switchedBack.Created {
		t.Fatal("Checkout(back).Created = true, want false")
	}
	if switchedBack.Name != baseBranch {
		t.Fatalf("Checkout(back).Name = %q, want %q", switchedBack.Name, baseBranch)
	}
	if switchedBack.CurrentHead != baseBranch {
		t.Fatalf("Checkout(back).CurrentHead = %q, want %q", switchedBack.CurrentHead, baseBranch)
	}
	if switchedBack.HeadHash != secondHash.String() {
		t.Fatalf("Checkout(back).HeadHash = %q, want %q", switchedBack.HeadHash, secondHash.String())
	}

	finalBranches, err := tool.Branches(ctx, BranchesArgs{RepoPath: repoRel, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(after checkout back) error = %v", err)
	}
	if finalBranches.Current != baseBranch {
		t.Fatalf("Branches(after checkout back).Current = %q, want %q", finalBranches.Current, baseBranch)
	}
}

func TestCheckoutForceDiscardsDirtyChanges(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	firstHash := mustCommitAll(t, repo, "first commit")
	baseBranch, _, _ := headInfo(repo)

	mustWriteFile(t, repoAbs, testFileName, testModifiedFileContents)
	_ = mustCommitAll(t, repo, "second commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	if _, err := tool.CreateBranch(ctx, CreateBranchArgs{
		RepoPath:   repoRel,
		Name:       "topic",
		StartPoint: firstHash.String(),
		Checkout:   false,
	}); err != nil {
		t.Fatalf("CreateBranch(topic) error = %v", err)
	}

	mustWriteFile(t, repoAbs, testFileName, "dirty change\n")

	_, err := tool.Checkout(ctx, CheckoutArgs{RepoPath: repoRel, Name: "topic"})
	if err == nil {
		t.Fatal("Checkout(dirty without force) error = nil, want error")
	}

	forced, err := tool.Checkout(ctx, CheckoutArgs{RepoPath: repoRel, Name: "topic", Force: true})
	if err != nil {
		t.Fatalf("Checkout(force=true) error = %v", err)
	}
	if !forced.Force {
		t.Fatal("Checkout(force=true).Force = false, want true")
	}
	if forced.CurrentHead != "topic" {
		t.Fatalf("Checkout(force=true).CurrentHead = %q, want %q", forced.CurrentHead, "topic")
	}
	if forced.HeadHash != firstHash.String() {
		t.Fatalf("Checkout(force=true).HeadHash = %q, want %q", forced.HeadHash, firstHash.String())
	}

	contents, err := os.ReadFile(filepath.Join(repoAbs, testFileName))
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", filepath.Join(repoAbs, testFileName), err)
	}
	if string(contents) != testFileContent {
		t.Fatalf("file contents after forced checkout = %q, want %q", string(contents), testFileContent)
	}

	branches, err := tool.Branches(ctx, BranchesArgs{RepoPath: repoRel, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(after forced checkout) error = %v", err)
	}
	if branches.Current != "topic" {
		t.Fatalf("Branches(after forced checkout).Current = %q, want %q", branches.Current, "topic")
	}
	if baseBranch == "" {
		t.Fatal("base branch name should not be empty")
	}
}

func TestCheckoutRejectsMissingBranchWithoutCreate(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, "first commit")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Checkout(ctx, CheckoutArgs{RepoPath: repoRel, Name: "missing-branch"})
	if err == nil {
		t.Fatal("Checkout(missing branch) error = nil, want error")
	}
}

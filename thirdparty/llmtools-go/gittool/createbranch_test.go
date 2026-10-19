package gittool

import (
	"path/filepath"
	"testing"
)

func TestCreateBranchAndCheckoutDefaultStartPointAndInvalidNames(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	headHash := mustCommitAll(t, repo, testCommitMessage)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	created, err := tool.CreateBranch(ctx, CreateBranchArgs{RepoPath: repoRel, Name: "topic-default"})
	if err != nil {
		t.Fatalf("CreateBranch(default startPoint) error = %v", err)
	}
	assertSamePath(t, created.RepoPath, repoAbs)
	if created.StartPoint != revisionHead {
		t.Fatalf("CreateBranch(default startPoint).StartPoint = %q, want %q", created.StartPoint, revisionHead)
	}
	if created.Hash != headHash.String() {
		t.Fatalf("CreateBranch(default startPoint).Hash = %q, want %q", created.Hash, headHash.String())
	}
	if created.CheckedOut {
		t.Fatal("CreateBranch(default startPoint).CheckedOut = true, want false")
	}

	checkedOut, err := tool.Checkout(ctx, CheckoutArgs{RepoPath: repoRel, Name: "checkout-default", Create: true})
	if err != nil {
		t.Fatalf("Checkout(default startPoint) error = %v", err)
	}
	assertSamePath(t, checkedOut.RepoPath, repoAbs)
	if checkedOut.StartPoint != revisionHead {
		t.Fatalf("Checkout(default startPoint).StartPoint = %q, want %q", checkedOut.StartPoint, revisionHead)
	}
	if checkedOut.CurrentHead != "checkout-default" {
		t.Fatalf("Checkout(default startPoint).CurrentHead = %q, want %q", checkedOut.CurrentHead, "checkout-default")
	}
	if checkedOut.HeadHash != headHash.String() {
		t.Fatalf("Checkout(default startPoint).HeadHash = %q, want %q", checkedOut.HeadHash, headHash.String())
	}

	if _, err := tool.CreateBranch(ctx, CreateBranchArgs{RepoPath: repoRel, Name: "refs/heads/bad"}); err == nil {
		t.Fatal("CreateBranch(invalid name) error = nil, want error")
	}
	if _, err := tool.Checkout(ctx, CheckoutArgs{RepoPath: repoRel, Name: "topic.lock"}); err == nil {
		t.Fatal("Checkout(invalid name) error = nil, want error")
	}
}

func TestCreateBranchCreatesAndChecksOutBranches(t *testing.T) {
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

	created, err := tool.CreateBranch(ctx, CreateBranchArgs{
		RepoPath:   repoRel,
		Name:       "feature-from-first",
		StartPoint: firstHash.String(),
		Checkout:   false,
	})
	if err != nil {
		t.Fatalf("CreateBranch(checkout=false) error = %v", err)
	}
	if created.CheckedOut {
		t.Fatal("CreateBranch().CheckedOut = true, want false")
	}
	if created.Hash != firstHash.String() {
		t.Fatalf("CreateBranch().Hash = %q, want %q", created.Hash, firstHash.String())
	}
	if created.StartPoint != firstHash.String() {
		t.Fatalf("CreateBranch().StartPoint = %q, want %q", created.StartPoint, firstHash.String())
	}

	branches, err := tool.Branches(ctx, BranchesArgs{RepoPath: repoRel, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(after create) error = %v", err)
	}
	if branches.Current != baseBranch {
		t.Fatalf("Branches(after create).Current = %q, want %q", branches.Current, baseBranch)
	}
	if len(branches.Branches) != 2 {
		t.Fatalf("Branches(after create).Branches len = %d, want 2", len(branches.Branches))
	}
	if branches.Branches[0].Name != "feature-from-first" || branches.Branches[0].Hash != firstHash.String() {
		t.Fatalf("Branches(after create)[0] = %#v, want feature-from-first at first hash", branches.Branches[0])
	}
	if branches.Branches[1].Name != baseBranch || branches.Branches[1].Hash != secondHash.String() {
		t.Fatalf("Branches(after create)[1] = %#v, want %q at second hash", branches.Branches[1], baseBranch)
	}

	checkedOut, err := tool.CreateBranch(ctx, CreateBranchArgs{
		RepoPath:   repoRel,
		Name:       "feature-checked-out",
		StartPoint: secondHash.String(),
		Checkout:   true,
	})
	if err != nil {
		t.Fatalf("CreateBranch(checkout=true) error = %v", err)
	}
	if !checkedOut.CheckedOut {
		t.Fatal("CreateBranch(checkout=true).CheckedOut = false, want true")
	}
	if checkedOut.CurrentHead != "feature-checked-out" {
		t.Fatalf("CreateBranch(checkout=true).CurrentHead = %q, want %q", checkedOut.CurrentHead, "feature-checked-out")
	}
	if checkedOut.Hash != secondHash.String() {
		t.Fatalf("CreateBranch(checkout=true).Hash = %q, want %q", checkedOut.Hash, secondHash.String())
	}

	postCheckout, err := tool.Branches(ctx, BranchesArgs{RepoPath: repoRel, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(after checkout) error = %v", err)
	}
	if postCheckout.Current != "feature-checked-out" {
		t.Fatalf("Branches(after checkout).Current = %q, want %q", postCheckout.Current, "feature-checked-out")
	}
}

func TestCreateBranchRejectsExistingBranch(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, testFileName, testFileContent)
	firstHash := mustCommitAll(t, repo, "first commit")
	_ = firstHash

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	if _, err := tool.CreateBranch(
		ctx,
		CreateBranchArgs{RepoPath: repoRel, Name: "feature-existing", StartPoint: firstHash.String()},
	); err != nil {
		t.Fatalf("CreateBranch(first) error = %v", err)
	}
	_, err := tool.CreateBranch(
		ctx,
		CreateBranchArgs{RepoPath: repoRel, Name: "feature-existing", StartPoint: firstHash.String()},
	)
	if err == nil {
		t.Fatal("CreateBranch(duplicate) error = nil, want error")
	}
}

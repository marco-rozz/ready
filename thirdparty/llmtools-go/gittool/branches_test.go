package gittool

import (
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/config"
)

func TestBranchesListsLocalRemoteAndAllBranches(t *testing.T) {
	base := t.TempDir()
	localAbs := filepath.Join(base, testRepoDirName)
	remoteAbs := filepath.Join(base, "remote.git")

	repo := mustInitRepo(t, localAbs, false)
	mustWriteFile(t, localAbs, testFileName, testFileContent)
	_ = mustCommitAll(t, repo, testCommitMessage)
	baseBranch, _, _ := headInfo(repo)

	mustCheckoutBranch(t, repo, testFeatureBranchName, true)
	mustInitRepo(t, remoteAbs, true)
	remote := mustCreateRemote(t, repo, testOriginRemoteName, remoteAbs)

	ctx := t.Context()
	mustPushRefs(t, ctx, remote, []config.RefSpec{
		config.RefSpec("+refs/heads/*:refs/heads/*"),
	})
	mustFetchRefs(t, ctx, remote, []config.RefSpec{
		config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
	})

	tool := newTestGitTool(t, base)

	local, err := tool.Branches(ctx, BranchesArgs{RepoPath: testRepoDirName, Kind: BranchKindLocal})
	if err != nil {
		t.Fatalf("Branches(local) error = %v", err)
	}
	if local.Kind != BranchKindLocal {
		t.Fatalf("Branches(local).Kind = %q, want %q", local.Kind, BranchKindLocal)
	}
	if local.Current != testFeatureBranchName {
		t.Fatalf("Branches(local).Current = %q, want %q", local.Current, testFeatureBranchName)
	}
	if len(local.Branches) != 2 {
		t.Fatalf("Branches(local).Branches len = %d, want 2", len(local.Branches))
	}
	if local.Branches[0].Name != testFeatureBranchName || !local.Branches[0].Current {
		t.Fatalf("Branches(local)[0] = %#v, want current feature branch", local.Branches[0])
	}
	if local.Branches[1].Name != baseBranch {
		t.Fatalf("Branches(local)[1] = %#v, want %q branch", local.Branches[1], baseBranch)
	}

	remoteOut, err := tool.Branches(ctx, BranchesArgs{RepoPath: testRepoDirName, Kind: BranchKindRemote})
	if err != nil {
		t.Fatalf("Branches(remote) error = %v", err)
	}
	if remoteOut.Kind != BranchKindRemote {
		t.Fatalf("Branches(remote).Kind = %q, want %q", remoteOut.Kind, BranchKindRemote)
	}
	if len(remoteOut.Branches) != 2 {
		t.Fatalf("Branches(remote).Branches len = %d, want 2", len(remoteOut.Branches))
	}
	wantRemote0 := "origin/" + baseBranch
	wantRemote1 := "origin/" + testFeatureBranchName
	if remoteOut.Branches[0].Name != wantRemote1 || remoteOut.Branches[1].Name != wantRemote0 {
		t.Fatalf("Branches(remote).Branches = %#v, want origin refs", remoteOut.Branches)
	}
	if remoteOut.Branches[0].Current || remoteOut.Branches[1].Current {
		t.Fatalf("Branches(remote) current flags should be false, got %#v", remoteOut.Branches)
	}

	allOut, err := tool.Branches(ctx, BranchesArgs{RepoPath: testRepoDirName, Kind: BranchKindAll})
	if err != nil {
		t.Fatalf("Branches(all) error = %v", err)
	}
	if allOut.Kind != BranchKindAll {
		t.Fatalf("Branches(all).Kind = %q, want %q", allOut.Kind, BranchKindAll)
	}
	if len(allOut.Branches) != 4 {
		t.Fatalf("Branches(all).Branches len = %d, want 4", len(allOut.Branches))
	}
	if allOut.Branches[0].Kind != BranchKindLocal || allOut.Branches[1].Kind != BranchKindLocal {
		t.Fatalf("Branches(all) first entries = %#v, want local branches first", allOut.Branches[:2])
	}
	if allOut.Branches[2].Kind != BranchKindRemote || allOut.Branches[3].Kind != BranchKindRemote {
		t.Fatalf("Branches(all) last entries = %#v, want remote branches last", allOut.Branches[2:])
	}
	if allOut.Current != testFeatureBranchName {
		t.Fatalf("Branches(all).Current = %q, want %q", allOut.Current, testFeatureBranchName)
	}

	unknown, err := tool.Branches(ctx, BranchesArgs{RepoPath: testRepoDirName, Kind: BranchKind("unexpected")})
	if err != nil {
		t.Fatalf("Branches(unknown kind) error = %v", err)
	}
	if unknown.Kind != BranchKindLocal {
		t.Fatalf("Branches(unknown kind).Kind = %q, want %q", unknown.Kind, BranchKindLocal)
	}
}

func TestBranchesMissingRepository(t *testing.T) {
	base := t.TempDir()
	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.Branches(ctx, BranchesArgs{RepoPath: filepath.FromSlash("missing/repo")})
	if err == nil {
		t.Fatal("Branches(missing) error = nil, want error")
	}
}

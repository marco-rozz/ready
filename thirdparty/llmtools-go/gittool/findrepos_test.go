package gittool

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFindReposHonorsDepthAndVisitedLimits(t *testing.T) {
	base := t.TempDir()
	rootRepo := mustInitRepo(t, base, false)
	_ = rootRepo
	deepRepoAbs := filepath.Join(base, "nested", "repo")
	_ = mustInitRepo(t, deepRepoAbs, false)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	depthLimited, err := tool.FindRepos(ctx, FindReposArgs{
		RootPath:       ".",
		IncludeBare:    new(false),
		MaxDepth:       1,
		MaxRepos:       10,
		MaxVisitedDirs: 100,
	})
	if err != nil {
		t.Fatalf("FindRepos(maxDepth=1) error = %v", err)
	}
	assertSamePath(t, depthLimited.RootPath, base)
	if depthLimited.Count != 1 || len(depthLimited.Repos) != 1 {
		t.Fatalf("FindRepos(maxDepth=1.Repos = %#v, want only the root repository", depthLimited.Repos)
	}
	if depthLimited.SkippedByDepth == 0 {
		t.Fatal("FindRepos(maxDepth=1).SkippedByDepth = 0, want skipped nested directories")
	}
	assertSamePath(t, depthLimited.Repos[0].RepoPath, base)

	visitedLimited, err := tool.FindRepos(ctx, FindReposArgs{
		RootPath:       ".",
		IncludeBare:    new(false),
		MaxDepth:       5,
		MaxRepos:       10,
		MaxVisitedDirs: 1,
	})
	if err != nil {
		t.Fatalf("FindRepos(maxVisitedDirs=1) error = %v", err)
	}
	if !visitedLimited.Truncated {
		t.Fatal("FindRepos(maxVisitedDirs=1).Truncated = false, want true")
	}
	if visitedLimited.Count != 1 || len(visitedLimited.Repos) != 1 {
		t.Fatalf("FindRepos(maxVisitedDirs=1).Repos = %#v, want only the root repository", visitedLimited.Repos)
	}
}

func TestFindReposFindsNestedAndBareRepositories(t *testing.T) {
	base := t.TempDir()

	workRepoAbs := filepath.Join(base, "work", "repo1")
	nestedRepoAbs := filepath.Join(base, "work", "nested", "repo2")
	bareRepoAbs := filepath.Join(base, "bare.git")

	if err := os.MkdirAll(workRepoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", workRepoAbs, err)
	}
	if err := os.MkdirAll(nestedRepoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", nestedRepoAbs, err)
	}
	if err := os.MkdirAll(bareRepoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", bareRepoAbs, err)
	}

	_ = mustInitRepo(t, workRepoAbs, false)
	_ = mustInitRepo(t, nestedRepoAbs, false)
	_ = mustInitRepo(t, bareRepoAbs, true)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	wantRoot := canonForPolicyExpectations(base)
	wantWorkRepo := canonForPolicyExpectations(workRepoAbs)
	wantNestedRepo := canonForPolicyExpectations(nestedRepoAbs)
	wantBareRepo := canonForPolicyExpectations(bareRepoAbs)

	withBare, err := tool.FindRepos(ctx, FindReposArgs{
		RootPath:       ".",
		IncludeBare:    new(true),
		MaxDepth:       5,
		MaxRepos:       10,
		MaxVisitedDirs: 100,
	})
	if err != nil {
		t.Fatalf("FindRepos(includeBare=true) error = %v", err)
	}
	assertSamePath(t, withBare.RootPath, wantRoot)
	if !withBare.IncludeBare {
		t.Fatal("FindRepos(includeBare=true).IncludeBare = false, want true")
	}
	if withBare.Count != 3 || len(withBare.Repos) != 3 {
		t.Fatalf("FindRepos(includeBare=true).Repos = %#v, want 3 repos", withBare.Repos)
	}
	paths := []string{withBare.Repos[0].RepoPath, withBare.Repos[1].RepoPath, withBare.Repos[2].RepoPath}
	if !reflect.DeepEqual(paths, sortStringsCopy(paths)) {
		t.Fatalf("FindRepos(includeBare=true).Repos not sorted by path: %#v", withBare.Repos)
	}

	wantRepos := map[string]FoundRepoInfo{
		normalizePathForCompare(wantBareRepo): {
			RepoPath: wantBareRepo,
			GitDir:   wantBareRepo,
			Bare:     true,
		},
		normalizePathForCompare(wantNestedRepo): {
			RepoPath: wantNestedRepo,
			GitDir:   filepath.Join(wantNestedRepo, ".git"),
			Bare:     false,
		},
		normalizePathForCompare(wantWorkRepo): {
			RepoPath: wantWorkRepo,
			GitDir:   filepath.Join(wantWorkRepo, ".git"),
			Bare:     false,
		},
	}

	var bareSeen bool
	for _, repo := range withBare.Repos {
		key := normalizePathForCompare(repo.RepoPath)
		want, ok := wantRepos[key]
		if !ok {
			t.Fatalf("FindRepos(includeBare=true) unexpected repo: %#v", repo)
		}
		assertSamePath(t, repo.RepoPath, want.RepoPath)
		assertSamePath(t, repo.GitDir, want.GitDir)
		if repo.Bare != want.Bare {
			t.Fatalf("FindRepos(includeBare=true) bare flag for %q = %v, want %v", repo.RepoPath, repo.Bare, want.Bare)
		}
		if repo.HeadHash != "" {
			t.Fatalf(
				"FindRepos(includeBare=true) head hash for %q = %q, want empty unborn head",
				repo.RepoPath,
				repo.HeadHash,
			)
		}
		if repo.Branch != "" {
			t.Fatalf(
				"FindRepos(includeBare=true) branch for %q = %q, want empty unborn head",
				repo.RepoPath,
				repo.Branch,
			)
		}
		if repo.DetachedHead {
			t.Fatalf("FindRepos(includeBare=true) detached flag for %q = true, want false", repo.RepoPath)
		}
		if !repo.UnbornHead {
			t.Fatalf("FindRepos(includeBare=true) unborn flag for %q = false, want true", repo.RepoPath)
		}
		if key == normalizePathForCompare(wantBareRepo) {
			bareSeen = true
		}
	}
	if !bareSeen {
		t.Fatalf("FindRepos(includeBare=true) repos = %#v, want bare repo included", withBare.Repos)
	}

	withoutBare, err := tool.FindRepos(ctx, FindReposArgs{
		RootPath:       ".",
		IncludeBare:    new(false),
		MaxDepth:       5,
		MaxRepos:       10,
		MaxVisitedDirs: 100,
	})
	if err != nil {
		t.Fatalf("FindRepos(includeBare=false) error = %v", err)
	}
	assertSamePath(t, withoutBare.RootPath, wantRoot)
	if withoutBare.IncludeBare {
		t.Fatal("FindRepos(includeBare=false).IncludeBare = true, want false")
	}
	if withoutBare.Count != 2 || len(withoutBare.Repos) != 2 {
		t.Fatalf("FindRepos(includeBare=false).Repos = %#v, want 2 repos", withoutBare.Repos)
	}
	for _, repo := range withoutBare.Repos {
		if repo.Bare {
			t.Fatalf("FindRepos(includeBare=false) unexpectedly returned bare repo: %#v", repo)
		}
		if normalizePathForCompare(repo.RepoPath) == normalizePathForCompare(wantBareRepo) {
			t.Fatalf("FindRepos(includeBare=false) returned bare repo path: %#v", repo)
		}
	}
}

func TestFindReposTruncatesByMaxRepos(t *testing.T) {
	base := t.TempDir()

	firstRepoAbs := filepath.Join(base, "a", "repo1")
	secondRepoAbs := filepath.Join(base, "b", "repo2")
	if err := os.MkdirAll(firstRepoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", firstRepoAbs, err)
	}
	if err := os.MkdirAll(secondRepoAbs, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", secondRepoAbs, err)
	}
	_ = mustInitRepo(t, firstRepoAbs, false)
	_ = mustInitRepo(t, secondRepoAbs, false)

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.FindRepos(ctx, FindReposArgs{
		RootPath:       ".",
		IncludeBare:    new(false),
		MaxDepth:       5,
		MaxRepos:       1,
		MaxVisitedDirs: 100,
	})
	if err != nil {
		t.Fatalf("FindRepos(maxRepos=1) error = %v", err)
	}
	assertSamePath(t, out.RootPath, canonForPolicyExpectations(base))
	if out.IncludeBare {
		t.Fatal("FindRepos(maxRepos=1).IncludeBare = true, want false")
	}
	if !out.Truncated {
		t.Fatal("FindRepos(maxRepos=1).Truncated = false, want true")
	}
	if out.Count != 1 || len(out.Repos) != 1 {
		t.Fatalf("FindRepos(maxRepos=1).Repos = %#v, want 1 repo", out.Repos)
	}
}

func TestFindReposRejectsInvalidRoots(t *testing.T) {
	base := t.TempDir()
	tool := newTestGitTool(t, base)
	ctx := t.Context()

	_, err := tool.FindRepos(ctx, FindReposArgs{RootPath: ""})
	if err == nil {
		t.Fatal("FindRepos(empty root) error = nil, want error")
	}

	missingFile := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(missingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", missingFile, err)
	}
	_, err = tool.FindRepos(ctx, FindReposArgs{RootPath: filepath.Base(missingFile)})
	if err == nil {
		t.Fatal("FindRepos(non-directory) error = nil, want error")
	}
}

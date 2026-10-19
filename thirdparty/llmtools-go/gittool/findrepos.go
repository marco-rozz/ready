package gittool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
)

const findReposFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/findrepos.FindRepos"

var findReposTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c017",
	Slug:          "gitfindrepos",
	Version:       spec.VersionOne,
	DisplayName:   "Git find repositories",
	Description:   "Discover local Git repositories below a directory using hard depth, visit, and result bounds.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"rootPath": {
		"type": "string",
		"description": "Directory under which repositories should be discovered."
	},
	"maxDepth": {
		"type": "integer",
		"description": "Maximum directory depth below rootPath.",
		"default": 5,
		"minimum": 0,
		"maximum": 12
	},
	"maxRepos": {
		"type": "integer",
		"description": "Maximum repositories to return.",
		"default": 100,
		"minimum": 1,
		"maximum": 1000
	},
	"maxVisitedDirs": {
		"type": "integer",
		"description": "Maximum directories to visit before truncating.",
		"default": 20000,
		"minimum": 1,
		"maximum": 100000
	},
	"includeBare": {
		"type": "boolean",
		"description": "If true, include bare repositories.",
		"default": true
	}
},
"required": ["rootPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: findReposFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type FindReposArgs struct {
	RootPath       string `json:"rootPath"`
	MaxDepth       int    `json:"maxDepth,omitempty"`
	MaxRepos       int    `json:"maxRepos,omitempty"`
	MaxVisitedDirs int    `json:"maxVisitedDirs,omitempty"`
	IncludeBare    *bool  `json:"includeBare,omitempty"`
}

type FoundRepoInfo struct {
	RepoPath     string `json:"repoPath"`
	GitDir       string `json:"gitDir"`
	Bare         bool   `json:"bare"`
	Branch       string `json:"branch,omitempty"`
	HeadHash     string `json:"headHash,omitempty"`
	DetachedHead bool   `json:"detachedHead"`
	UnbornHead   bool   `json:"unbornHead"`
}

type FindReposOut struct {
	RootPath       string          `json:"rootPath"`
	MaxDepth       int             `json:"maxDepth"`
	MaxRepos       int             `json:"maxRepos"`
	MaxVisitedDirs int             `json:"maxVisitedDirs"`
	IncludeBare    bool            `json:"includeBare"`
	Repos          []FoundRepoInfo `json:"repos"`
	Count          int             `json:"count"`
	VisitedDirs    int             `json:"visitedDirs"`
	SkippedByDepth int             `json:"skippedByDepth"`
	Truncated      bool            `json:"truncated"`
}

func findRepos(ctx context.Context, snap gitToolSnapshot, args FindReposArgs) (*FindReposOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.RootPath) == "" {
		return nil, errors.New("rootPath is required")
	}

	root, err := snap.policy.ResolvePath(args.RootPath, "")
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, errors.New("rootPath is not a directory")
	}

	includeBare := true
	if args.IncludeBare != nil {
		includeBare = *args.IncludeBare
	}
	maxDepth := normalizePositiveInt(args.MaxDepth, defaultFindMaxDepth, 0, hardFindMaxDepth)
	maxRepos := normalizePositiveInt(args.MaxRepos, defaultFindMaxRepos, 1, hardFindMaxRepos)
	maxVisited := normalizePositiveInt(args.MaxVisitedDirs, defaultFindMaxVisited, 1, hardFindMaxVisited)

	out := &FindReposOut{
		RootPath:       root,
		MaxDepth:       maxDepth,
		MaxRepos:       maxRepos,
		MaxVisitedDirs: maxVisited,
		IncludeBare:    includeBare,
	}

	err = filepath.WalkDir(root, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			//nolint:nilerr // Walk err.
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}

		depth := repoFindDepth(root, current)
		if depth > maxDepth {
			out.SkippedByDepth++
			return filepath.SkipDir
		}

		out.VisitedDirs++
		if out.VisitedDirs > maxVisited {
			out.Truncated = true
			return errStopIteration
		}

		if d.Name() == ".git" && current != root {
			return filepath.SkipDir
		}

		info, ok := inspectRepoAtPath(current, includeBare)
		if !ok {
			return nil
		}
		out.Repos = append(out.Repos, info)
		if len(out.Repos) >= maxRepos {
			out.Truncated = true
			return errStopIteration
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, err
	}

	sort.Slice(out.Repos, func(i, j int) bool {
		return out.Repos[i].RepoPath < out.Repos[j].RepoPath
	})
	out.Count = len(out.Repos)
	return out, nil
}

func repoFindDepth(root, current string) int {
	rel, err := filepath.Rel(root, current)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(os.PathSeparator)))
}

func inspectRepoAtPath(p string, includeBare bool) (FoundRepoInfo, bool) {
	gitDir := filepath.Join(p, ".git")
	if st, err := os.Stat(gitDir); err == nil && st.IsDir() {
		return openFoundRepo(p, gitDir, false)
	}
	if st, err := os.Stat(gitDir); err == nil && !st.IsDir() {
		return openFoundRepo(p, gitDir, false)
	}

	if !includeBare {
		return FoundRepoInfo{}, false
	}
	if !looksLikeBareRepo(p) {
		return FoundRepoInfo{}, false
	}
	return openFoundRepo(p, p, true)
}

func looksLikeBareRepo(p string) bool {
	if st, err := os.Stat(filepath.Join(p, "HEAD")); err != nil || st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(p, "objects")); err != nil || !st.IsDir() {
		return false
	}
	if st, err := os.Stat(filepath.Join(p, "refs")); err != nil || !st.IsDir() {
		return false
	}
	return true
}

func openFoundRepo(repoPath, gitDir string, bare bool) (FoundRepoInfo, bool) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return FoundRepoInfo{}, false
	}
	branch, hash, detached, unborn := headInfoDetailed(repo)
	return FoundRepoInfo{
		RepoPath:     repoPath,
		GitDir:       gitDir,
		Bare:         bare,
		Branch:       branch,
		HeadHash:     hash,
		DetachedHead: detached,
		UnbornHead:   unborn,
	}, true
}

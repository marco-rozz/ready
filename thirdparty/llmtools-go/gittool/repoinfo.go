package gittool

import (
	"context"
	"path/filepath"
	"sort"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing"
)

const repoInfoFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/repoinfo.RepoInfo"

var repoInfoTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c019",
	Slug:          "gitrepoinfo",
	Version:       spec.VersionOne,
	DisplayName:   "Git repository info",
	Description:   "Return compact local Git repository metadata, including bare/unborn/detached state, refs, remotes, and author config.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: repoInfoFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type RepoInfoArgs struct {
	RepoPath string `json:"repoPath"`
}

type RepoInfoOut struct {
	RepoPath           string       `json:"repoPath"`
	GitDir             string       `json:"gitDir"`
	Bare               bool         `json:"bare"`
	HeadHash           string       `json:"headHash,omitempty"`
	Branch             string       `json:"branch,omitempty"`
	DetachedHead       bool         `json:"detachedHead"`
	UnbornHead         bool         `json:"unbornHead"`
	DefaultAuthorName  string       `json:"defaultAuthorName,omitempty"`
	DefaultAuthorEmail string       `json:"defaultAuthorEmail,omitempty"`
	Remotes            []RemoteInfo `json:"remotes,omitempty"`
	LocalBranchCount   int          `json:"localBranchCount"`
	RemoteBranchCount  int          `json:"remoteBranchCount"`
	TagCount           int          `json:"tagCount"`
	IndexState         IndexState   `json:"indexState,omitzero"`
}

func repoInfo(ctx context.Context, snap gitToolSnapshot, args RepoInfoArgs) (*RepoInfoOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	branch, hash, detached, unborn := headInfoDetailed(repo)
	out := &RepoInfoOut{
		RepoPath:     abs,
		GitDir:       filepath.Join(abs, ".git"),
		HeadHash:     hash,
		Branch:       branch,
		DetachedHead: detached,
		UnbornHead:   unborn,
	}

	if wt, err := repo.Worktree(); err == nil {
		out.IndexState, _ = detectIndexState(repo, wt)
	} else {
		out.Bare = true
		out.GitDir = abs
	}

	if cfg, err := repo.Config(); err == nil && cfg != nil {
		out.DefaultAuthorName = cfg.User.Name
		out.DefaultAuthorEmail = cfg.User.Email
		remoteNames := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			remoteNames = append(remoteNames, name)
		}
		sort.Strings(remoteNames)
		for _, name := range remoteNames {
			remote := cfg.Remotes[name]
			if remote == nil {
				continue
			}
			out.Remotes = append(out.Remotes, RemoteInfo{Name: name, URLs: append([]string(nil), remote.URLs...)})
		}
	}

	if iter, err := repo.Branches(); err == nil {
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			out.LocalBranchCount++
			return nil
		})
		iter.Close()
	}
	if iter, err := repo.References(); err == nil {
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if ref.Name().IsRemote() {
				out.RemoteBranchCount++
			}
			return nil
		})
		iter.Close()
	}
	if iter, err := repo.Tags(); err == nil {
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			out.TagCount++
			return nil
		})
		iter.Close()
	}

	return out, nil
}

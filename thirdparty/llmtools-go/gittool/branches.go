package gittool

import (
	"context"
	"sort"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing"
)

const branchesFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/branches.Branches"

var branchesTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c006",
	Slug:          "gitbranch",
	Version:       spec.VersionOne,
	DisplayName:   "Git branch",
	Description:   "List local, remote, or all branches in a local Git repository.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"kind": {
		"type": "string",
		"enum": ["local", "remote", "all"],
		"description": "Branch kind to list.",
		"default": "local"
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: branchesFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type BranchKind string

const (
	BranchKindLocal  BranchKind = "local"
	BranchKindRemote BranchKind = "remote"
	BranchKindAll    BranchKind = "all"
)

type BranchesArgs struct {
	RepoPath string     `json:"repoPath"`
	Kind     BranchKind `json:"kind,omitempty"`
}

type BranchInfo struct {
	Name    string     `json:"name"`
	Kind    BranchKind `json:"kind"`
	Hash    string     `json:"hash"`
	Current bool       `json:"current"`
}

type BranchesOut struct {
	RepoPath string       `json:"repoPath"`
	Kind     BranchKind   `json:"kind"`
	Current  string       `json:"current,omitempty"`
	Branches []BranchInfo `json:"branches"`
}

func branches(ctx context.Context, snap gitToolSnapshot, args BranchesArgs) (*BranchesOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	k := strings.ToLower(strings.TrimSpace(string(args.Kind)))
	var kind BranchKind
	switch k {
	case "remote":
		kind = BranchKindRemote
	case "all":
		kind = BranchKindAll
	default:
		kind = BranchKindLocal
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	current, _, _ := headInfo(repo)
	out := &BranchesOut{
		RepoPath: abs,
		Kind:     kind,
		Current:  current,
	}

	if kind == BranchKindLocal || kind == BranchKindAll {
		iter, err := repo.Branches()
		if err != nil {
			return nil, err
		}
		defer iter.Close()

		if err := iter.ForEach(func(ref *plumbing.Reference) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := ref.Name().Short()
			out.Branches = append(out.Branches, BranchInfo{
				Name:    name,
				Kind:    BranchKindLocal,
				Hash:    ref.Hash().String(),
				Current: name == current,
			})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	if kind == BranchKindRemote || kind == BranchKindAll {
		iter, err := repo.References()
		if err != nil {
			return nil, err
		}
		defer iter.Close()

		if err := iter.ForEach(func(ref *plumbing.Reference) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !ref.Name().IsRemote() {
				return nil
			}
			name := strings.TrimPrefix(string(ref.Name()), "refs/remotes/")
			out.Branches = append(out.Branches, BranchInfo{
				Name: name,
				Kind: BranchKindRemote,
				Hash: ref.Hash().String(),
			})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	sort.Slice(out.Branches, func(i, j int) bool {
		if out.Branches[i].Kind != out.Branches[j].Kind {
			return out.Branches[i].Kind < out.Branches[j].Kind
		}
		return out.Branches[i].Name < out.Branches[j].Name
	})

	return out, nil
}

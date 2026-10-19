package gittool

import (
	"context"
	"fmt"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const createBranchFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/createbranch.CreateBranch"

var createBranchTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c00b",
	Slug:          "gitcreatebranch",
	Version:       spec.VersionOne,
	DisplayName:   "Git create branch",
	Description:   "Create a local Git branch from a revision, optionally checking it out.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"name": {
		"type": "string",
		"description": "New local branch name."
	},
	"startPoint": {
		"type": "string",
		"description": "Revision to create the branch from.",
		"default": "HEAD"
	},
	"checkout": {
		"type": "boolean",
		"description": "If true, checkout the branch after creating it.",
		"default": false
	}
},
"required": ["repoPath", "name"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: createBranchFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type CreateBranchArgs struct {
	RepoPath   string `json:"repoPath"`
	Name       string `json:"name"`
	StartPoint string `json:"startPoint,omitempty"`
	Checkout   bool   `json:"checkout,omitempty"`
}

type CreateBranchOut struct {
	RepoPath    string `json:"repoPath"`
	Name        string `json:"name"`
	StartPoint  string `json:"startPoint"`
	Hash        string `json:"hash"`
	CheckedOut  bool   `json:"checkedOut"`
	CurrentHead string `json:"currentHead,omitempty"`
}

func createBranch(ctx context.Context, snap gitToolSnapshot, args CreateBranchArgs) (*CreateBranchOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(args.Name)
	if err := validateLocalBranchName(name); err != nil {
		return nil, err
	}

	startPoint := strings.TrimSpace(args.StartPoint)
	if startPoint == "" {
		startPoint = revisionHead
	}
	if err := validateRevision(startPoint); err != nil {
		return nil, err
	}

	repo, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	branchRefName := plumbing.NewBranchReferenceName(name)
	if _, err := repo.Reference(branchRefName, true); err == nil {
		return nil, fmt.Errorf("branch already exists: %s", name)
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(startPoint))
	if err != nil {
		return nil, err
	}

	ref := plumbing.NewHashReference(branchRefName, *hash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return nil, err
	}

	out := &CreateBranchOut{
		RepoPath:   abs,
		Name:       name,
		StartPoint: startPoint,
		Hash:       hash.String(),
		CheckedOut: false,
	}

	if args.Checkout {
		if err := ensureNoIndexConflicts(repo, wt); err != nil {
			return nil, err
		}
		if err := wt.Checkout(&git.CheckoutOptions{
			Branch: branchRefName,
		}); err != nil {
			return nil, err
		}
		out.CheckedOut = true
		current, currentHash, _ := headInfo(repo)
		if current != "" {
			out.CurrentHead = current
		} else {
			out.CurrentHead = currentHash
		}
	}

	return out, nil
}

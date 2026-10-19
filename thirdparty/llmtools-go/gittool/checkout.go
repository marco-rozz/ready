package gittool

import (
	"context"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const checkoutFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/checkout.Checkout"

var checkoutTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c00c",
	Slug:          "gitcheckout",
	Version:       spec.VersionOne,
	DisplayName:   "Git checkout",
	Description:   "Checkout a local branch, optionally creating it from a startPoint.",
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
		"description": "Local branch name to checkout."
	},
	"create": {
		"type": "boolean",
		"description": "If true, create the branch before checkout.",
		"default": false
	},
	"startPoint": {
		"type": "string",
		"description": "Revision used when create=true.",
		"default": "HEAD"
	},
	"force": {
		"type": "boolean",
		"description": "If true, force checkout. Defaults to false because it can discard local changes.",
		"default": false
	}
},
"required": ["repoPath", "name"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: checkoutFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type CheckoutArgs struct {
	RepoPath   string `json:"repoPath"`
	Name       string `json:"name"`
	Create     bool   `json:"create,omitempty"`
	StartPoint string `json:"startPoint,omitempty"`
	Force      bool   `json:"force,omitempty"`
}

type CheckoutOut struct {
	RepoPath    string `json:"repoPath"`
	Name        string `json:"name"`
	Created     bool   `json:"created"`
	Force       bool   `json:"force"`
	StartPoint  string `json:"startPoint,omitempty"`
	CurrentHead string `json:"currentHead,omitempty"`
	HeadHash    string `json:"headHash,omitempty"`
}

func checkout(ctx context.Context, snap gitToolSnapshot, args CheckoutArgs) (*CheckoutOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(args.Name)
	if err := validateLocalBranchName(name); err != nil {
		return nil, err
	}

	repo, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	branchRefName := plumbing.NewBranchReferenceName(name)
	opts := &git.CheckoutOptions{
		Branch: branchRefName,
		Create: args.Create,
		Force:  args.Force,
	}

	startPoint := strings.TrimSpace(args.StartPoint)
	if args.Create {
		if startPoint == "" {
			startPoint = revisionHead
		}
		if err := validateRevision(startPoint); err != nil {
			return nil, err
		}
		hash, err := repo.ResolveRevision(plumbing.Revision(startPoint))
		if err != nil {
			return nil, err
		}
		opts.Hash = *hash
	}

	if err := ensureNoIndexConflicts(repo, wt); err != nil {
		return nil, err
	}

	if err := wt.Checkout(opts); err != nil {
		return nil, err
	}

	current, hash, _ := headInfo(repo)
	currentHead := current
	if currentHead == "" {
		currentHead = hash
	}

	return &CheckoutOut{
		RepoPath:    abs,
		Name:        name,
		Created:     args.Create,
		Force:       args.Force,
		StartPoint:  startPoint,
		CurrentHead: currentHead,
		HeadHash:    hash,
	}, nil
}

package gittool

import (
	"context"
	"errors"

	"github.com/flexigpt/llmtools-go/spec"
)

const addFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/add.Add"

var addTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c008",
	Slug:          "gitadd",
	Version:       spec.VersionOne,
	DisplayName:   "Git add",
	Description:   "Add repository-relative paths to the index, or add all changes, in a local Git repository.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"paths": {
		"type": "array",
		"items": {"type": "string"},
		"description": "Repository-relative paths to add. Required unless all=true."
	},
	"all": {
		"type": "boolean",
		"description": "If true, add all changes from the worktree root.",
		"default": false
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: addFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type AddArgs struct {
	RepoPath string   `json:"repoPath"`
	Paths    []string `json:"paths,omitempty"`
	All      bool     `json:"all,omitempty"`
}

type AddOut struct {
	RepoPath string   `json:"repoPath"`
	All      bool     `json:"all"`
	Paths    []string `json:"paths,omitempty"`
}

func add(ctx context.Context, snap gitToolSnapshot, args AddArgs) (*AddOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	_, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	out := &AddOut{
		RepoPath: abs,
		All:      args.All,
	}

	if args.All {
		if snap.blockSymlinks {
			return nil, errors.New("all=true is not allowed when symlink blocking is enabled; pass explicit paths")
		}
		if _, err := wt.Add("."); err != nil {
			return nil, err
		}
		out.Paths = []string{"."}
		return out, nil
	}

	if len(args.Paths) == 0 {
		return nil, errors.New("paths is required unless all=true")
	}
	if len(args.Paths) > hardStagePaths {
		return nil, errors.New("too many paths to add")
	}

	normalized := make([]string, 0, len(args.Paths))
	for _, p := range args.Paths {
		cleaned, err := normalizeRepoRelativePath(p)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, cleaned)
	}
	normalized = sortedStrings(normalized)

	for _, p := range normalized {
		if snap.blockSymlinks {
			if err := validateNoSymlinkTraversal(abs, p); err != nil {
				return nil, err
			}
		}
		if _, err := wt.Add(p); err != nil {
			return nil, err
		}
	}

	out.Paths = normalized
	return out, nil
}

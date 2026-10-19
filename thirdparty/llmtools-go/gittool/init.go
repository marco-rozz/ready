package gittool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
)

const initFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/init.Init"

var initTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c00d",
	Slug:          "gitinit",
	Version:       spec.VersionOne,
	DisplayName:   "Git init",
	Description:   "Initialize a local Git repository at repoPath using pure Go Git operations.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Directory path where the repository should be initialized."
	},
	"bare": {
		"type": "boolean",
		"description": "If true, initialize a bare repository.",
		"default": false
	},
	"createParents": {
		"type": "boolean",
		"description": "If true, create missing parent directories.",
		"default": false
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: initFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type InitArgs struct {
	RepoPath      string `json:"repoPath"`
	Bare          bool   `json:"bare,omitempty"`
	CreateParents bool   `json:"createParents,omitempty"`
}

type InitOut struct {
	RepoPath string `json:"repoPath"`
	GitDir   string `json:"gitDir"`
	Bare     bool   `json:"bare"`
	Action   string `json:"action"`
}

func initRepo(ctx context.Context, snap gitToolSnapshot, args InitArgs) (*InitOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args.RepoPath == "" {
		return nil, errors.New("repoPath is required")
	}

	abs, err := snap.policy.ResolvePath(args.RepoPath, "")
	if err != nil {
		return nil, err
	}

	if args.CreateParents {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return nil, err
		}
	} else {
		st, err := os.Stat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("repoPath does not exist and createParents=false: %s", abs)
			}
			return nil, err
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("repoPath is not a directory: %s", abs)
		}
	}

	if _, err := git.PlainInitWithOptions(abs, &git.PlainInitOptions{Bare: args.Bare}); err != nil {
		return nil, err
	}

	gitDir := filepath.Join(abs, ".git")
	if args.Bare {
		gitDir = abs
	}
	return &InitOut{
		RepoPath: abs,
		GitDir:   gitDir,
		Bare:     args.Bare,
		Action:   "initialized",
	}, nil
}

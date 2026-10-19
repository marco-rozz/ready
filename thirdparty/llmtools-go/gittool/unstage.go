package gittool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

const resetFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/reset.Reset"

var resetTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c009",
	Slug:          "gitreset",
	Version:       spec.VersionOne,
	DisplayName:   "Git reset",
	Description:   "Run a safe mixed reset to unstage all staged changes while preserving the working tree.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"mode": {
		"type": "string",
		"enum": ["mixed", "hard"],
		"description": "Reset mode. mixed resets the index and preserves the worktree. hard resets index and worktree and requires confirmDiscard=true.",
		"default": "mixed"
	},
	"revision": {
		"type": "string",
		"description": "Revision to reset to.",
		"default": "HEAD"
	},
	"paths": {
		"type": "array",
		"items": {"type": "string"},
		"description": "Optional repository-relative paths for a path-limited mixed reset."
	},
	"confirmDiscard": {
		"type": "boolean",
		"description": "Required for mode=hard because it discards worktree changes.",
		"default": false
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: resetFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ResetMode string

const (
	ResetModeMixed ResetMode = "mixed"
	ResetModeHard  ResetMode = "hard"
)

type ResetArgs struct {
	RepoPath       string    `json:"repoPath"`
	Mode           ResetMode `json:"mode,omitempty"`
	Revision       string    `json:"revision,omitempty"`
	Paths          []string  `json:"paths,omitempty"`
	ConfirmDiscard bool      `json:"confirmDiscard,omitempty"`
}

type ResetOut struct {
	RepoPath          string    `json:"repoPath"`
	Mode              ResetMode `json:"mode"`
	Revision          string    `json:"revision"`
	HeadHash          string    `json:"headHash,omitempty"`
	Paths             []string  `json:"paths,omitempty"`
	DiscardedWorktree bool      `json:"discardedWorktree"`
	Action            string    `json:"action"`
}

func reset(ctx context.Context, snap gitToolSnapshot, args ResetArgs) (*ResetOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	mode := ResetMode(strings.ToLower(strings.TrimSpace(string(args.Mode))))
	switch mode {
	case "", ResetModeMixed:
		mode = ResetModeMixed
	case ResetModeHard:
	default:
		return nil, errors.New(`mode must be "mixed" or "hard"`)
	}

	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	if err := validateRevision(revision); err != nil {
		return nil, err
	}

	paths, err := normalizeRepoRelativePaths(args.Paths, true)
	if err != nil {
		return nil, err
	}
	if len(paths) > 0 && mode != ResetModeMixed {
		return nil, errors.New("path-limited reset supports only mode=mixed")
	}
	if mode == ResetModeHard && !args.ConfirmDiscard {
		return nil, errors.New("confirmDiscard=true is required for mode=hard")
	}

	_, _, _, unborn := headInfoDetailed(repo)
	if unborn && revision == revisionHead {
		if mode != ResetModeMixed {
			return nil, errors.New("unborn HEAD supports only mode=mixed reset")
		}
		if err := resetUnbornIndex(repo, paths); err != nil {
			return nil, err
		}
		return &ResetOut{
			RepoPath: abs,
			Mode:     mode,
			Revision: revision,
			Paths:    paths,
			Action:   "mixed-reset-unborn-index",
		}, nil
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return nil, fmt.Errorf("resolve revision %q: %w", revision, err)
	}

	resetMode := git.MixedReset
	if mode == ResetModeHard {
		resetMode = git.HardReset
	}

	opts := &git.ResetOptions{
		Commit: *hash,
		Mode:   resetMode,
	}
	if len(paths) > 0 {
		opts.Files = paths
	}
	if err := wt.Reset(opts); err != nil {
		return nil, err
	}

	return &ResetOut{
		RepoPath:          abs,
		Mode:              mode,
		Revision:          revision,
		HeadHash:          hash.String(),
		Paths:             paths,
		DiscardedWorktree: mode == ResetModeHard,
		Action:            string(mode) + "-reset",
	}, nil
}

func resetUnbornIndex(repo *git.Repository, paths []string) error {
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	if idx == nil {
		return nil
	}
	if len(paths) == 0 {
		idx.Entries = nil
		return repo.Storer.SetIndex(idx)
	}
	kept := idx.Entries[:0]
	for _, entry := range idx.Entries {
		if entry == nil {
			continue
		}
		if matchesRepoRelativePathFilter(entry.Name, paths) {
			continue
		}
		kept = append(kept, entry)
	}
	idx.Entries = kept
	return repo.Storer.SetIndex(idx)
}

package gittool

import (
	"context"
	"errors"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const diffFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/diff.Diff"

var diffTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c003",
	Slug:          "gitdiff",
	Version:       spec.VersionOne,
	DisplayName:   "Git diff",
	Description:   "Return a bounded unified diff for working changes, staged changes, or two revisions in a local Git repository.",
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
		"enum": ["working", "staged", "refs"],
		"description": "Diff kind. working=unstaged worktree changes, staged=index changes, refs=diff base..target.",
		"default": "working"
	},
	"base": {
		"type": "string",
		"description": "Base revision for kind=refs."
	},
	"target": {
		"type": "string",
		"description": "Target revision for kind=refs.",
		"default": "HEAD"
	},
	"paths": {
		"type": "array",
		"items": {"type": "string"},
		"description": "Optional repository-relative paths to include."
	},
	"contextLines": {
		"type": "integer",
		"description": "Unified diff context lines.",
		"default": 3,
		"minimum": 0,
		"maximum": 20
	},
	"maxBytes": {
		"type": "integer",
		"description": "Maximum diff bytes returned. Hard capped by the implementation.",
		"default": 1048576,
		"minimum": 34,
		"maximum": 2097152
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: diffFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type DiffKind string

const (
	DiffKindWorking DiffKind = "working"
	DiffKindStaged  DiffKind = "staged"
	DiffKindRefs    DiffKind = "refs"
)

type DiffArgs struct {
	RepoPath     string   `json:"repoPath"`
	Kind         DiffKind `json:"kind,omitempty"`
	Base         string   `json:"base,omitempty"`
	Target       string   `json:"target,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	ContextLines *int     `json:"contextLines,omitempty"`
	MaxBytes     int      `json:"maxBytes,omitempty"`
}

type DiffOut struct {
	RepoPath           string     `json:"repoPath"`
	Kind               DiffKind   `json:"kind"`
	Base               string     `json:"base,omitempty"`
	Target             string     `json:"target,omitempty"`
	Paths              []string   `json:"paths,omitempty"`
	ContextLines       int        `json:"contextLines"`
	Diff               string     `json:"diff"`
	Bytes              int        `json:"bytes"`
	Truncated          bool       `json:"truncated"`
	OmittedBinaryFiles int        `json:"omittedBinaryFiles"`
	SkippedLargeFiles  int        `json:"skippedLargeFiles"`
	OutputMeta         OutputMeta `json:"outputMeta"`
}

func diff(ctx context.Context, snap gitToolSnapshot, args DiffArgs) (*DiffOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	k := strings.ToLower(strings.TrimSpace(string(args.Kind)))
	var kind DiffKind
	switch k {
	case "staged":
		kind = DiffKindStaged
	case "refs":
		kind = DiffKindRefs
	default:
		kind = DiffKindWorking
	}

	contextLines := normalizeOptionalInt(args.ContextLines, defaultContextLines, 0, 20)
	maxBytes := normalizePositiveInt(args.MaxBytes, defaultDiffMaxBytes, 64, hardDiffMaxBytes)
	pathFilters, err := normalizeRepoRelativePaths(args.Paths, true)
	if err != nil {
		return nil, err
	}

	var diffText string
	var abs string
	var omittedBinaryFiles int
	var skippedLargeFiles int

	switch kind {
	case DiffKindWorking:
		repo, wt, repoAbs, err := openWorktree(ctx, snap, args.RepoPath)
		if err != nil {
			return nil, err
		}
		abs = repoAbs

		st, err := wt.Status()
		if err != nil {
			return nil, err
		}
		idx, err := indexEntriesByPath(repo)
		if err != nil {
			return nil, err
		}
		paths := statusDiffPaths(st, DiffKindWorking, pathFilters)
		var b strings.Builder
		for _, p := range paths {
			oldData, oldExists, oldTruncated, err := indexPathContentLimited(repo, idx, p, hardBlobReadBytes)
			if err != nil {
				return nil, err
			}
			if snap.blockSymlinks {
				if err := validateNoSymlinkTraversal(abs, p); err != nil {
					return nil, err
				}
			}
			newData, newExists, newTruncated, err := worktreePathContentLimited(wt, p, hardBlobReadBytes)
			if err != nil {
				return nil, err
			}
			part, binaryOmitted, largeSkipped := unifiedFileDiff(
				p, oldData, oldExists, oldTruncated, newData, newExists, newTruncated, contextLines,
			)
			if binaryOmitted {
				omittedBinaryFiles++
			}
			if largeSkipped {
				skippedLargeFiles++
			}
			b.WriteString(part)
		}
		diffText = b.String()

	case DiffKindStaged:
		repo, wt, repoAbs, err := openWorktree(ctx, snap, args.RepoPath)
		if err != nil {
			return nil, err
		}
		abs = repoAbs

		st, err := wt.Status()
		if err != nil {
			return nil, err
		}
		var headTree *object.Tree
		if headCommit, err := resolveCommit(repo, revisionHead); err == nil {
			if tree, treeErr := headCommit.Tree(); treeErr == nil {
				headTree = tree
			}
		}
		idx, err := indexEntriesByPath(repo)
		if err != nil {
			return nil, err
		}
		paths := statusDiffPaths(st, DiffKindStaged, pathFilters)
		var b strings.Builder
		for _, p := range paths {
			oldData, oldExists, oldTruncated, err := treePathContentLimited(headTree, p, hardBlobReadBytes)
			if err != nil {
				return nil, err
			}
			newData, newExists, newTruncated, err := indexPathContentLimited(repo, idx, p, hardBlobReadBytes)
			if err != nil {
				return nil, err
			}
			part, binaryOmitted, largeSkipped := unifiedFileDiff(
				p, oldData, oldExists, oldTruncated, newData, newExists, newTruncated, contextLines,
			)
			if binaryOmitted {
				omittedBinaryFiles++
			}
			if largeSkipped {
				skippedLargeFiles++
			}
			b.WriteString(part)
		}
		diffText = b.String()

	case DiffKindRefs:
		repo, repoAbs, err := openRepository(ctx, snap, args.RepoPath)
		if err != nil {
			return nil, err
		}
		abs = repoAbs

		if strings.TrimSpace(args.Base) == "" {
			return nil, errors.New("base is required when kind=refs")
		}
		target := strings.TrimSpace(args.Target)
		if target == "" {
			target = revisionHead
		}

		baseCommit, err := resolveCommit(repo, args.Base)
		if err != nil {
			return nil, err
		}
		targetCommit, err := resolveCommit(repo, target)
		if err != nil {
			return nil, err
		}

		baseTree, err := baseCommit.Tree()
		if err != nil {
			return nil, err
		}
		targetTree, err := targetCommit.Tree()
		if err != nil {
			return nil, err
		}
		changes, err := baseTree.Diff(targetTree)
		if err != nil {
			return nil, err
		}
		changes = filterObjectChangesByPaths(changes, pathFilters)
		if len(pathFilters) == 0 && contextLines == defaultContextLines {
			patch, err := baseTree.Patch(targetTree)
			if err != nil {
				return nil, err
			}
			diffText = patch.String()
		} else {
			diffText, omittedBinaryFiles, skippedLargeFiles, err = treeChangesUnifiedDiff(
				ctx, baseTree, targetTree, changes, contextLines,
			)
			if err != nil {
				return nil, err
			}
		}
		args.Target = target

	default:
		return nil, errors.New(`kind must be "working", "staged", or "refs"`)
	}

	limited, truncated := limitStringBytes(diffText, maxBytes)
	return &DiffOut{
		RepoPath:           abs,
		Kind:               kind,
		Base:               strings.TrimSpace(args.Base),
		Target:             strings.TrimSpace(args.Target),
		Paths:              pathFilters,
		ContextLines:       contextLines,
		Diff:               limited,
		Bytes:              len(limited),
		Truncated:          truncated,
		OmittedBinaryFiles: omittedBinaryFiles,
		SkippedLargeFiles:  skippedLargeFiles,
		OutputMeta: OutputMeta{
			Bytes:              len(limited),
			Truncated:          truncated,
			OmittedBinaryFiles: omittedBinaryFiles,
			SkippedLargeFiles:  skippedLargeFiles,
			MaxBytes:           maxBytes,
		},
	}, nil
}

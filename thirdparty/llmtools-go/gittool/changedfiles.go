package gittool

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
)

const changedFilesFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/changedfiles.ChangedFiles"

var changedFilesTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c014",
	Slug:          "gitchangedfiles",
	Version:       spec.VersionOne,
	DisplayName:   "Git changed files",
	Description:   "Return a bounded list of changed files without producing a full diff.",
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
		"description": "Change kind. working=unstaged worktree changes, staged=index changes, refs=changes from base to target.",
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
	"maxCount": {
		"type": "integer",
		"description": "Maximum changed files to return.",
		"default": 500,
		"minimum": 1,
		"maximum": 5000
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: changedFilesFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ChangedFilesArgs struct {
	RepoPath string   `json:"repoPath"`
	Kind     DiffKind `json:"kind,omitempty"`
	Base     string   `json:"base,omitempty"`
	Target   string   `json:"target,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	MaxCount int      `json:"maxCount,omitempty"`
}

type ChangedFileInfo struct {
	Path     string `json:"path"`
	OldPath  string `json:"oldPath,omitempty"`
	Status   string `json:"status"`
	Staging  string `json:"staging,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

type ChangedFilesOut struct {
	RepoPath  string            `json:"repoPath"`
	Kind      DiffKind          `json:"kind"`
	Base      string            `json:"base,omitempty"`
	Target    string            `json:"target,omitempty"`
	Paths     []string          `json:"paths,omitempty"`
	MaxCount  int               `json:"maxCount"`
	Files     []ChangedFileInfo `json:"files"`
	Count     int               `json:"count"`
	Truncated bool              `json:"truncated"`
}

func changedFiles(ctx context.Context, snap gitToolSnapshot, args ChangedFilesArgs) (*ChangedFilesOut, error) {
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

	pathFilters, err := normalizeRepoRelativePaths(args.Paths, true)
	if err != nil {
		return nil, err
	}
	maxCount := normalizePositiveInt(args.MaxCount, 500, 1, 5000)

	out := &ChangedFilesOut{
		Kind:     kind,
		Base:     strings.TrimSpace(args.Base),
		Paths:    pathFilters,
		MaxCount: maxCount,
	}

	switch kind {
	case DiffKindWorking, DiffKindStaged:
		_, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
		if err != nil {
			return nil, err
		}
		out.RepoPath = abs

		st, err := wt.Status()
		if err != nil {
			return nil, err
		}

		paths := make([]string, 0, len(st))
		for p := range st {
			if matchesRepoRelativePathFilter(p, pathFilters) {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)

		for _, p := range paths {
			fs := st[p]
			var code string
			switch kind {
			case DiffKindWorking:
				if fs.Worktree == ' ' {
					continue
				}
				code = statusCodeName(fs.Worktree)
			case DiffKindStaged:
				if fs.Staging == ' ' || fs.Staging == '?' {
					continue
				}
				code = statusCodeName(fs.Staging)
			default:
			}
			out.Files = append(out.Files, ChangedFileInfo{
				Path:     p,
				Status:   code,
				Staging:  statusCodeName(fs.Staging),
				Worktree: statusCodeName(fs.Worktree),
			})
			if len(out.Files) >= maxCount {
				out.Truncated = true
				break
			}
		}

	case DiffKindRefs:
		repo, abs, err := openRepository(ctx, snap, args.RepoPath)
		if err != nil {
			return nil, err
		}
		out.RepoPath = abs

		if strings.TrimSpace(args.Base) == "" {
			return nil, errors.New("base is required when kind=refs")
		}
		target := strings.TrimSpace(args.Target)
		if target == "" {
			target = revisionHead
		}
		out.Target = target

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

		for _, change := range changes {
			path := change.To.Name
			oldPath := change.From.Name
			if path == "" {
				path = oldPath
			}
			status := "modified"
			switch {
			case change.From.Name == "":
				status = "added"
			case change.To.Name == "":
				status = "deleted"
			case change.From.Name != change.To.Name:
				status = "renamed"
			}
			out.Files = append(out.Files, ChangedFileInfo{
				Path:    path,
				OldPath: oldPath,
				Status:  status,
			})
			if len(out.Files) >= maxCount {
				out.Truncated = true
				break
			}
		}

	default:
		return nil, errors.New(`kind must be "working", "staged", or "refs"`)
	}

	out.Count = len(out.Files)
	return out, nil
}

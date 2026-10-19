package gittool

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const listTreeFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/listtree.ListTree"

var listTreeTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c015",
	Slug:          "gitlisttree",
	Version:       spec.VersionOne,
	DisplayName:   "Git list tree",
	Description:   "List files and optionally directories at HEAD or another revision without checking it out.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"revision": {
		"type": "string",
		"description": "Revision to inspect.",
		"default": "HEAD"
	},
	"path": {
		"type": "string",
		"description": "Repository-relative path to list.",
		"default": "."
	},
	"recursive": {
		"type": "boolean",
		"description": "If true, recursively list files below path.",
		"default": true
	},
	"includeDirs": {
		"type": "boolean",
		"description": "If true, include tree/directory entries.",
		"default": false
	},
	"maxEntries": {
		"type": "integer",
		"description": "Maximum entries to return.",
		"default": 1000,
		"minimum": 1,
		"maximum": 10000
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: listTreeFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ListTreeArgs struct {
	RepoPath    string `json:"repoPath"`
	Revision    string `json:"revision,omitempty"`
	Path        string `json:"path,omitempty"`
	Recursive   *bool  `json:"recursive,omitempty"`
	IncludeDirs bool   `json:"includeDirs,omitempty"`
	MaxEntries  int    `json:"maxEntries,omitempty"`
}

type TreeModeKind string

const (
	TreeModeKindTree      TreeModeKind = "tree"
	TreeModeKindSymlink   TreeModeKind = "symlink"
	TreeModeKindSubmodule TreeModeKind = "submodule"
	TreeModeKindBlob      TreeModeKind = "blob"
)

type TreeEntryInfo struct {
	Path string       `json:"path"`
	Kind TreeModeKind `json:"kind"`
	Mode string       `json:"mode"`
	Hash string       `json:"hash"`
	Size int64        `json:"size,omitempty"`
}

type ListTreeOut struct {
	RepoPath    string          `json:"repoPath"`
	Revision    string          `json:"revision"`
	Path        string          `json:"path"`
	Recursive   bool            `json:"recursive"`
	IncludeDirs bool            `json:"includeDirs"`
	MaxEntries  int             `json:"maxEntries"`
	Entries     []TreeEntryInfo `json:"entries"`
	Count       int             `json:"count"`
	Truncated   bool            `json:"truncated"`
}

func listTree(ctx context.Context, snap gitToolSnapshot, args ListTreeArgs) (*ListTreeOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	p := strings.TrimSpace(args.Path)
	if p == "" {
		p = "."
	}
	p, err := normalizeRepoRelativePath(p)
	if err != nil {
		return nil, err
	}
	recursive := true
	if args.Recursive != nil {
		recursive = *args.Recursive
	}
	maxEntries := normalizePositiveInt(args.MaxEntries, defaultTreeMaxCount, 1, hardTreeMaxCount)

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}
	commit, err := resolveCommit(repo, revision)
	if err != nil {
		return nil, err
	}
	root, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	out := &ListTreeOut{
		RepoPath:    abs,
		Revision:    revision,
		Path:        p,
		Recursive:   recursive,
		IncludeDirs: args.IncludeDirs,
		MaxEntries:  maxEntries,
	}

	if p != "." {
		if f, err := root.File(p); err == nil {
			out.Entries = append(out.Entries, TreeEntryInfo{
				Path: p,
				Kind: treeModeKind(f.Mode),
				Mode: f.Mode.String(),
				Hash: f.Hash.String(),
				Size: f.Size,
			})
			out.Count = len(out.Entries)
			return out, nil
		}
	}

	tree := root
	prefix := ""
	if p != "." {
		tree, err = root.Tree(p)
		if err != nil {
			return nil, errors.New("path does not exist as a tree at revision")
		}
		prefix = p + "/"
	}

	if recursive {
		iter := tree.Files()
		defer iter.Close()
		if err := iter.ForEach(func(f *object.File) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := f.Name
			if prefix != "" && !strings.HasPrefix(name, prefix) {
				name = prefix + name
			}
			out.Entries = append(out.Entries, TreeEntryInfo{
				Path: name,
				Kind: treeModeKind(f.Mode),
				Mode: f.Mode.String(),
				Hash: f.Hash.String(),
				Size: f.Size,
			})
			if len(out.Entries) >= maxEntries {
				out.Truncated = true
				return errStopIteration
			}
			return nil
		}); err != nil && !errors.Is(err, errStopIteration) {
			return nil, err
		}
	} else {
		for _, entry := range tree.Entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			kind := treeModeKind(entry.Mode)
			if kind == "tree" && !args.IncludeDirs {
				continue
			}
			out.Entries = append(out.Entries, TreeEntryInfo{
				Path: prefix + entry.Name,
				Kind: kind,
				Mode: entry.Mode.String(),
				Hash: entry.Hash.String(),
			})
			if len(out.Entries) >= maxEntries {
				out.Truncated = true
				break
			}
		}
	}

	sort.Slice(out.Entries, func(i, j int) bool {
		return out.Entries[i].Path < out.Entries[j].Path
	})
	out.Count = len(out.Entries)
	return out, nil
}

func treeModeKind(mode filemode.FileMode) TreeModeKind {
	switch mode {
	case filemode.Dir:
		return TreeModeKindTree
	case filemode.Symlink:
		return TreeModeKindSymlink
	case filemode.Submodule:
		return TreeModeKindSubmodule
	default:
		return TreeModeKindBlob
	}
}

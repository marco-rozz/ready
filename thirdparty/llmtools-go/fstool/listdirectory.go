package fstool

import (
	"context"
	"fmt"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const listDirectoryFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/listdirectory.ListDirectory"

var listDirectoryTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "018fe0f4-b8cd-7e55-82d5-9df0bd70e4bb",
	Slug:          "listdirectory",
	Version:       spec.VersionOne,
	DisplayName:   "List directory",
	Description:   "List immediate entries in a directory. Dot-prefixed files and directories are excluded by default. You can optionally filter by basename glob and entry kind.",
	Tags:          []string{"fs", "list"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Directory to list.",
		"default": "."
	},
	"nameGlob": {
		"type": "string",
		"description": "Optional glob applied to immediate entry names, like \"*.txt\"."
	},
	"includeDotEntries": {
		"type": "boolean",
		"description": "Include entries whose names start with '.'.",
		"default": false
	},
	"kind": {
		"type": "string",
		"enum": ["all", "file", "directory", "other"],
		"description": "Optional entry-kind filter.",
		"default": "all"
	},
	"maxEntries": {
		"type": "integer",
		"description": "Maximum entries to return after filtering.",
		"default": 200,
		"minimum": 1,
		"maximum": 5000
	}
},
"required": [],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: listDirectoryFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ListDirectoryEntryKind string

const (
	ListDirectoryEntryKindAll       ListDirectoryEntryKind = "all"
	ListDirectoryEntryKindFile      ListDirectoryEntryKind = "file"
	ListDirectoryEntryKindDirectory ListDirectoryEntryKind = "directory"
	ListDirectoryEntryKindOther     ListDirectoryEntryKind = "other"
)

type ListDirectoryArgs struct {
	Path              string                 `json:"path,omitempty"`
	NameGlob          string                 `json:"nameGlob,omitempty"`
	IncludeDotEntries bool                   `json:"includeDotEntries,omitempty"`
	Kind              ListDirectoryEntryKind `json:"kind,omitempty"`
	MaxEntries        int                    `json:"maxEntries,omitempty"`
}

type ListDirectoryEntry struct {
	Name string                 `json:"name"`
	Kind ListDirectoryEntryKind `json:"kind"`
}

type ListDirectoryOut struct {
	Path              string               `json:"path"`
	ReachedMaxEntries bool                 `json:"reachedMaxEntries"`
	Items             []ListDirectoryEntry `json:"items,omitempty"`
}

// listDirectory lists immediate entries in Path, with optional glob and kind filtering.
func listDirectory(
	ctx context.Context,
	args ListDirectoryArgs,
	p fspolicy.FSPolicy,
) (*ListDirectoryOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := p.ResolvePath(args.Path, ".")
	if err != nil {
		return nil, err
	}

	if err := p.VerifyDirResolved(dir); err != nil {
		return nil, err
	}

	nameGlob := args.NameGlob

	maxEntries := args.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultListDirectoryMaxEntries
	}
	if maxEntries > hardListDirectoryMaxEntries {
		return nil, fmt.Errorf("maxEntries must be between 1 and %d", hardListDirectoryMaxEntries)
	}

	items, reachedMaxEntries, err := ioutil.ListDirectoryDetailedNormalized(dir, ioutil.ListDirectoryOptions{
		NameGlob:          nameGlob,
		IncludeDotEntries: args.IncludeDotEntries,
		Kind:              ioutil.ListDirectoryKind(args.Kind),
		MaxEntries:        maxEntries,
	})
	if err != nil {
		return nil, err
	}

	entryNames := make([]string, len(items))
	outItems := make([]ListDirectoryEntry, len(items))
	for i, item := range items {
		entryNames[i] = item.Name
		outItems[i] = ListDirectoryEntry{
			Name: item.Name,
			Kind: ListDirectoryEntryKind(item.Kind),
		}
	}

	return &ListDirectoryOut{
		Path:              dir,
		ReachedMaxEntries: reachedMaxEntries,
		Items:             outItems,
	}, nil
}

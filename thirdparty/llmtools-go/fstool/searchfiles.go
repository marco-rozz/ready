package fstool

import (
	"context"
	"errors"
	"fmt"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const searchFilesFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/searchfiles.SearchFiles"

var searchFilesTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "018fe0f4-b8cd-7e55-82d5-9df0bd70e4bc",
	Slug:          "searchfiles",
	Version:       spec.VersionOne,
	DisplayName:   "Search files (content or path)",
	Description: "Recursively search files by path, file content, or both. " +
		"Dot-prefixed files and directories are excluded by default. Content search only checks small (< 1MB) UTF-8 text-like files.",
	Tags: []string{"fs", "search"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"root": {
		"type": "string",
		"description": "Directory to start searching from.",
		"default": "."
	},
	"query": {
		"type": "string",
		"description": "Search text to match against file paths and/or file content."
	},
	"regexp": {
		"type": "boolean",
		"description": "If true, interpret query as an RE2 regular expression. If false, match literal text.",
		"default": false
	},
	"searchIn": {
		"type": "string",
		"enum": ["path", "content", "pathOrContent"],
		"description": "Where to search for matches.",
		"default": "pathOrContent"
	},
	"nameGlob": {
		"type": "string",
		"description": "Optional basename glob to limit which files are searched, like \"*.go\"."
	},
	"caseSensitive": {
		"type": "boolean",
		"description": "Whether matching is case-sensitive.",
		"default": true
	},
	"includeDotEntries": {
		"type": "boolean",
		"description": "Include files and directories whose names start with '.'",
		"default": false
	},
	"maxResults": {
		"type": "integer",
		"description": "Maximum matches to return.",
		"default": 100,
		"minimum": 1,
		"maximum": 1000
	}
},
"required": ["query"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: searchFilesFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type SearchFilesSearchIn string

const (
	SearchFilesSearchInPath          SearchFilesSearchIn = "path"
	SearchFilesSearchInContent       SearchFilesSearchIn = "content"
	SearchFilesSearchInPathOrContent SearchFilesSearchIn = "pathOrContent"
)

type SearchFilesMatchKind string

const (
	SearchFilesMatchKindPath           SearchFilesMatchKind = "path"
	SearchFilesMatchKindContent        SearchFilesMatchKind = "content"
	SearchFilesMatchKindPathAndContent SearchFilesMatchKind = "pathAndContent"
)

type SearchFilesArgs struct {
	Root              string              `json:"root,omitempty"`
	Query             string              `json:"query,omitempty"`
	Regexp            bool                `json:"regexp,omitempty"`
	SearchIn          SearchFilesSearchIn `json:"searchIn,omitempty"`
	NameGlob          string              `json:"nameGlob,omitempty"`
	CaseSensitive     *bool               `json:"caseSensitive,omitempty"`
	IncludeDotEntries bool                `json:"includeDotEntries,omitempty"`
	MaxResults        int                 `json:"maxResults,omitempty"`
}

type SearchFilesMatch struct {
	Path      string               `json:"path"`
	MatchKind SearchFilesMatchKind `json:"matchKind"`
}

type SearchFilesOut struct {
	Root              string             `json:"root"`
	Items             []SearchFilesMatch `json:"items,omitempty"`
	MatchCount        int                `json:"matchCount"`
	ReachedMaxResults bool               `json:"reachedMaxResults"`
}

// searchFiles walks Root recursively and returns up to MaxResults file matches.
func searchFiles(
	ctx context.Context,
	args SearchFilesArgs,
	p fspolicy.FSPolicy,
) (*SearchFilesOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	query := args.Query
	regexp := args.Regexp

	if query == "" {
		return nil, errors.New("query is required")
	}

	caseSensitive := true
	if args.CaseSensitive != nil {
		caseSensitive = *args.CaseSensitive
	}

	maxResults := args.MaxResults
	if maxResults <= 0 {
		maxResults = defaultSearchFilesMaxResults
	}
	if maxResults > hardSearchFilesMaxResults {
		return nil, fmt.Errorf("maxResults must be between 1 and %d", hardSearchFilesMaxResults)
	}

	searchIn := args.SearchIn
	if searchIn == "" {
		searchIn = SearchFilesSearchInPathOrContent
	}

	items, reachedLimit, err := ioutil.SearchFilesDetailed(ctx, p, ioutil.SearchFilesOptions{
		Root:              args.Root,
		Query:             query,
		Regexp:            regexp,
		SearchIn:          ioutil.SearchFilesSearchIn(searchIn),
		MaxResults:        maxResults,
		MaxContentBytes:   hardSearchContentBytes,
		IncludeDotEntries: args.IncludeDotEntries,
		NameGlob:          args.NameGlob,
		CaseSensitive:     caseSensitive,
	})
	if err != nil {
		return nil, err
	}

	outItems := make([]SearchFilesMatch, 0, len(items))
	for _, item := range items {
		outItems = append(outItems, SearchFilesMatch{
			Path:      item.Path,
			MatchKind: SearchFilesMatchKind(item.MatchKind),
		})
	}

	root := args.Root
	if root == "" {
		root = "."
	}

	return &SearchFilesOut{
		Root:              root,
		Items:             outItems,
		MatchCount:        len(outItems),
		ReachedMaxResults: reachedLimit,
	}, nil
}

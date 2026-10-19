package gittool

import (
	"context"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
)

const showFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/show.Show"

var showTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c005",
	Slug:          "gitshow",
	Version:       spec.VersionOne,
	DisplayName:   "Git show",
	Description:   "Show a local Git commit with metadata and an optional bounded patch.",
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
		"description": "Commit revision to show.",
		"default": "HEAD"
	},
	"includePatch": {
		"type": "boolean",
		"description": "If true, include the commit patch when available.",
		"default": true
	},
	"maxBytes": {
		"type": "integer",
		"description": "Maximum patch bytes returned. Hard capped by the implementation.",
		"default": 1048576,
		"minimum": 64,
		"maximum": 2097152
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: showFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ShowArgs struct {
	RepoPath     string `json:"repoPath"`
	Revision     string `json:"revision,omitempty"`
	IncludePatch *bool  `json:"includePatch,omitempty"`
	MaxBytes     int    `json:"maxBytes,omitempty"`
}

type ShowOut struct {
	RepoPath       string     `json:"repoPath"`
	Revision       string     `json:"revision"`
	Commit         CommitInfo `json:"commit"`
	ParentHashes   []string   `json:"parentHashes,omitempty"`
	Patch          string     `json:"patch,omitempty"`
	PatchBytes     int        `json:"patchBytes,omitempty"`
	PatchTruncated bool       `json:"patchTruncated,omitempty"`
	OutputMeta     OutputMeta `json:"outputMeta,omitzero"`
	Note           string     `json:"note,omitempty"`
}

func show(ctx context.Context, snap gitToolSnapshot, args ShowArgs) (*ShowOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	commit, err := resolveCommit(repo, revision)
	if err != nil {
		return nil, err
	}

	out := &ShowOut{
		RepoPath: abs,
		Revision: revision,
		Commit:   commitInfo(commit),
	}

	for _, h := range commit.ParentHashes {
		out.ParentHashes = append(out.ParentHashes, h.String())
	}

	includePatch := true
	if args.IncludePatch != nil {
		includePatch = *args.IncludePatch
	}
	if !includePatch {
		return out, nil
	}

	if len(commit.ParentHashes) == 0 {
		out.Note = "root commit patch is not emitted by this tool version"
		return out, nil
	}

	parent, err := commit.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	commitTree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	patch, err := parentTree.Patch(commitTree)
	if err != nil {
		return nil, err
	}
	maxBytes := normalizePositiveInt(args.MaxBytes, defaultDiffMaxBytes, 64, hardDiffMaxBytes)

	limited, truncated := limitStringBytes(
		patch.String(),
		maxBytes,
	)
	out.Patch = limited
	out.PatchBytes = len(limited)
	out.PatchTruncated = truncated
	out.OutputMeta = OutputMeta{
		Bytes:     len(limited),
		Truncated: truncated,
		MaxBytes:  maxBytes,
	}

	return out, nil
}

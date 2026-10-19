package gittool

import (
	"context"
	"errors"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const blameFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/blame.Blame"

var blameTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c011",
	Slug:          "gitblame",
	Version:       spec.VersionOne,
	DisplayName:   "Git blame",
	Description:   "Return a bounded best-effort first-parent blame for a repository-relative file path.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"path": {
		"type": "string",
		"description": "Repository-relative file path."
	},
	"revision": {
		"type": "string",
		"description": "Revision to blame.",
		"default": "HEAD"
	},
	"startLine": {
		"type": "integer",
		"description": "1-based start line. Defaults to 1.",
		"default": 1,
		"minimum": 1
	},
	"endLine": {
		"type": "integer",
		"description": "1-based end line. Defaults to the end of file.",
		"minimum": 1
	},
	"maxCommits": {
		"type": "integer",
		"description": "Maximum first-parent commits to walk.",
		"default": 200,
		"minimum": 1,
		"maximum": 1000
	}
},
"required": ["repoPath", "path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: blameFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type BlameArgs struct {
	RepoPath   string `json:"repoPath"`
	Path       string `json:"path"`
	Revision   string `json:"revision,omitempty"`
	StartLine  int    `json:"startLine,omitempty"`
	EndLine    int    `json:"endLine,omitempty"`
	MaxCommits int    `json:"maxCommits,omitempty"`
}

type BlameLine struct {
	LineNumber int        `json:"lineNumber"`
	Text       string     `json:"text"`
	Commit     CommitInfo `json:"commit"`
}

type BlameOut struct {
	RepoPath   string      `json:"repoPath"`
	Path       string      `json:"path"`
	Revision   string      `json:"revision"`
	StartLine  int         `json:"startLine"`
	EndLine    int         `json:"endLine"`
	MaxCommits int         `json:"maxCommits"`
	Lines      []BlameLine `json:"lines"`
	Note       string      `json:"note,omitempty"`
}

func blame(ctx context.Context, snap gitToolSnapshot, args BlameArgs) (*BlameOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}
	p, err := normalizeRepoRelativePath(args.Path)
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

	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	data, exists, err := treePathContent(tree, p, hardBlobReadBytes)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("path does not exist at revision")
	}
	if isBinaryData(data) {
		return nil, errors.New("cannot blame binary file")
	}

	lines := blameSplitLines(string(data))
	if len(lines) == 0 {
		return &BlameOut{
			RepoPath:   abs,
			Path:       p,
			Revision:   revision,
			StartLine:  1,
			EndLine:    0,
			MaxCommits: normalizePositiveInt(args.MaxCommits, 200, 1, 1000),
			Lines:      nil,
			Note:       "best-effort first-parent blame",
		}, nil
	}

	start := args.StartLine
	if start <= 0 {
		start = 1
	}
	end := args.EndLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return nil, errors.New("startLine must be <= endLine")
	}

	maxCommits := normalizePositiveInt(args.MaxCommits, 200, 1, 1000)
	assignments := make([]*object.Commit, len(lines))
	for i := range assignments {
		assignments[i] = commit
	}

	currentCommit := commit
	for range maxCommits {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if currentCommit.NumParents() == 0 {
			break
		}
		parent, err := currentCommit.Parent(0)
		if err != nil {
			break
		}
		parentTree, err := parent.Tree()
		if err != nil {
			break
		}
		parentData, exists, err := treePathContent(parentTree, p, hardBlobReadBytes)
		if err != nil || !exists || isBinaryData(parentData) {
			break
		}
		parentLines := blameSplitLines(string(parentData))
		parentCounts := make(map[string]int, len(parentLines))
		for _, line := range parentLines {
			parentCounts[line]++
		}
		for i, line := range lines {
			if assignments[i] != currentCommit {
				continue
			}
			if parentCounts[line] > 0 {
				parentCounts[line]--
				assignments[i] = parent
			}
		}
		currentCommit = parent
	}

	out := &BlameOut{
		RepoPath:   abs,
		Path:       p,
		Revision:   revision,
		StartLine:  start,
		EndLine:    end,
		MaxCommits: maxCommits,
		Note:       "best-effort first-parent blame; duplicate or moved lines may differ from git CLI blame",
	}
	for lineNo := start; lineNo <= end; lineNo++ {
		c := assignments[lineNo-1]
		if c == nil {
			c = commit
		}
		out.Lines = append(out.Lines, BlameLine{
			LineNumber: lineNo,
			Text:       lines[lineNo-1],
			Commit:     commitInfo(c),
		})
	}
	return out, nil
}

func blameSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

package gittool

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const logFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/log.Log"

var logTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c004",
	Slug:          "gitlog",
	Version:       spec.VersionOne,
	DisplayName:   "Git log",
	Description:   "Return bounded structured commit history for a local Git repository.",
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
		"description": "Revision to start from.",
		"default": "HEAD"
	},
	"maxCount": {
		"type": "integer",
		"description": "Maximum commits to return.",
		"default": 10,
		"minimum": 1,
		"maximum": 100
	},
	"since": {
		"type": "string",
		"description": "Optional lower date bound. Accepts RFC3339, YYYY-MM-DDTHH:MM:SS, or YYYY-MM-DD."
	},
	"until": {
		"type": "string",
		"description": "Optional upper date bound. Accepts RFC3339, YYYY-MM-DDTHH:MM:SS, or YYYY-MM-DD."
	}
},
"required": ["repoPath"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: logFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type LogArgs struct {
	RepoPath string `json:"repoPath"`
	Revision string `json:"revision,omitempty"`
	MaxCount int    `json:"maxCount,omitempty"`
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
}

type LogOut struct {
	RepoPath string       `json:"repoPath"`
	Revision string       `json:"revision"`
	MaxCount int          `json:"maxCount"`
	Commits  []CommitInfo `json:"commits"`
}

func logCommits(ctx context.Context, snap gitToolSnapshot, args LogArgs) (*LogOut, error) {
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
	if err := validateRevision(revision); err != nil {
		return nil, err
	}

	maxCount := normalizePositiveInt(args.MaxCount, defaultLogMaxCount, 1, hardLogMaxCount)
	var since *time.Time

	if strings.TrimSpace(args.Since) != "" {
		since, err = parseTime("since", args.Since)
		if err != nil {
			return nil, err
		}
	}
	var until *time.Time
	if strings.TrimSpace(args.Until) != "" {
		until, err = parseTime("until", args.Until)
		if err != nil {
			return nil, err
		}
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return nil, err
	}

	iter, err := repo.Log(&git.LogOptions{From: *hash})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	out := &LogOut{
		RepoPath: abs,
		Revision: revision,
		MaxCount: maxCount,
		Commits:  make([]CommitInfo, 0, maxCount),
	}

	err = iter.ForEach(func(c *object.Commit) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		when := c.Author.When
		if since != nil && when.Before(*since) {
			return nil
		}
		if until != nil && when.After(*until) {
			return nil
		}
		out.Commits = append(out.Commits, commitInfo(c))
		if len(out.Commits) >= maxCount {
			return errStopIteration
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, err
	}

	return out, nil
}

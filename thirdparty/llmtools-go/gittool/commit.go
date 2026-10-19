package gittool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const commitFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/commit.Commit"

var commitTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c00a",
	Slug:          "gitcommit",
	Version:       spec.VersionOne,
	DisplayName:   "Git commit",
	Description:   "Create a local Git commit from staged changes, optionally staging modified tracked files with all=true.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"message": {
		"type": "string",
		"description": "Commit message."
	},
	"authorName": {
		"type": "string",
		"description": "Optional author name. Defaults to repo config, then GitTool default author."
	},
	"authorEmail": {
		"type": "string",
		"description": "Optional author email. Defaults to repo config, then GitTool default author."
	},
	"all": {
		"type": "boolean",
		"description": "If true, automatically stage modified and deleted tracked files before committing.",
		"default": false
	}
},
"required": ["repoPath", "message"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: commitFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type CommitArgs struct {
	RepoPath    string `json:"repoPath"`
	Message     string `json:"message"`
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
	All         bool   `json:"all,omitempty"`
}

type CommitOut struct {
	RepoPath    string    `json:"repoPath"`
	Hash        string    `json:"hash"`
	ShortHash   string    `json:"shortHash"`
	AuthorName  string    `json:"authorName"`
	AuthorEmail string    `json:"authorEmail"`
	When        time.Time `json:"when"`
	All         bool      `json:"all"`
}

func commit(ctx context.Context, snap gitToolSnapshot, args CommitArgs) (*CommitOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	message := strings.TrimSpace(args.Message)
	if message == "" {
		return nil, errors.New("message is required")
	}
	if len([]byte(args.Message)) > hardCommitMessageBytes {
		return nil, fmt.Errorf("message too large: max %d bytes", hardCommitMessageBytes)
	}

	repo, wt, abs, err := openWorktree(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	if err := ensureNoIndexConflicts(repo, wt); err != nil {
		return nil, err
	}

	authorName, authorEmail := resolveAuthor(repo, snap, args.AuthorName, args.AuthorEmail)
	now := time.Now()

	hash, err := wt.Commit(args.Message, &git.CommitOptions{
		All: args.All,
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  now,
		},
		Committer: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  now,
		},
	})
	if err != nil {
		return nil, err
	}

	hashStr := hash.String()
	short := hashStr
	if len(short) > 12 {
		short = short[:12]
	}

	return &CommitOut{
		RepoPath:    abs,
		Hash:        hashStr,
		ShortHash:   short,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		When:        now,
		All:         args.All,
	}, nil
}

func resolveAuthor(repo *git.Repository, snap gitToolSnapshot, argName, argEmail string) (name, email string) {
	name = strings.TrimSpace(argName)
	email = strings.TrimSpace(argEmail)

	if name != "" && email != "" {
		return name, email
	}

	if cfg, err := repo.Config(); err == nil && cfg != nil {
		if name == "" {
			name = strings.TrimSpace(cfg.User.Name)
		}
		if email == "" {
			email = strings.TrimSpace(cfg.User.Email)
		}
	}

	if name == "" {
		name = strings.TrimSpace(snap.defaultAuthorName)
	}
	if email == "" {
		email = strings.TrimSpace(snap.defaultAuthorEmail)
	}
	if name == "" {
		name = "llmtools git tool"
	}
	if email == "" {
		email = "llmtools-git@example.invalid"
	}
	return name, email
}

package gittool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const createTagFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/createtag.CreateTag"

var createTagTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c012",
	Slug:          "gitcreatetag",
	Version:       spec.VersionOne,
	DisplayName:   "Git create tag",
	Description:   "Create a local lightweight or annotated Git tag. If message is non-empty, an annotated tag is created; otherwise a lightweight tag is created.",
	Tags:          []string{toolTagGit, toolTagWrite},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"name": {
		"type": "string",
		"description": "Tag name to create. Pass a plain tag name, not refs/tags/name."
	},
	"target": {
		"type": "string",
		"description": "Revision to tag.",
		"default": "HEAD"
	},
	"message": {
		"type": "string",
		"description": "Optional tag message. If non-empty, creates an annotated tag. If omitted or empty, creates a lightweight tag."
	},
	"taggerName": {
		"type": "string",
		"description": "Optional tagger name for annotated tags. Defaults to repo config, then GitTool default author."
	},
	"taggerEmail": {
		"type": "string",
		"description": "Optional tagger email for annotated tags. Defaults to repo config, then GitTool default author."
	}
},
"required": ["repoPath", "name"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: createTagFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type CreateTagArgs struct {
	RepoPath    string `json:"repoPath"`
	Name        string `json:"name"`
	Target      string `json:"target,omitempty"`
	Message     string `json:"message,omitempty"`
	TaggerName  string `json:"taggerName,omitempty"`
	TaggerEmail string `json:"taggerEmail,omitempty"`
}

type CreateTagOut struct {
	RepoPath        string    `json:"repoPath"`
	Name            string    `json:"name"`
	Target          string    `json:"target"`
	TargetHash      string    `json:"targetHash"`
	TargetShortHash string    `json:"targetShortHash"`
	TagHash         string    `json:"tagHash"`
	Annotated       bool      `json:"annotated"`
	Message         string    `json:"message,omitempty"`
	TaggerName      string    `json:"taggerName,omitempty"`
	TaggerEmail     string    `json:"taggerEmail,omitempty"`
	When            time.Time `json:"when,omitzero"`
	Action          string    `json:"action"`
}

func createTag(ctx context.Context, snap gitToolSnapshot, args CreateTagArgs) (*CreateTagOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(args.Name)
	if err := validateTagName(name); err != nil {
		return nil, err
	}

	target := strings.TrimSpace(args.Target)
	if target == "" {
		target = revisionHead
	}
	if err := validateRevision(target); err != nil {
		return nil, err
	}

	message := strings.TrimSpace(args.Message)
	if len([]byte(args.Message)) > hardTagMessageBytes {
		return nil, fmt.Errorf("tag message too large: max %d bytes", hardTagMessageBytes)
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	hash, err := repo.ResolveRevision(plumbing.Revision(target))
	if err != nil {
		return nil, fmt.Errorf("resolve target %q: %w", target, err)
	}

	tagRefName := plumbing.ReferenceName("refs/tags/" + name)
	if _, err := repo.Reference(tagRefName, false); err == nil {
		return nil, fmt.Errorf("tag already exists: %s", name)
	}

	targetHash := hash.String()
	targetShortHash := targetHash
	if len(targetShortHash) > 12 {
		targetShortHash = targetShortHash[:12]
	}

	out := &CreateTagOut{
		RepoPath:        abs,
		Name:            name,
		Target:          target,
		TargetHash:      targetHash,
		TargetShortHash: targetShortHash,
		Annotated:       message != "",
		Message:         message,
		Action:          "created",
	}

	if message == "" {
		ref := plumbing.NewHashReference(tagRefName, *hash)
		if err := repo.Storer.SetReference(ref); err != nil {
			return nil, err
		}
		out.TagHash = ref.Hash().String()
		return out, nil
	}

	taggerName, taggerEmail := resolveAuthor(repo, snap, args.TaggerName, args.TaggerEmail)
	now := time.Now()

	ref, err := repo.CreateTag(name, *hash, &git.CreateTagOptions{
		Tagger: &object.Signature{
			Name:  taggerName,
			Email: taggerEmail,
			When:  now,
		},
		Message: message,
	})
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, fmt.Errorf("create annotated tag %q: %w", name, err)
		}
		return nil, err
	}

	out.TagHash = ref.Hash().String()
	out.TaggerName = taggerName
	out.TaggerEmail = taggerEmail
	out.When = now

	return out, nil
}

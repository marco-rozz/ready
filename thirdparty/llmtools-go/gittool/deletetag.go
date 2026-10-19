package gittool

import (
	"context"
	"fmt"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing"
)

const deleteTagFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/deletetag.DeleteTag"

var deleteTagTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c013",
	Slug:          "gitdeletetag",
	Version:       spec.VersionOne,
	DisplayName:   "Git delete tag",
	Description:   "Delete a local Git tag ref. This does not contact remotes.",
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
		"description": "Tag name to delete. Pass a plain tag name, not refs/tags/name."
	}
},
"required": ["repoPath", "name"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: deleteTagFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type DeleteTagArgs struct {
	RepoPath string `json:"repoPath"`
	Name     string `json:"name"`
}

type DeleteTagOut struct {
	RepoPath string `json:"repoPath"`
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	Action   string `json:"action"`
}

func deleteTag(ctx context.Context, snap gitToolSnapshot, args DeleteTagArgs) (*DeleteTagOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(args.Name)
	if err := validateTagName(name); err != nil {
		return nil, err
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}

	tagRefName := plumbing.ReferenceName("refs/tags/" + name)
	ref, err := repo.Reference(tagRefName, false)
	if err != nil {
		return nil, fmt.Errorf("tag not found: %s", name)
	}

	if err := repo.Storer.RemoveReference(tagRefName); err != nil {
		return nil, err
	}

	return &DeleteTagOut{
		RepoPath: abs,
		Name:     name,
		Hash:     ref.Hash().String(),
		Action:   "deleted",
	}, nil
}

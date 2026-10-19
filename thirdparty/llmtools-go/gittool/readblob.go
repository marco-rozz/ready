package gittool

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
)

const readBlobFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/readblob.ReadBlob"

var readBlobTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c016",
	Slug:          "gitreadblob",
	Version:       spec.VersionOne,
	DisplayName:   "Git read blob",
	Description:   "Read a repository-relative file at HEAD or another revision without checkout.",
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
		"description": "Repository-relative file path."
	},
	"encoding": {
		"type": "string",
		"enum": ["auto", "text", "base64"],
		"description": "Output encoding. auto emits text for UTF-8 text and omits binary content.",
		"default": "auto"
	},
	"maxBytes": {
		"type": "integer",
		"description": "Maximum blob bytes to read.",
		"default": 1048576,
		"minimum": 1,
		"maximum": 4194304
	}
},
"required": ["repoPath", "path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: readBlobFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type BlobEncoding string

const (
	BlobEncodingAuto   BlobEncoding = "auto"
	BlobEncodingText   BlobEncoding = "text"
	BlobEncodingBase64 BlobEncoding = "base64"
)

type ReadBlobArgs struct {
	RepoPath string       `json:"repoPath"`
	Revision string       `json:"revision,omitempty"`
	Path     string       `json:"path"`
	Encoding BlobEncoding `json:"encoding,omitempty"`
	MaxBytes int          `json:"maxBytes,omitempty"`
}

type ReadBlobOut struct {
	RepoPath       string       `json:"repoPath"`
	Revision       string       `json:"revision"`
	Path           string       `json:"path"`
	Mode           string       `json:"mode"`
	Hash           string       `json:"hash"`
	Size           int64        `json:"size"`
	Encoding       BlobEncoding `json:"encoding"`
	Content        string       `json:"content,omitempty"`
	Bytes          int          `json:"bytes"`
	Truncated      bool         `json:"truncated"`
	MaxBytes       int          `json:"maxBytes"`
	IsBinary       bool         `json:"isBinary"`
	ContentOmitted bool         `json:"contentOmitted"`
	OmissionReason string       `json:"omissionReason,omitempty"`
	OutputMeta     OutputMeta   `json:"outputMeta"`
}

func readBlob(ctx context.Context, snap gitToolSnapshot, args ReadBlobArgs) (*ReadBlobOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	p, err := normalizeRepoRelativePath(args.Path)
	if err != nil {
		return nil, err
	}
	maxBytes := normalizePositiveInt(args.MaxBytes, defaultDiffMaxBytes, 1, hardBlobReadBytes)

	encoding := BlobEncoding(strings.ToLower(strings.TrimSpace(string(args.Encoding))))
	switch encoding {
	case "", BlobEncodingAuto:
		encoding = BlobEncodingAuto
	case BlobEncodingText, BlobEncodingBase64:
	default:
		return nil, errors.New(`encoding must be "auto", "text", or "base64"`)
	}

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}
	commit, err := resolveCommit(repo, revision)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	f, err := tree.File(p)
	if err != nil {
		return nil, errors.New("path does not exist as a file at revision")
	}
	r, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer r.Close()

	data, truncated, err := readLimited(r, int64(maxBytes))
	if err != nil {
		return nil, err
	}
	isBinary := isBinaryData(data)

	out := &ReadBlobOut{
		RepoPath:  abs,
		Revision:  revision,
		Path:      p,
		Mode:      f.Mode.String(),
		Hash:      f.Hash.String(),
		Size:      f.Size,
		Encoding:  encoding,
		Bytes:     len(data),
		Truncated: truncated,
		MaxBytes:  maxBytes,
		IsBinary:  isBinary,
		OutputMeta: OutputMeta{
			Bytes:     len(data),
			Truncated: truncated,
			MaxBytes:  maxBytes,
		},
	}

	switch encoding {
	case BlobEncodingBase64:
		out.Content = base64.StdEncoding.EncodeToString(data)
	case BlobEncodingText:
		if isBinary {
			return nil, errors.New("blob is binary or not valid UTF-8; use encoding=base64")
		}
		out.Content = string(data)
	case BlobEncodingAuto:
		if isBinary {
			out.ContentOmitted = true
			out.OmissionReason = "binary content omitted; use encoding=base64 to read it"
			return out, nil
		}
		out.Encoding = BlobEncodingText
		out.Content = string(data)
	}

	return out, nil
}

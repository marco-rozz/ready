package fstool

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const writeFileFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/writefile.WriteFile"

var writeFileTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04cf-72ec-7eed-ab2a-45e6fb9e1a86",
	Slug:          "writefile",
	Version:       spec.VersionOne,
	DisplayName:   "Write file",
	Description:   "Write a file to disk. Set createParents=true if folders dont exist. Set overwrite=true to replace the file. Text mode writes UTF-8 text. Binary mode expects base64 input and writes raw bytes.",
	Tags:          []string{"fs"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path of the file to write."
	},
	"encoding": {
		"type": "string",
		"enum": ["text", "binary"],
		"description": "Write mode.",
		"default": "text"
	},
	"content": {
		"type": "string",
		"description": "If encoding=text, UTF-8 content. If encoding=binary, base64-encoded bytes."
	},
	"overwrite": {
		"type": "boolean",
		"description": "If false and the file exists, return an error. If true, create the file if needed or replace the existing file.",
		"default": false
	},
	"createParents": {
		"type": "boolean",
		"description": "If true, create missing parent directories. Max new directories created is 8.",
		"default": false
	}
},
"required": ["path", "content"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: writeFileFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type WriteFileArgs struct {
	Path          string `json:"path"`
	Encoding      string `json:"encoding,omitempty"` // "text"(default) | "binary"
	Content       string `json:"content"`
	Overwrite     bool   `json:"overwrite,omitempty"`
	CreateParents bool   `json:"createParents,omitempty"`
}

type WriteFileAction string

const (
	WriteFileActionCreated     WriteFileAction = "created"
	WriteFileActionOverwritten WriteFileAction = "overwritten"
)

type WriteFileOut struct {
	Path         string          `json:"path"`
	BytesWritten int64           `json:"bytesWritten"`
	Action       WriteFileAction `json:"action"`
}

func writeFile(
	ctx context.Context,
	args WriteFileArgs,
	p fspolicy.FSPolicy,
) (*WriteFileOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	enc := ioutil.ReadEncoding(strings.ToLower(strings.TrimSpace(args.Encoding)))
	if enc == "" {
		enc = ioutil.ReadEncodingText
	}
	if enc != ioutil.ReadEncodingText && enc != ioutil.ReadEncodingBinary {
		return nil, errors.New(`encoding must be "text" or "binary"`)
	}

	// Decode/validate content.
	var data []byte
	switch enc {
	case ioutil.ReadEncodingText:
		// Content is required by schema, but empty string is a valid payload.
		if !utf8.ValidString(args.Content) {
			return nil, errors.New("content is not valid UTF-8")
		}
		data = []byte(args.Content)
	case ioutil.ReadEncodingBinary:
		b64 := strings.TrimSpace(args.Content)
		// Pre-check decoded size to avoid huge allocations.
		if int64(base64.StdEncoding.DecodedLen(len(b64))) > hardFileWriteBytes {
			return nil, fmt.Errorf("content too large (decoded > %d bytes)", hardFileWriteBytes)
		}
		decoded, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			return nil, fmt.Errorf("invalid base64 content: %w", derr)
		}
		data = decoded
	}

	if int64(len(data)) > hardFileWriteBytes {
		return nil, fmt.Errorf("content too large (%d bytes; max %d)", len(data), hardFileWriteBytes)
	}

	action := WriteFileActionCreated
	if resolvedDst, err := p.ResolvePath(args.Path, ""); err != nil {
		return nil, err
	} else if _, statErr := os.Lstat(resolvedDst); statErr == nil {
		action = WriteFileActionOverwritten
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}

	dst, err := ioutil.WriteFileAtomicBytesWithParents(
		p,
		args.Path,
		data,
		0o600,
		args.Overwrite,
		args.CreateParents,
		hardWriteFileParentCreations,
	)
	if err != nil {
		if !args.Overwrite && errors.Is(err, os.ErrExist) {
			if dst == "" {
				dst = args.Path
			}
			return nil, fmt.Errorf("file already exists and overwrite=false: %s", dst)
		}
		return nil, err
	}
	return &WriteFileOut{
		Path:         dst,
		BytesWritten: int64(len(data)),
		Action:       action,
	}, nil
}

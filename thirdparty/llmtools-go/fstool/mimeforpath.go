package fstool

import (
	"context"
	"path/filepath"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const mimeForPathFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/mimeforpath.MIMEForPath"

var mimeForPathTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019bf910-2645-7965-bd9e-006831eabbc3",
	Slug:          "mimeforpath",
	Version:       spec.VersionOne,
	DisplayName:   "Detect MIME for path",
	Description:   "Best-effort MIME type detection for a filesystem path. Uses the extension if reliable; otherwise sniffs the file bytes.",
	Tags:          []string{"fs", "mime"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path of the file to check."
	}
},
"required": ["path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: mimeForPathFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type MIMEDetectMethod string

const (
	MIMEDetectMethodExtension MIMEDetectMethod = "extension"
	MIMEDetectMethodSniff     MIMEDetectMethod = "sniff"
)

type MIMEForPathArgs struct {
	Path string `json:"path"`
}

type MIMEForPathOut struct {
	Path                string           `json:"path"`
	Extension           string           `json:"extension,omitempty"`
	NormalizedExtension string           `json:"normalizedExtension,omitempty"`
	MIMEType            string           `json:"mimeType"`
	BaseMIMEType        string           `json:"baseMIMEType"`
	Mode                MIMEMode         `json:"mode"`
	Method              MIMEDetectMethod `json:"method"`
}

// mimeForPath returns a best-effort MIME type for a filesystem path.
//
// Notes:
// - If the extension maps to a non-generic MIME type, it may succeed even if the file does not exist.
// - If the extension is unknown/generic, it tries to open and sniff the file (can error if unreadable).
func mimeForPath(
	ctx context.Context,
	args MIMEForPathArgs,
	p fspolicy.FSPolicy,
) (*MIMEForPathOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, mt, mode, method, err := ioutil.MIMEForPath(p, args.Path, hardMIMESniffBytes)
	if err != nil {
		return nil, err
	}

	ext := filepath.Ext(abs)
	normExt := ioutil.GetNormalizedExt(ext)

	return &MIMEForPathOut{
		Path:                abs,
		Extension:           ext,
		NormalizedExtension: string(normExt),
		MIMEType:            string(mt),
		BaseMIMEType:        ioutil.GetBaseMIME(mt),
		Mode:                MIMEMode(mode),
		Method:              MIMEDetectMethod(method),
	}, nil
}

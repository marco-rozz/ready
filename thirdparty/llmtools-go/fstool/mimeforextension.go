package fstool

import (
	"context"
	"errors"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const mimeForExtensionFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/mimeforextension.MIMEForExtension"

var mimeForExtensionTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019bf911-3dca-73df-9b1c-4f5e7254a89e",
	Slug:          "mimeforextension",
	Version:       spec.VersionOne,
	DisplayName:   "Detect MIME for extension",
	Description:   "Return the best-known MIME type for an extension (accepts both 'png' and '.png'). Falls back to application/octet-stream when unknown.",
	Tags:          []string{"mime"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"extension": {
		"type": "string",
		"description": "File extension (e.g. 'png' or '.png')."
	}
},
"required": ["extension"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: mimeForExtensionFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type MIMEMode string

const (
	MIMEModeText     MIMEMode = "text"
	MIMEModeImage    MIMEMode = "image"
	MIMEModeDocument MIMEMode = "document"
	MIMEModeDefault  MIMEMode = "default"
)

type MIMEForExtensionArgs struct {
	Extension string `json:"extension"`
}

type MIMEForExtensionOut struct {
	Extension           string   `json:"extension"`
	NormalizedExtension string   `json:"normalizedExtension"`
	MIMEType            string   `json:"mimeType"`
	BaseMIMEType        string   `json:"baseMIMEType"`
	Mode                MIMEMode `json:"mode"`
	Known               bool     `json:"known"`
}

// mimeForExtension returns the best-known MIME type for an extension.
//
// If the extension is unknown, this tool returns:
// - mimeType: "application/octet-stream"
// - known: false
// and does NOT error (so calling code can continue).
func mimeForExtension(
	ctx context.Context,
	args MIMEForExtensionArgs,
	_ fspolicy.FSPolicy,
) (*MIMEForExtensionOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	extIn := strings.TrimSpace(args.Extension)
	if extIn == "" {
		return nil, ioutil.ErrInvalidPath
	}

	mt, err := ioutil.MIMEFromExtensionString(extIn)

	known := err == nil
	if errors.Is(err, ioutil.ErrUnknownExtension) {
		// Unknown extension is not fatal for this tool.
		known = false
		err = nil
	}
	if err != nil {
		return nil, err
	}

	normExt := ioutil.GetNormalizedExt(extIn)
	base := ioutil.GetBaseMIME(mt)

	return &MIMEForExtensionOut{
		Extension:           extIn,
		NormalizedExtension: string(normExt),
		MIMEType:            string(mt),
		BaseMIMEType:        base,
		Mode:                MIMEMode(ioutil.GetModeForMIME(mt)),
		Known:               known,
	}, nil
}

package imagetool

import (
	"context"
	"time"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const readImageFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/imagetool/readimage.ReadImage"

var readImageTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019bf894-a7e4-7f19-8e7d-8e2297f4f799",
	Slug:          "readimage",
	Version:       spec.VersionOne,
	DisplayName:   "Read image",
	Description:   "Read intrinsic metadata for a local image file, optionally including base64-encoded contents.",
	Tags:          []string{"image", "file"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path of the image to read."
	},
	"includeBase64Data": {
		"type": "boolean",
		"description": "If true, include the base64-encoded file contents in the output.",
		"default": false
	}
},
"required": ["path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: readImageFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ReadImageArgs struct {
	Path              string `json:"path"`
	IncludeBase64Data bool   `json:"includeBase64Data"`
}

type ReadImageOut struct {
	Path      string     `json:"path"`
	Name      string     `json:"name"`
	Exists    bool       `json:"exists"`
	SizeBytes int64      `json:"sizeBytes,omitempty"`
	ModTime   *time.Time `json:"modTime,omitempty"`

	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Format   string `json:"format,omitempty"`   // "png", "jpeg", ...
	MIMEType string `json:"mimeType,omitempty"` // "image/png", ...

	// Optional content.
	Base64Data string `json:"base64Data,omitempty"`
}

// readImage reads intrinsic metadata for a local image file, optionally including base64-encoded contents.
// Semantics:
//   - empty path => error
//   - directory path => error
//   - non-image/unsupported image => error
//   - non-existent path => (Exists=false, err=nil).
func readImage(
	ctx context.Context,
	args ReadImageArgs,
	p fspolicy.FSPolicy,
) (*ReadImageOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := ioutil.ReadImage(p, args.Path, args.IncludeBase64Data, hardImageReadBytes)
	if err != nil {
		return nil, err
	}

	out := &ReadImageOut{
		Path:      info.Path,
		Name:      info.Name,
		Exists:    info.Exists,
		SizeBytes: info.Size,
		ModTime:   info.ModTime,

		Width:    info.Width,
		Height:   info.Height,
		Format:   info.Format,
		MIMEType: string(info.MIMEType),

		Base64Data: info.Base64Data,
	}
	return out, nil
}

package fstool

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/pdfutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const readFileFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/fstool/readfile.ReadFile"

var readFileTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "018fe0f4-b8cd-7e55-82d5-9df0bd70e4ba",
	Slug:          "readfile",
	Version:       spec.VersionOne,
	DisplayName:   "Read file",
	Description:   "Read a local file. Text mode is for UTF-8 text-like files and PDFs (extracted content). Binary mode returns file or image output with base64 data instead of plain text.",
	Tags:          []string{"fs", "read"},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"description": "Path of the file to read."
	},
	"encoding": {
		"type": "string",
		"enum": ["text", "binary"],
		"description": "Return mode. Use \"text\" for UTF-8 text-like files and PDFs (extracted text is returned). Use \"binary\" for non-text files; binary mode returns file/image output with base64 data, not plain text.",
		"default": "text"
	}
},
"required": ["path"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: readFileFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ReadFileArgs struct {
	Path     string `json:"path"`               // required
	Encoding string `json:"encoding,omitempty"` // "text" (default) | "binary"
}

// readFile reads a file from disk and returns its contents.
// If Encoding == "binary" the output is base64-encoded.
func readFile(
	ctx context.Context,
	args ReadFileArgs,
	p fspolicy.FSPolicy,
) ([]spec.ToolOutputUnion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Normalize and validate encoding.
	enc := ioutil.ReadEncoding(strings.ToLower(strings.TrimSpace(args.Encoding)))
	if enc == "" {
		enc = ioutil.ReadEncodingText
	}
	if enc != ioutil.ReadEncodingText && enc != ioutil.ReadEncodingBinary {
		return nil, errors.New(`encoding must be "text" or "binary"`)
	}

	abs, err := p.ResolvePath(args.Path, "")
	if err != nil {
		return nil, err
	}

	_, err = p.RequireExistingRegularFileResolved(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("path does not exist: %s", abs)
		}
		return nil, err
	}

	// Detect MIME / extension where possible.
	mimeType, extMode, _, mimeErr := ioutil.MIMEForLocalFile(abs, hardMIMESniffBytes)
	ext := strings.ToLower(filepath.Ext(abs))

	isPDFByExt := ext == string(ioutil.ExtPDF)
	isPDFByMime := mimeErr == nil && mimeType == ioutil.MIMEApplicationPDF
	isPDF := isPDFByExt || isPDFByMime

	if enc == ioutil.ReadEncodingText {
		// For non-PDFs, fail if MIME detection fails (conservative).
		// For PDFs, allow text extraction even if MIME sniffing fails,
		// as long as the extension is .pdf.
		if !isPDF && mimeErr != nil {
			return nil, fmt.Errorf("cannot read %q as text (MIME detection failed: %w)", abs, mimeErr)
		}

		if isPDF {
			// PDF: use the same extraction logic as attachments.
			// Extraction output is limited by this tool's hard read limit.
			text, err := pdfutil.ExtractPDFTextSafe(ctx, abs, int(hardFileReadBytes))
			if err != nil {
				return nil, err
			}

			return []spec.ToolOutputUnion{
				{
					Kind: spec.ToolOutputKindText,
					TextItem: &spec.ToolOutputText{
						Text: text,
					},
				},
			}, nil
		}

		// Non‑PDF: only allow clearly text-like files.
		if extMode != ioutil.ExtensionModeText {
			return nil, fmt.Errorf(
				"cannot read non-text file %q as text; use encoding \"binary\" instead",
				abs,
			)
		}

		// Normal text file: read and validate UTF‑8.
		data, err := ioutil.ReadFile(abs, ioutil.ReadEncodingText, hardFileReadBytes)
		if err != nil {
			return nil, err
		}
		if !utf8.ValidString(data) {
			return nil, fmt.Errorf(
				"file %q is not valid UTF-8 text; use encoding \"binary\" instead",
				abs,
			)
		}

		return []spec.ToolOutputUnion{
			{
				Kind: spec.ToolOutputKindText,
				TextItem: &spec.ToolOutputText{
					Text: data,
				},
			},
		}, nil
	}

	// Binary mode: base64-encode and return, like before.
	data, err := ioutil.ReadFile(abs, ioutil.ReadEncodingBinary, hardFileReadBytes)
	if err != nil {
		return nil, err
	}

	baseName := filepath.Base(abs)

	if baseName == "" {
		baseName = "file"
	}

	// Prefer MIMEForLocalFile result if available; otherwise fall back to extension mapping.
	var mt string
	if mimeErr == nil && mimeType != "" {
		mt = string(mimeType)
	} else {
		if ext == "" {
			ext = strings.ToLower(filepath.Ext(baseName))
		}
		mt = mime.TypeByExtension(ext)
	}
	if mt == "" {
		mt = string(ioutil.MIMEApplicationOctetStream)
	}

	if strings.HasPrefix(mt, "image/") {
		return []spec.ToolOutputUnion{
			{
				Kind: spec.ToolOutputKindImage,
				ImageItem: &spec.ToolOutputImage{
					Detail:    spec.ImageDetailAuto,
					ImageName: baseName,
					ImageMIME: mt,
					ImageData: data, // base64-encoded
				},
			},
		}, nil
	}

	return []spec.ToolOutputUnion{
		{
			Kind: spec.ToolOutputKindFile,
			FileItem: &spec.ToolOutputFile{
				FileName: baseName,
				FileMIME: mt,
				FileData: data, // base64-encoded
			},
		},
	}, nil
}

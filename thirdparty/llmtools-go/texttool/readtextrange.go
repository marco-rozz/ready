package texttool

import (
	"context"
	"errors"
	"fmt"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const readTextRangeFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/readtextrange.ReadTextRange"

var readTextRangeTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c0973-ec5d-7dad-85b2-8048e02deaab",
	Slug:          "readtextrange",
	Version:       spec.VersionOne,
	DisplayName:   "Read text range",
	Description:   "Read a contiguous range of lines from a UTF-8 text file and return them with line numbers.",
	Tags:          []string{toolTagText},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
  "path": {
    "type": "string",
    "minLength": 1,
    "description": "Path of the UTF-8 text file."
  },
  "startLine": {
    "type": "integer",
    "minimum": 1,
    "default": 1,
    "description": "Optional 1-based line number to start reading from. Defaults to 1."
  },
  "lineCount": {
    "type": "integer",
    "minimum": 1,
    "maximum": 16000,
    "default": 1000,
    "description": "Optional number of lines to return. Defaults to 1000. Maximum 16000. If the file ends sooner, fewer lines are returned and eofReached will be true."
  }
},
"required": ["path"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: readTextRangeFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ReadTextRangeArgs struct {
	Path string `json:"path"`

	// Optional 1-based start line. Defaults to 1.
	StartLine *int `json:"startLine,omitempty"`

	// Optional number of lines to return. Defaults to defaultReadTextRangeLineCount.
	LineCount *int `json:"lineCount,omitempty"`
}

type ReadTextRangeLine struct {
	LineNumber int    `json:"lineNumber"` // 1-based
	Text       string `json:"text"`       // original file line (not trimmed)
}

type ReadTextRangeOut struct {
	StartLine     int                 `json:"startLine,omitempty"` // 1-based; zero for empty output
	EndLine       int                 `json:"endLine,omitempty"`   // 1-based; zero for empty output
	LinesReturned int                 `json:"linesReturned"`
	Lines         []ReadTextRangeLine `json:"lines"`
	EOFReached    bool                `json:"eofReached"` // true if returned range reaches EOF
}

// readTextRange reads a contiguous range of lines from a UTF-8 file.
//
// Behavior notes:
//   - File must exist, be regular, not a symlink, and valid UTF-8.
//   - No text matching is performed.
//   - startLine is 1-based and defaults to 1.
//   - lineCount defaults to 1000 and may not exceed 16000.
//   - If the file ends before lineCount lines are available, the tool returns through EOF.
//   - eofReached is true when the returned range includes the end of the file.
//   - Empty file returns zero lines with eofReached=true; startLine may be omitted or set to 1.
func readTextRange(
	ctx context.Context,
	args ReadTextRangeArgs,
	p fspolicy.FSPolicy,
) (*ReadTextRangeOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if args.Path == "" {
		return nil, errors.New("path is required")
	}
	startLine := 1
	if args.StartLine != nil {
		startLine = *args.StartLine
	}
	if startLine < 1 {
		return nil, fmt.Errorf("startLine must be >= 1 (got %d)", startLine)
	}

	lineCount := defaultReadTextRangeLineCount
	if args.LineCount != nil {
		lineCount = *args.LineCount
	}
	if lineCount < 1 {
		return nil, fmt.Errorf("lineCount must be >= 1 (got %d)", lineCount)
	}
	if lineCount > hardReadTextRangeOutputLines {
		return nil, fmt.Errorf("lineCount too large: %d (max %d)", lineCount, hardReadTextRangeOutputLines)
	}

	tf, err := ioutil.ReadTextFileUTF8(p, args.Path, hardTextProcessingBytes)
	if err != nil {
		return nil, err
	}

	total := len(tf.Lines)
	if total == 0 {
		if startLine > 1 {
			return nil, fmt.Errorf("startLine %d is out of bounds for empty file", startLine)
		}
		return &ReadTextRangeOut{
			Lines:         nil,
			LinesReturned: 0,
			EOFReached:    true,
		}, nil
	}

	if startLine > total {
		return nil, fmt.Errorf("startLine %d is out of bounds for file with %d lines", startLine, total)
	}

	selStart := startLine - 1
	selEnd := selStart + lineCount - 1
	if selEnd >= total {
		selEnd = total - 1
	}

	eofReached := selEnd == total-1

	outLines := make([]ReadTextRangeLine, 0, selEnd-selStart+1)
	for i := selStart; i <= selEnd; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outLines = append(outLines, ReadTextRangeLine{
			LineNumber: i + 1,
			Text:       tf.Lines[i],
		})
	}

	return &ReadTextRangeOut{
		StartLine:     selStart + 1,
		EndLine:       selEnd + 1,
		LinesReturned: len(outLines),
		Lines:         outLines,
		EOFReached:    eofReached,
	}, nil
}

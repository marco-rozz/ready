package texttool

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const (
	insertTextFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/inserttext.InsertText"
	whereBetween     string      = "between"
	whereEnd         string      = "end"
	whereStart       string      = "start"
)

var insertTextTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04d3-572e-7d26-b4ca-f37feb7e8368",
	Slug:          "inserttext",
	Version:       spec.VersionOne,
	DisplayName:   "Insert text",
	Description: "Insert a text block into a UTF-8 text file at the start, end, or between copied surrounding text. " +
		`Between mode requires textAbove or textBelow. When both are provided, only blank or whitespace-only lines may lie between them and insertion happens at the start of that blank gap`,

	Tags: []string{toolTagText},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"path": {
		"type": "string",
		"minLength": 1,
		"description": "Path of the file."
	},
	"text": {
		"type": "string",
		"description": "Text block to insert. Newline characters are treated as line breaks."
	},
	"position": {
		"type": "string",
		"enum": ["start", "end", "between"],
		"description": "Where to insert the text block"
	},
	"textAbove": {
		"type": "string",
		"description": "Text above the insertion point"
	},
	"textBelow": {
		"type": "string",
		"description": "Text below the insertion point"
	},
	"lineHint": {
		"type": "integer",
		"minimum": 1,
		"description": "Optional 1-based hint for the line where inserted text should begin, taken from read/find output. If several locations still match, the tool only succeeds when one nearby location is uniquely preferred within a small built-in tolerance"
	}
},
"required": ["path", "text", "position"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: insertTextFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type InsertTextArgs struct {
	Path      string  `json:"path"`
	Text      *string `json:"text"`
	Position  string  `json:"position"`
	TextAbove *string `json:"textAbove,omitempty"`
	TextBelow *string `json:"textBelow,omitempty"`
	LineHint  *int    `json:"lineHint,omitempty"`
}

type InsertTextOut struct {
	InsertedAtLine         int  `json:"insertedAtLine"` // 1-based, where insertion begins
	InsertedLineCount      int  `json:"insertedLineCount"`
	TextAboveMatchedAtLine *int `json:"textAboveMatchedAtLine,omitempty"` // 1-based start line of the matched textAbove boundary block used in the original file
	TextBelowMatchedAtLine *int `json:"textBelowMatchedAtLine,omitempty"` // 1-based start line of the matched textBelow boundary block used in the original file
}

// insertText inserts Text into a UTF‑8 file.
// Behavior notes (entry point):
//
//   - File must exist, be regular, not a symlink, and valid UTF‑8.
//   - Matching is line-wise using strings.TrimSpace.
//   - For position="between": at least one of textAbove/textBelow is required.
//   - If both textAbove and textBelow are provided, only blank/whitespace-only lines may lie between them
//     and insertion occurs at the start of that blank gap.
//   - The resulting insertion location must match exactly once; otherwise it fails.
//   - Writes are atomic and preserve newline style and final newline presence.
func insertText(
	ctx context.Context,
	args InsertTextArgs,
	p fspolicy.FSPolicy,
) (*InsertTextOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if args.Path == "" {
		return nil, errors.New("path is required")
	}

	pos := strings.TrimSpace(strings.ToLower(args.Position))
	if pos == "" {
		return nil, errors.New("position is required")
	}

	textToInsert, err := normalizeRequiredTextBlockArg("text", args.Text)
	if err != nil {
		return nil, err
	}
	textAbove, err := normalizeOptionalTextBlockArg("textAbove", args.TextAbove)
	if err != nil {
		return nil, err
	}
	textBelow, err := normalizeOptionalTextBlockArg("textBelow", args.TextBelow)
	if err != nil {
		return nil, err
	}
	switch pos {
	case whereStart, whereEnd:
		if args.TextAbove != nil || args.TextBelow != nil || args.LineHint != nil {
			return nil, errors.New(`textAbove/textBelow/lineHint must be omitted when position is "start" or "end"`)
		}
	case whereBetween:
		if args.TextAbove == nil && args.TextBelow == nil {
			return nil, errors.New(`position="between" requires textAbove or textBelow`)
		}
		if args.LineHint != nil && *args.LineHint < 1 {
			return nil, errors.New("lineHint must be >= 1")
		}
	default:
		return nil, fmt.Errorf(
			`invalid position value %q (expected: "start","end","between")`,
			pos,
		)
	}

	tf, err := ioutil.ReadTextFileUTF8(p, args.Path, hardTextProcessingBytes)
	if err != nil {
		return nil, err
	}
	originalContent := tf.Render()

	insertAt, textAboveAt, textBelowAt, err := computeInsertIndex(
		tf.Lines,
		pos,
		textAbove,
		textBelow,
		args.LineHint,
	)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tf.Lines = ioutil.ReplaceStringRange(tf.Lines, insertAt, insertAt, textToInsert)

	outStr := tf.Render()
	if err := ioutil.WriteRenderedTextFileIfUnchanged(
		p,
		tf,
		originalContent,
		outStr,
		hardTextProcessingBytes,
	); err != nil {
		return nil, err
	}

	return &InsertTextOut{
		InsertedAtLine:         insertAt + 1,
		InsertedLineCount:      len(textToInsert),
		TextAboveMatchedAtLine: textAboveAt,
		TextBelowMatchedAtLine: textBelowAt,
	}, nil
}

func computeInsertIndex(
	lines []string,
	pos string,
	textAbove []string,
	textBelow []string,
	lineHint *int,
) (insertAt int, textAboveMatchedAtLine, textBelowMatchedAtLine *int, err error) {
	switch pos {
	case whereStart:
		return 0, nil, nil, nil
	case whereEnd:
		return len(lines), nil, nil, nil
	case whereBetween:
		if len(textAbove) == 0 && len(textBelow) == 0 {
			return 0, nil, nil, errors.New(`position="between" requires textAbove or textBelow`)
		}

		matches := ioutil.FindTrimmedInsertionPointMatchCandidates(lines, textAbove, textBelow)
		idxs := ioutil.InsertionPointMatchIndices(matches)
		idxs, hintDiag := ioutil.NarrowIndicesByLineHint(idxs, lineHint, maybeStartLineTolerance)

		if len(idxs) != 1 {
			diag := ioutil.BuildInsertionPointDiagnosticJSON(
				lines,
				idxs,
				len(textAbove),
				len(textBelow),
				hintDiag,
				maxAmbiguityDiagnosticCandidates,
				ambiguityDiagnosticContextLines,
			)

			if len(idxs) == 0 {
				return 0, nil, nil, fmt.Errorf(
					"no insertion point matched the provided textAbove/textBelow. diagnostics=%s suggestion=copy exact text immediately above and/or below the intended insertion point, include more surrounding lines, remember that when both are provided the tool only skips blank or whitespace-only lines between them, and if you know roughly where the text should be inserted set lineHint near that line",
					diag,
				)
			}

			return 0, nil, nil, fmt.Errorf(
				"ambiguous insertion point: found %d candidate locations. diagnostics=%s suggestion=copy exact text immediately above and/or below the intended insertion point, include more surrounding lines, remember that when both are provided the tool only skips blank or whitespace-only lines between them, and if you know roughly where the text should be inserted set lineHint near that line",
				len(idxs),
				diag,
			)
		}

		insertAt = idxs[0]

		matchIdx := -1
		for i := range matches {
			if matches[i].InsertAt == insertAt {
				matchIdx = i
				break
			}
		}
		if matchIdx < 0 {
			return 0, nil, nil, errors.New("internal error resolving insertion point candidate")
		}

		m := matches[matchIdx]
		if m.AboveStart != nil {
			a := *m.AboveStart + 1
			textAboveMatchedAtLine = &a
		}
		if m.BelowStart != nil {
			b := *m.BelowStart + 1
			textBelowMatchedAtLine = &b
		}

		return m.InsertAt, textAboveMatchedAtLine, textBelowMatchedAtLine, nil
	default:
		return 0, nil, nil, fmt.Errorf(
			`invalid position value %q (expected: "start","end","between")`,
			pos,
		)
	}
}

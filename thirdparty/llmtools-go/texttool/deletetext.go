package texttool

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const deleteTextFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/deletetext.DeleteText"

var deleteTextTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04d3-354f-73dc-909c-1b79f73d0f55",
	Slug:          "deletetext",
	Version:       spec.VersionOne,
	DisplayName:   "Delete text",
	Description: "Delete a text block from a UTF-8 text file. Matching compares TrimSpace(line). " +
		"Use textAbove and/or textBelow only to locate the intended occurrence. " +
		"Use lineHint only when expectedCount is 1. " +
		"The tool fails unless the number of matches equals expectedCount.",
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
	"oldText": {
		"type": "string",
		"minLength": 1,
		"description": "Text block to delete."
	},
	"textAbove": {
		"type": "string",
		"minLength": 1,
		"description": "Optional text above oldText used only to locate the occurrence."
	},
	"textBelow": {
		"type": "string",
		"minLength": 1,
		"description": "Optional copied text below oldText used only to locate the occurrence."
	},
	"lineHint": {
		"type": "integer",
		"minimum": 1,
		"description": "Optional 1-based hint for the start line of oldText. Use only when expectedCount is 1."
	},
	"expectedCount": {
		"type": "integer",
		"minimum": 1,
		"default": 1,
		"description": "Fail unless exactly this many matches are found. Defaults to 1."
	}
},
"required": ["path", "oldText"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: deleteTextFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type DeleteTextArgs struct {
	Path string `json:"path"`

	// Required. Nil means omitted. Must not be the empty string.
	OldText *string `json:"oldText"`

	// Optional. Must not be the empty string if provided.
	TextAbove *string `json:"textAbove,omitempty"`

	// Optional. Must not be the empty string if provided.
	TextBelow *string `json:"textBelow,omitempty"`

	// Optional 1-based hint for the start line of oldText.
	// May only be used when ExpectedCount is omitted or 1.
	LineHint *int `json:"lineHint,omitempty"`

	// Default 1.
	ExpectedCount *int `json:"expectedCount,omitempty"`
}

type DeleteTextOut struct {
	DeletionsMade  int   `json:"deletionsMade"`
	DeletedAtLines []int `json:"deletedAtLines"` // 1-based start lines of deleted blocks
}

// deleteText deletes occurrences of oldText from a UTF-8 file.
//
// Behavior notes:
//   - File must exist, be regular, not a symlink, and valid UTF-8.
//   - Matching is line-wise using TrimSpace comparisons.
//   - oldText is the exact block that is deleted.
//   - textAbove/textBelow are locator-only context and are not deleted.
//   - Only blank or whitespace-only lines may lie between that context and the target.
//   - lineHint is a soft 1-based hint and may only be used when expectedCount == 1.
//   - oldText must be provided and must not be the empty string.
//   - Writes are atomic and preserve newline style and final newline presence.
func deleteText(
	ctx context.Context,
	args DeleteTextArgs,
	p fspolicy.FSPolicy,
) (*DeleteTextOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if args.Path == "" {
		return nil, errors.New("path is required")
	}

	oldText, err := normalizeRequiredTextBlockArg("oldText", args.OldText)
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

	expected, err := resolveTextBlockExpectedCount(args.ExpectedCount)
	if err != nil {
		return nil, err
	}
	if err := validateTextBlockLineHint(args.LineHint, expected); err != nil {
		return nil, err
	}

	tf, err := ioutil.ReadTextFileUTF8(p, args.Path, hardTextProcessingBytes)
	if err != nil {
		return nil, err
	}
	originalContent := tf.Render()
	matchIdxs := findTrimmedTextBlockMatchStarts(tf.Lines, textAbove, oldText, textBelow)

	if err := ioutil.EnsureNonOverlappingFixedWidth(matchIdxs, len(oldText)); err != nil {
		return nil, err
	}

	var hintDiag *ioutil.LineHintDiagnostic
	if expected == 1 {
		matchIdxs, hintDiag = ioutil.NarrowIndicesByLineHint(
			matchIdxs,
			args.LineHint,
			textBlockEditLineHintTolerance,
		)
	}

	if len(matchIdxs) != expected {
		suggestion := "copy a longer unique oldText block from the file and add nearby textAbove/textBelow"
		if expected == 1 {
			suggestion += "; if you know roughly where the block starts, set lineHint near the intended start line"
		}
		return nil, fmt.Errorf(
			"delete match count mismatch: expected %d, found %d. diagnostics=%s suggestion=%s",
			expected,
			len(matchIdxs),
			buildTextBlockDiagnosticJSON(
				tf.Lines,
				matchIdxs,
				len(oldText),
				hintDiag,
				textBlockEditMaxAmbiguityDiagnosticCandidates,
				textBlockEditAmbiguityDiagnosticContextLines,
			),
			suggestion,
		)
	}

	for _, v := range slices.Backward(matchIdxs) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := v
		end := start + len(oldText)
		tf.Lines = ioutil.ReplaceStringRange(tf.Lines, start, end, nil)
	}

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

	deletedAt := make([]int, 0, len(matchIdxs))
	for _, idx := range matchIdxs {
		deletedAt = append(deletedAt, idx+1)
	}

	return &DeleteTextOut{
		DeletionsMade:  len(matchIdxs),
		DeletedAtLines: deletedAt,
	}, nil
}

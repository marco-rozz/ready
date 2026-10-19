package texttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const replaceTextFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/replacetext.ReplaceText"

var replaceTextTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04d3-c723-7dfa-b85a-12ee7d328502",
	Slug:          "replacetext",
	Version:       spec.VersionOne,
	DisplayName:   "Replace text",
	Description: "Replace a text block in a UTF-8 text file with new text. Matching compares TrimSpace(line). " +
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
		"description": "Path of file."
	},
	"oldText": {
		"type": "string",
		"minLength": 1,
		"description": "Exact text block to replace."
	},
	"newText": {
		"type": "string",
		"minLength": 1,
		"description": "Replacement text block."
	},
	"textAbove": {
		"type": "string",
		"minLength": 1,
		"description": "Optional text above oldText used only to locate the occurrence."
	},
	"textBelow": {
		"type": "string",
		"minLength": 1,
		"description": "Optional text below oldText used only to locate the occurrence."
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
"required": ["path", "oldText", "newText"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: replaceTextFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type ReplaceTextArgs struct {
	Path string `json:"path"`

	// Required. Nil means omitted. Must not be the empty string.
	OldText *string `json:"oldText"`

	// Required. Nil means omitted. Must not be the empty string.
	NewText *string `json:"newText"`

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

type ReplaceTextOut struct {
	ReplacementsMade int   `json:"replacementsMade"`
	ReplacedAtLines  []int `json:"replacedAtLines"` // 1-based start lines of replaced blocks
}

// replaceText replaces occurrences of oldText in a UTF-8 file.
//
// Behavior notes:
//   - File must exist, be regular, not a symlink, and valid UTF-8.
//   - Matching is line-wise using TrimSpace comparisons.
//   - oldText is the exact block that is replaced.
//   - textAbove/textBelow are locator-only context and are not edited.
//   - Only blank or whitespace-only lines may lie between that context and the target.
//   - lineHint is a soft 1-based hint and may only be used when expectedCount == 1.
//   - oldText and newText must be provided and must not be the empty string.
//   - newText is written exactly as provided after normalization into logical lines.
//   - Writes are atomic and preserve newline style and final newline presence.
func replaceText(
	ctx context.Context,
	args ReplaceTextArgs,
	p fspolicy.FSPolicy,
) (*ReplaceTextOut, error) {
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
	newText, err := normalizeRequiredTextBlockArg("newText", args.NewText)
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
			"replace match count mismatch: expected %d, found %d. diagnostics=%s suggestion=%s",
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
		tf.Lines = ioutil.ReplaceStringRange(tf.Lines, start, end, newText)
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

	replacedAt := make([]int, 0, len(matchIdxs))
	for _, idx := range matchIdxs {
		replacedAt = append(replacedAt, idx+1)
	}

	return &ReplaceTextOut{
		ReplacementsMade: len(matchIdxs),
		ReplacedAtLines:  replacedAt,
	}, nil
}

func normalizeRequiredTextBlockArg(name string, s *string) ([]string, error) {
	if s == nil {
		return nil, fmt.Errorf("%s is required", name)
	}
	if *s == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}

	lines := ioutil.NormalizeTextBlockString(*s)
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s must contain at least one line", name)
	}
	return lines, nil
}

func normalizeOptionalTextBlockArg(name string, s *string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	if *s == "" {
		return nil, fmt.Errorf("%s must not be empty", name)
	}

	lines := ioutil.NormalizeTextBlockString(*s)
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s must contain at least one line", name)
	}
	return lines, nil
}

func resolveTextBlockExpectedCount(expectedCount *int) (int, error) {
	expected := 1
	if expectedCount != nil {
		expected = *expectedCount
	}
	if expected < 1 {
		return 0, fmt.Errorf("expectedCount must be >= 1 (got %d)", expected)
	}
	return expected, nil
}

func validateTextBlockLineHint(lineHint *int, expected int) error {
	if lineHint == nil {
		return nil
	}
	if *lineHint < 1 {
		return fmt.Errorf("lineHint must be >= 1 (got %d)", *lineHint)
	}
	if expected != 1 {
		return errors.New("lineHint may only be used when expectedCount == 1")
	}
	return nil
}

func findTrimmedTextBlockMatchStarts(lines, textAbove, oldText, textBelow []string) []int {
	if len(oldText) == 0 {
		return nil
	}

	tLines := ioutil.GetTrimmedLines(lines)
	tAbove := ioutil.TrimTrailingBoundaryBlankLines(ioutil.GetTrimmedLines(textAbove))
	tOld := ioutil.GetTrimmedLines(oldText)
	tBelow := ioutil.TrimLeadingBoundaryBlankLines(ioutil.GetTrimmedLines(textBelow))

	var idxs []int
	for start := 0; start+len(tOld) <= len(tLines); start++ {
		if !ioutil.IsBlockEqualsAt(tLines, tOld, start) {
			continue
		}
		if !textBlockAboveContextMatchesAt(tLines, tAbove, start) {
			continue
		}
		if !textBlockBelowContextMatchesAt(tLines, tBelow, start+len(tOld)) {
			continue
		}
		idxs = append(idxs, start)
	}

	return idxs
}

func textBlockAboveContextMatchesAt(tLines, tAbove []string, targetStart int) bool {
	if len(tAbove) == 0 {
		return true
	}

	for end := targetStart; end >= len(tAbove); end-- {
		if end < targetStart && tLines[end] != "" {
			break
		}
		if ioutil.IsBlockEqualsAt(tLines, tAbove, end-len(tAbove)) {
			return true
		}
	}

	return false
}

func textBlockBelowContextMatchesAt(tLines, tBelow []string, targetEnd int) bool {
	if len(tBelow) == 0 {
		return true
	}

	for start := targetEnd; start+len(tBelow) <= len(tLines); start++ {
		if start > targetEnd && tLines[start-1] != "" {
			break
		}
		if ioutil.IsBlockEqualsAt(tLines, tBelow, start) {
			return true
		}
	}

	return false
}

type textBlockCandidateDiagnostic struct {
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
	BeforeLines []string `json:"beforeLines,omitempty"`
	MatchedText []string `json:"matchedText"`
	AfterLines  []string `json:"afterLines,omitempty"`
}

type textBlockDiagnostic struct {
	CandidateStartLines         []int                          `json:"candidateStartLines,omitempty"`
	AdditionalCandidatesOmitted int                            `json:"additionalCandidatesOmitted,omitempty"`
	SampleCandidates            []textBlockCandidateDiagnostic `json:"sampleCandidates,omitempty"`
	LineHint                    *ioutil.LineHintDiagnostic     `json:"lineHint,omitempty"`
}

func buildTextBlockDiagnosticJSON(
	lines []string,
	matchIdxs []int,
	matchWidth int,
	hint *ioutil.LineHintDiagnostic,
	maxCandidates int,
	contextLines int,
) string {
	diag := textBlockDiagnostic{
		CandidateStartLines: ioutil.OneBasedLineNumbers(matchIdxs),
		LineHint:            hint,
	}

	if maxCandidates < 0 {
		maxCandidates = 0
	}
	if contextLines < 0 {
		contextLines = 0
	}
	if matchWidth < 1 {
		matchWidth = 1
	}

	limit := len(matchIdxs)
	if maxCandidates > 0 && limit > maxCandidates {
		limit = maxCandidates
		diag.AdditionalCandidatesOmitted = len(matchIdxs) - maxCandidates
	}

	if limit > 0 && len(lines) > 0 {
		diag.SampleCandidates = make([]textBlockCandidateDiagnostic, 0, limit)
		for _, idx := range matchIdxs[:limit] {
			if idx < 0 || idx >= len(lines) {
				continue
			}

			matchEndExclusive := min(idx+matchWidth, len(lines))
			beforeStart := max(idx-contextLines, 0)
			afterEnd := min(matchEndExclusive+contextLines, len(lines))

			diag.SampleCandidates = append(diag.SampleCandidates, textBlockCandidateDiagnostic{
				StartLine:   idx + 1,
				EndLine:     matchEndExclusive,
				BeforeLines: toolutil.CloneStringSlice(lines[beforeStart:idx]),
				MatchedText: toolutil.CloneStringSlice(lines[idx:matchEndExclusive]),
				AfterLines:  toolutil.CloneStringSlice(lines[matchEndExclusive:afterEnd]),
			})
		}
	}

	b, err := json.Marshal(diag)
	if err != nil {
		return fmt.Sprintf("candidateStartLines=%v", ioutil.OneBasedLineNumbers(matchIdxs))
	}
	return string(b)
}

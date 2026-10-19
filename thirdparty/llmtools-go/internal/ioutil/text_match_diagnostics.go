package ioutil

import (
	"encoding/json"
	"fmt"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

type MaybeStartLineDiagnostic struct {
	RequestedLine     int   `json:"requestedLine"`
	Tolerance         int   `json:"tolerance"`
	ClosestDistance   int   `json:"closestDistance"`
	ClosestStartLines []int `json:"closestStartLines,omitempty"`
	Applied           bool  `json:"applied"`
}
type LineHintDiagnostic struct {
	RequestedLine      int   `json:"requestedLine"`
	Tolerance          int   `json:"tolerance"`
	ClosestDistance    int   `json:"closestDistance"`
	ClosestLineNumbers []int `json:"closestLineNumbers,omitempty"`
	Applied            bool  `json:"applied"`
}

type insertionPointCandidateDiagnostic struct {
	InsertedAtLine int      `json:"insertedAtLine"`
	BeforeLines    []string `json:"beforeLines,omitempty"`
	AfterLines     []string `json:"afterLines,omitempty"`
}

type insertionPointDiagnostic struct {
	CandidateInsertedAtLines    []int                               `json:"candidateInsertedAtLines,omitempty"`
	AdditionalCandidatesOmitted int                                 `json:"additionalCandidatesOmitted,omitempty"`
	SampleCandidates            []insertionPointCandidateDiagnostic `json:"sampleCandidates,omitempty"`
	LineHint                    *LineHintDiagnostic                 `json:"lineHint,omitempty"`
}

type blockMatchCandidateDiagnostic struct {
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
	BeforeLines []string `json:"beforeLines,omitempty"`
	MatchLines  []string `json:"matchLines"`
	AfterLines  []string `json:"afterLines,omitempty"`
}

type blockMatchDiagnostic struct {
	CandidateStartLines         []int                           `json:"candidateStartLines,omitempty"`
	AdditionalCandidatesOmitted int                             `json:"additionalCandidatesOmitted,omitempty"`
	SampleCandidates            []blockMatchCandidateDiagnostic `json:"sampleCandidates,omitempty"`
	MaybeStartLine              *MaybeStartLineDiagnostic       `json:"maybeStartLine,omitempty"`
}

// NarrowIndicesByLineHint applies a soft 1-based line hint to 0-based indices.
//
// Behavior:
//   - if lineHint is nil or invalid, no narrowing is applied
//   - if there is a unique closest candidate within tolerance, returns only that candidate
//   - otherwise returns the original candidates and emits diagnostics explaining the closest set
func NarrowIndicesByLineHint(
	idxs []int,
	lineHint *int,
	tolerance int,
) ([]int, *LineHintDiagnostic) {
	if len(idxs) == 0 || lineHint == nil || *lineHint <= 0 {
		return idxs, nil
	}
	if tolerance < 0 {
		tolerance = 0
	}

	hint := *lineHint
	minDist := -1
	closest := make([]int, 0, 1)

	for _, idx := range idxs {
		dist := idx + 1 - hint
		if dist < 0 {
			dist = -dist
		}
		if minDist == -1 || dist < minDist {
			minDist = dist
			closest = closest[:0]
			closest = append(closest, idx)
			continue
		}
		if dist == minDist {
			closest = append(closest, idx)
		}
	}

	diag := &LineHintDiagnostic{
		RequestedLine:      hint,
		Tolerance:          tolerance,
		ClosestDistance:    minDist,
		ClosestLineNumbers: OneBasedLineNumbers(closest),
	}

	if len(closest) == 1 && minDist <= tolerance {
		diag.Applied = true
		return []int{closest[0]}, diag
	}

	return idxs, diag
}

// NarrowMatchIndicesByMaybeStartLine applies a soft start-line hint.
//
// Behavior:
//   - if maybeStartLine is nil or invalid, no narrowing is applied
//   - if there is a unique closest candidate within tolerance, returns only that candidate
//   - otherwise returns the original candidates and emits diagnostics explaining the closest set
func NarrowMatchIndicesByMaybeStartLine(
	idxs []int,
	maybeStartLine *int,
	tolerance int,
) ([]int, *MaybeStartLineDiagnostic) {
	if len(idxs) == 0 || maybeStartLine == nil || *maybeStartLine <= 0 {
		return idxs, nil
	}
	if tolerance < 0 {
		tolerance = 0
	}

	hint := *maybeStartLine
	minDist := -1
	closest := make([]int, 0, 1)

	for _, idx := range idxs {
		dist := idx + 1 - hint
		if dist < 0 {
			dist = -dist
		}
		if minDist == -1 || dist < minDist {
			minDist = dist
			closest = closest[:0]
			closest = append(closest, idx)
			continue
		}
		if dist == minDist {
			closest = append(closest, idx)
		}
	}

	diag := &MaybeStartLineDiagnostic{
		RequestedLine:     hint,
		Tolerance:         tolerance,
		ClosestDistance:   minDist,
		ClosestStartLines: OneBasedLineNumbers(closest),
	}

	if len(closest) == 1 && minDist <= tolerance {
		diag.Applied = true
		return []int{closest[0]}, diag
	}

	return idxs, diag
}

func BuildInsertionPointDiagnosticJSON(
	lines []string,
	insertIdxs []int,
	aboveWidth int,
	belowWidth int,
	hint *LineHintDiagnostic,
	maxCandidates int,
	contextLines int,
) string {
	diag := insertionPointDiagnostic{
		CandidateInsertedAtLines: OneBasedLineNumbers(insertIdxs),
		LineHint:                 hint,
	}

	if maxCandidates < 0 {
		maxCandidates = 0
	}
	if contextLines < 0 {
		contextLines = 0
	}
	if aboveWidth < 0 {
		aboveWidth = 0
	}
	if belowWidth < 0 {
		belowWidth = 0
	}

	limit := len(insertIdxs)
	if maxCandidates > 0 && limit > maxCandidates {
		limit = maxCandidates
		diag.AdditionalCandidatesOmitted = len(insertIdxs) - maxCandidates
	}

	if limit > 0 {
		diag.SampleCandidates = make([]insertionPointCandidateDiagnostic, 0, limit)
		for _, idx := range insertIdxs[:limit] {
			if idx < 0 || idx > len(lines) {
				continue
			}

			beforeStart := max(idx-(aboveWidth+contextLines), 0)
			afterEnd := min(idx+belowWidth+contextLines, len(lines))

			diag.SampleCandidates = append(diag.SampleCandidates, insertionPointCandidateDiagnostic{
				InsertedAtLine: idx + 1,
				BeforeLines:    toolutil.CloneStringSlice(lines[beforeStart:idx]),
				AfterLines:     toolutil.CloneStringSlice(lines[idx:afterEnd]),
			})
		}
	}

	b, err := json.Marshal(diag)
	if err != nil {
		return fmt.Sprintf("candidateInsertedAtLines=%v", OneBasedLineNumbers(insertIdxs))
	}
	return string(b)
}

func BuildBlockMatchDiagnosticJSON(
	lines []string,
	matchIdxs []int,
	matchWidth int,
	hint *MaybeStartLineDiagnostic,
	maxCandidates int,
	contextLines int,
) string {
	diag := blockMatchDiagnostic{
		CandidateStartLines: OneBasedLineNumbers(matchIdxs),
		MaybeStartLine:      hint,
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
		diag.SampleCandidates = make([]blockMatchCandidateDiagnostic, 0, limit)
		for _, idx := range matchIdxs[:limit] {
			if idx < 0 || idx >= len(lines) {
				continue
			}

			matchEndExclusive := min(idx+matchWidth, len(lines))

			beforeStart := max(idx-contextLines, 0)

			afterEnd := min(matchEndExclusive+contextLines, len(lines))

			diag.SampleCandidates = append(diag.SampleCandidates, blockMatchCandidateDiagnostic{
				StartLine:   idx + 1,
				EndLine:     matchEndExclusive,
				BeforeLines: toolutil.CloneStringSlice(lines[beforeStart:idx]),
				MatchLines:  toolutil.CloneStringSlice(lines[idx:matchEndExclusive]),
				AfterLines:  toolutil.CloneStringSlice(lines[matchEndExclusive:afterEnd]),
			})
		}
	}

	b, err := json.Marshal(diag)
	if err != nil {
		return fmt.Sprintf("candidateStartLines=%v", OneBasedLineNumbers(matchIdxs))
	}
	return string(b)
}

func OneBasedLineNumbers(idxs []int) []int {
	if len(idxs) == 0 {
		return nil
	}
	out := make([]int, len(idxs))
	for i, idx := range idxs {
		out[i] = idx + 1
	}
	return out
}

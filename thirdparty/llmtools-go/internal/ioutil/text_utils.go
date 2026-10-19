package ioutil

import (
	"fmt"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

// NewlineKind describes the newline convention detected in a file.
type NewlineKind string

const (
	NewlineLF   NewlineKind = "lf"
	NewlineCRLF NewlineKind = "crlf"
)

func (n NewlineKind) sep() string {
	if n == NewlineCRLF {
		return "\r\n"
	}
	return "\n"
}

func NormalizeLineBlockInput(in []string) []string {
	if in == nil {
		return nil
	}

	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		s = strings.TrimRight(s, "\n")

		parts := strings.Split(s, "\n")
		out = append(out, parts...)
	}
	return out
}

func RequireSingleTrimmedBlockMatch(lines, block []string, name string) (int, error) {
	idxs := FindTrimmedBlockMatches(lines, block)
	if len(idxs) == 0 {
		return 0, fmt.Errorf(
			"no match found for %s; suggestion=copy a longer distinctive block from the file or read nearby lines first",
			name,
		)
	}
	if len(idxs) > 1 {
		return 0, fmt.Errorf(
			"ambiguous match for %s: found %d occurrences. diagnostics=%s suggestion=copy a longer distinctive block from the file",
			name,
			len(idxs),
			BuildBlockMatchDiagnosticJSON(lines, idxs, len(block), nil, 5, 1),
		)
	}
	return idxs[0], nil
}

func RequireSingleMatch(idxs []int, name string) (int, error) {
	if len(idxs) == 0 {
		return 0, fmt.Errorf("no match found for %s", name)
	}
	if len(idxs) > 1 {
		return 0, fmt.Errorf(
			"ambiguous match for %s: found %d occurrences at start lines %v; provide a more specific match",
			name,
			len(idxs),
			OneBasedLineNumbers(idxs),
		)
	}
	return idxs[0], nil
}

// NormalizeTextBlockString normalizes a multiline text block into logical lines.
//
// It normalizes CRLF/CR to LF and removes at most one terminal newline, so a
// final line terminator does not create an extra empty line while intentional
// trailing blank lines are preserved.
func NormalizeTextBlockString(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// InsertionPointMatch describes one candidate insertion point.
type InsertionPointMatch struct {
	InsertAt   int
	AboveStart *int
	BelowStart *int
}

// FindTrimmedInsertionPointMatches returns the 0-based insertion indices from
// FindTrimmedInsertionPointMatchCandidates.
func FindTrimmedInsertionPointMatches(lines, above, below []string) []int {
	return InsertionPointMatchIndices(FindTrimmedInsertionPointMatchCandidates(lines, above, below))
}

// InsertionPointMatchIndices returns the 0-based insertion indices from matches.
func InsertionPointMatchIndices(matches []InsertionPointMatch) []int {
	if len(matches) == 0 {
		return nil
	}
	out := make([]int, len(matches))
	for i, m := range matches {
		out[i] = m.InsertAt
	}
	return out
}

// FindTrimmedInsertionPointMatchCandidates returns candidate insertion points.
//
// Matching compares strings.TrimSpace line-wise.
//
// If only one of above/below is provided, matching is exact at the immediate
// boundary.
//
// If both are provided, the matcher allows zero or more blank or
// whitespace-only lines between them and places the insertion point at the
// start of that blank gap. To reduce newline-count brittleness, trailing blank
// lines in above and leading blank lines in below are ignored for matching when
// those blocks also contain at least one nonblank line.
func FindTrimmedInsertionPointMatchCandidates(lines, above, below []string) []InsertionPointMatch {
	if len(above) == 0 && len(below) == 0 {
		return nil
	}

	tLines := GetTrimmedLines(lines)

	switch {
	case len(above) > 0 && len(below) > 0:
		tAbove := TrimTrailingBoundaryBlankLines(GetTrimmedLines(above))
		tBelow := TrimLeadingBoundaryBlankLines(GetTrimmedLines(below))

		var matches []InsertionPointMatch
		for insertAt := 0; insertAt <= len(tLines); insertAt++ {
			var aboveStart *int
			if len(tAbove) > 0 {
				start := insertAt - len(tAbove)
				if !IsBlockEqualsAt(tLines, tAbove, start) {
					continue
				}
				aboveStart = &start
			}

			var belowStart *int
			if len(tBelow) > 0 {
				found := -1
				for start := insertAt; start <= len(tLines); start++ {
					if start > insertAt && tLines[start-1] != "" {
						break
					}
					if IsBlockEqualsAt(tLines, tBelow, start) {
						found = start
						break
					}
				}
				if found < 0 {
					continue
				}
				belowStart = &found
			}

			matches = append(matches, InsertionPointMatch{
				InsertAt:   insertAt,
				AboveStart: aboveStart,
				BelowStart: belowStart,
			})
		}
		return matches

	case len(above) > 0:
		tAbove := GetTrimmedLines(above)
		var matches []InsertionPointMatch
		for start := 0; start+len(tAbove) <= len(tLines); start++ {
			if !IsBlockEqualsAt(tLines, tAbove, start) {
				continue
			}
			s := start
			matches = append(matches, InsertionPointMatch{
				InsertAt:   start + len(tAbove),
				AboveStart: &s,
			})
		}
		return matches

	default:
		tBelow := GetTrimmedLines(below)
		var matches []InsertionPointMatch
		for start := 0; start+len(tBelow) <= len(tLines); start++ {
			if !IsBlockEqualsAt(tLines, tBelow, start) {
				continue
			}
			s := start
			matches = append(matches, InsertionPointMatch{
				InsertAt:   start,
				BelowStart: &s,
			})
		}
		return matches
	}
}

func TrimTrailingBoundaryBlankLines(lines []string) []string {
	if len(lines) == 0 || !HasAnyNonBlankTrimmedLine(lines) {
		return toolutil.CloneStringSlice(lines)
	}
	end := len(lines)
	for end > 0 && lines[end-1] == "" {
		end--
	}
	return toolutil.CloneStringSlice(lines[:end])
}

func TrimLeadingBoundaryBlankLines(lines []string) []string {
	if len(lines) == 0 || !HasAnyNonBlankTrimmedLine(lines) {
		return toolutil.CloneStringSlice(lines)
	}
	start := 0
	for start < len(lines) && lines[start] == "" {
		start++
	}
	return toolutil.CloneStringSlice(lines[start:])
}

func HasAnyNonBlankTrimmedLine(lines []string) bool {
	for _, line := range lines {
		if line != "" {
			return true
		}
	}
	return false
}

func FindTrimmedBlockMatches(lines, block []string) []int {
	if len(block) == 0 {
		return nil
	}

	tLines := GetTrimmedLines(lines)
	tBlock := GetTrimmedLines(block)

	var idxs []int
	for i := 0; i+len(tBlock) <= len(tLines); i++ {
		if IsBlockEqualsAt(tLines, tBlock, i) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}

func FindTrimmedAdjacentBlockMatches(lines, before, match, after []string) []int {
	if len(match) == 0 {
		return nil
	}

	tLines := GetTrimmedLines(lines)
	tBefore := GetTrimmedLines(before)
	tMatch := GetTrimmedLines(match)
	tAfter := GetTrimmedLines(after)

	var idxs []int
	for i := 0; i+len(tMatch) <= len(tLines); i++ {
		if !IsBlockEqualsAt(tLines, tMatch, i) {
			continue
		}

		if len(tBefore) > 0 {
			if i-len(tBefore) < 0 {
				continue
			}
			if !IsBlockEqualsAt(tLines, tBefore, i-len(tBefore)) {
				continue
			}
		}

		if len(tAfter) > 0 {
			afterStart := i + len(tMatch)
			if afterStart+len(tAfter) > len(tLines) {
				continue
			}
			if !IsBlockEqualsAt(tLines, tAfter, afterStart) {
				continue
			}
		}

		idxs = append(idxs, i)
	}

	return idxs
}

func EnsureNonOverlappingFixedWidth(matchIdxs []int, width int) error {
	if len(matchIdxs) <= 1 || width <= 0 {
		return nil
	}
	for i := 0; i < len(matchIdxs)-1; i++ {
		if matchIdxs[i]+width > matchIdxs[i+1] {
			return fmt.Errorf(
				"overlapping matches detected at start lines %d and %d; provide tighter surrounding context to disambiguate",
				matchIdxs[i]+1,
				matchIdxs[i+1]+1,
			)
		}
	}
	return nil
}

func GetTrimmedLines(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i := range lines {
		out[i] = strings.TrimSpace(lines[i])
	}
	return out
}

func IsBlockEqualsAt(haystack, needle []string, start int) bool {
	if start < 0 {
		return false
	}
	if len(needle) == 0 {
		return true
	}
	if start+len(needle) > len(haystack) {
		return false
	}
	for j := range needle {
		if haystack[start+j] != needle[j] {
			return false
		}
	}
	return true
}

func ReplaceStringRange(lines []string, start, end int, repl []string) []string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}

	out := make([]string, 0, len(lines)-(end-start)+len(repl))
	out = append(out, lines[:start]...)
	out = append(out, repl...)
	out = append(out, lines[end:]...)
	return out
}

func StringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

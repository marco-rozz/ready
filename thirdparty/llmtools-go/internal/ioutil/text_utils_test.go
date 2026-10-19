package ioutil

import (
	"slices"
	"strings"
	"testing"
)

const (
	textUtilsTestMatchLine = "match"
	textUtilsTestAfterLine = "after"
)

type insertionPointCandidateJSON struct {
	InsertedAtLine int      `json:"insertedAtLine"`
	BeforeLines    []string `json:"beforeLines,omitempty"`
	AfterLines     []string `json:"afterLines,omitempty"`
}

type insertionPointDiagnosticJSON struct {
	CandidateInsertedAtLines    []int                         `json:"candidateInsertedAtLines,omitempty"`
	AdditionalCandidatesOmitted int                           `json:"additionalCandidatesOmitted,omitempty"`
	SampleCandidates            []insertionPointCandidateJSON `json:"sampleCandidates,omitempty"`
	LineHint                    *LineHintDiagnostic           `json:"lineHint,omitempty"`
}

type blockMatchCandidateJSON struct {
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
	BeforeLines []string `json:"beforeLines,omitempty"`
	MatchLines  []string `json:"matchLines"`
	AfterLines  []string `json:"afterLines,omitempty"`
}

type blockMatchDiagnosticJSON struct {
	CandidateStartLines         []int                     `json:"candidateStartLines,omitempty"`
	AdditionalCandidatesOmitted int                       `json:"additionalCandidatesOmitted,omitempty"`
	SampleCandidates            []blockMatchCandidateJSON `json:"sampleCandidates,omitempty"`
	MaybeStartLine              *MaybeStartLineDiagnostic `json:"maybeStartLine,omitempty"`
}

func TestTextBlockNormalizationAndBoundaryHelpers(t *testing.T) {
	t.Run("normalize text block string handles CRLF and trims one terminal newline", func(t *testing.T) {
		got := NormalizeTextBlockString("a\r\nb\n")
		want := []string{"a", "b"}
		if !slices.Equal(got, want) {
			t.Fatalf("got=%#v want=%#v", got, want)
		}
	})

	t.Run("trim trailing boundary blank lines", func(t *testing.T) {
		got := TrimTrailingBoundaryBlankLines([]string{"a", "", ""})
		want := []string{"a"}
		if !slices.Equal(got, want) {
			t.Fatalf("got=%#v want=%#v", got, want)
		}

		allBlank := []string{"", ""}
		got = TrimTrailingBoundaryBlankLines(allBlank)
		if !slices.Equal(got, allBlank) {
			t.Fatalf("all blank got=%#v want=%#v", got, allBlank)
		}
	})

	t.Run("trim leading boundary blank lines", func(t *testing.T) {
		got := TrimLeadingBoundaryBlankLines([]string{"", "", "a"})
		want := []string{"a"}
		if !slices.Equal(got, want) {
			t.Fatalf("got=%#v want=%#v", got, want)
		}

		allBlank := []string{"", ""}
		got = TrimLeadingBoundaryBlankLines(allBlank)
		if !slices.Equal(got, allBlank) {
			t.Fatalf("all blank got=%#v want=%#v", got, allBlank)
		}
	})

	t.Run("has any non blank trimmed line", func(t *testing.T) {
		if HasAnyNonBlankTrimmedLine([]string{"", ""}) {
			t.Fatalf("expected false for all blank lines")
		}
		if !HasAnyNonBlankTrimmedLine([]string{"", "x"}) {
			t.Fatalf("expected true when a non-blank line is present")
		}
	})
}

func TestFindTrimmedInsertionPointMatchCandidatesCoverage(t *testing.T) {
	t.Run("both blocks with boundary blanks are trimmed and matched", func(t *testing.T) {
		lines := []string{"header", "alpha", "", "beta", "footer"}
		above := []string{"alpha", ""}
		below := []string{"", "beta"}

		got := FindTrimmedInsertionPointMatchCandidates(lines, above, below)
		if len(got) != 1 {
			t.Fatalf("len(got)=%d want=1; got=%#v", len(got), got)
		}
		if got[0].InsertAt != 2 {
			t.Fatalf("InsertAt=%d want=2", got[0].InsertAt)
		}
		if got[0].AboveStart == nil || *got[0].AboveStart != 1 {
			t.Fatalf("AboveStart=%v want=1", got[0].AboveStart)
		}
		if got[0].BelowStart == nil || *got[0].BelowStart != 3 {
			t.Fatalf("BelowStart=%v want=3", got[0].BelowStart)
		}

		idxs := FindTrimmedInsertionPointMatches(lines, above, below)
		if !slices.Equal(idxs, []int{2}) {
			t.Fatalf("FindTrimmedInsertionPointMatches=%v want=[2]", idxs)
		}
		if !slices.Equal(InsertionPointMatchIndices(got), []int{2}) {
			t.Fatalf("InsertionPointMatchIndices=%v want=[2]", InsertionPointMatchIndices(got))
		}
	})

	t.Run("above only finds insertion after the block", func(t *testing.T) {
		lines := []string{"one", "target", "three"}
		got := FindTrimmedInsertionPointMatchCandidates(lines, []string{"target"}, nil)
		if len(got) != 1 {
			t.Fatalf("len(got)=%d want=1", len(got))
		}
		if got[0].InsertAt != 2 {
			t.Fatalf("InsertAt=%d want=2", got[0].InsertAt)
		}
		if got[0].AboveStart == nil || *got[0].AboveStart != 1 {
			t.Fatalf("AboveStart=%v want=1", got[0].AboveStart)
		}
		if got[0].BelowStart != nil {
			t.Fatalf("BelowStart=%v want=nil", got[0].BelowStart)
		}

		idxs := FindTrimmedInsertionPointMatches(lines, []string{"target"}, nil)
		if !slices.Equal(idxs, []int{2}) {
			t.Fatalf("FindTrimmedInsertionPointMatches=%v want=[2]", idxs)
		}
	})

	t.Run("below only finds insertion before the block", func(t *testing.T) {
		lines := []string{"one", "target", "three"}
		got := FindTrimmedInsertionPointMatchCandidates(lines, nil, []string{"target"})
		if len(got) != 1 {
			t.Fatalf("len(got)=%d want=1", len(got))
		}
		if got[0].InsertAt != 1 {
			t.Fatalf("InsertAt=%d want=1", got[0].InsertAt)
		}
		if got[0].BelowStart == nil || *got[0].BelowStart != 1 {
			t.Fatalf("BelowStart=%v want=1", got[0].BelowStart)
		}
		if got[0].AboveStart != nil {
			t.Fatalf("AboveStart=%v want=nil", got[0].AboveStart)
		}

		idxs := FindTrimmedInsertionPointMatches(lines, nil, []string{"target"})
		if !slices.Equal(idxs, []int{1}) {
			t.Fatalf("FindTrimmedInsertionPointMatches=%v want=[1]", idxs)
		}
	})

	t.Run("insertion point indices helper handles empty input", func(t *testing.T) {
		if got := InsertionPointMatchIndices(nil); got != nil {
			t.Fatalf("got=%v want=nil", got)
		}
		got := InsertionPointMatchIndices([]InsertionPointMatch{{InsertAt: 2}, {InsertAt: 5}})
		if !slices.Equal(got, []int{2, 5}) {
			t.Fatalf("got=%v want=[2 5]", got)
		}
	})
}

func TestRequireSingleMatchHelpers(t *testing.T) {
	t.Run("require single trimmed block match unique", func(t *testing.T) {
		idx, err := RequireSingleTrimmedBlockMatch(
			[]string{"before", " alpha ", " beta ", "after"},
			[]string{"alpha", "beta"},
			"block",
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idx != 1 {
			t.Fatalf("idx=%d want=1", idx)
		}
	})

	t.Run("require single trimmed block match no match", func(t *testing.T) {
		_, err := RequireSingleTrimmedBlockMatch([]string{"before", "alpha"}, []string{"gamma"}, "block")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no match found") {
			t.Fatalf("error %q does not contain %q", err.Error(), "no match found")
		}
	})

	t.Run("require single trimmed block match ambiguous", func(t *testing.T) {
		_, err := RequireSingleTrimmedBlockMatch(
			[]string{"alpha", "beta", "x", "alpha", "beta"},
			[]string{"alpha", "beta"},
			"block",
		)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous match") || !strings.Contains(err.Error(), "found 2 occurrences") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("require single unique match", func(t *testing.T) {
		idx, err := RequireSingleMatch([]int{4}, "match")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if idx != 4 {
			t.Fatalf("idx=%d want=4", idx)
		}
	})

	t.Run("require single no match", func(t *testing.T) {
		_, err := RequireSingleMatch(nil, "match")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "no match found") {
			t.Fatalf("error %q does not contain %q", err.Error(), "no match found")
		}
	})

	t.Run("require single ambiguous match", func(t *testing.T) {
		_, err := RequireSingleMatch([]int{1, 4}, "match")
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "ambiguous match") || !strings.Contains(err.Error(), "start lines") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestNarrowIndicesByLineHintCoverage(t *testing.T) {
	tests := []struct {
		name      string
		idxs      []int
		hint      *int
		tolerance int
		want      []int
		wantDiag  *LineHintDiagnostic
	}{
		{
			name:      "nil hint leaves indices untouched",
			idxs:      []int{1, 4},
			hint:      nil,
			tolerance: 2,
			want:      []int{1, 4},
			wantDiag:  nil,
		},
		{
			name:      "unique closest within tolerance narrows",
			idxs:      []int{1, 4, 7},
			hint:      new(5),
			tolerance: 1,
			want:      []int{4},
			wantDiag: &LineHintDiagnostic{
				RequestedLine:      5,
				Tolerance:          1,
				ClosestDistance:    0,
				ClosestLineNumbers: []int{5},
				Applied:            true,
			},
		},
		{
			name:      "tie does not narrow",
			idxs:      []int{1, 3},
			hint:      new(3),
			tolerance: 2,
			want:      []int{1, 3},
			wantDiag: &LineHintDiagnostic{
				RequestedLine:      3,
				Tolerance:          2,
				ClosestDistance:    1,
				ClosestLineNumbers: []int{2, 4},
				Applied:            false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, diag := NarrowIndicesByLineHint(tc.idxs, tc.hint, tc.tolerance)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			if tc.wantDiag == nil {
				if diag != nil {
					t.Fatalf("expected nil diagnostic, got=%+v", diag)
				}
				return
			}
			if diag == nil {
				t.Fatalf("expected diagnostic, got nil")
			}
			if diag.RequestedLine != tc.wantDiag.RequestedLine || diag.Tolerance != tc.wantDiag.Tolerance ||
				diag.ClosestDistance != tc.wantDiag.ClosestDistance ||
				diag.Applied != tc.wantDiag.Applied {
				t.Fatalf("diag=%+v want=%+v", diag, tc.wantDiag)
			}
			if !slices.Equal(diag.ClosestLineNumbers, tc.wantDiag.ClosestLineNumbers) {
				t.Fatalf("ClosestLineNumbers=%v want=%v", diag.ClosestLineNumbers, tc.wantDiag.ClosestLineNumbers)
			}
		})
	}
}

func TestNarrowMatchIndicesByMaybeStartLineCoverage(t *testing.T) {
	tests := []struct {
		name      string
		idxs      []int
		hint      *int
		tolerance int
		want      []int
		wantDiag  *MaybeStartLineDiagnostic
	}{
		{
			name:      "nil hint leaves indices untouched",
			idxs:      []int{0, 4},
			hint:      nil,
			tolerance: 2,
			want:      []int{0, 4},
			wantDiag:  nil,
		},
		{
			name:      "unique closest within tolerance narrows",
			idxs:      []int{0, 3, 6},
			hint:      new(4),
			tolerance: 1,
			want:      []int{3},
			wantDiag: &MaybeStartLineDiagnostic{
				RequestedLine:     4,
				Tolerance:         1,
				ClosestDistance:   0,
				ClosestStartLines: []int{4},
				Applied:           true,
			},
		},
		{
			name:      "tie does not narrow",
			idxs:      []int{1, 3},
			hint:      new(3),
			tolerance: 2,
			want:      []int{1, 3},
			wantDiag: &MaybeStartLineDiagnostic{
				RequestedLine:     3,
				Tolerance:         2,
				ClosestDistance:   1,
				ClosestStartLines: []int{2, 4},
				Applied:           false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, diag := NarrowMatchIndicesByMaybeStartLine(tc.idxs, tc.hint, tc.tolerance)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
			if tc.wantDiag == nil {
				if diag != nil {
					t.Fatalf("expected nil diagnostic, got=%+v", diag)
				}
				return
			}
			if diag == nil {
				t.Fatalf("expected diagnostic, got nil")
			}
			if diag.RequestedLine != tc.wantDiag.RequestedLine || diag.Tolerance != tc.wantDiag.Tolerance ||
				diag.ClosestDistance != tc.wantDiag.ClosestDistance ||
				diag.Applied != tc.wantDiag.Applied {
				t.Fatalf("diag=%+v want=%+v", diag, tc.wantDiag)
			}
			if !slices.Equal(diag.ClosestStartLines, tc.wantDiag.ClosestStartLines) {
				t.Fatalf("ClosestStartLines=%v want=%v", diag.ClosestStartLines, tc.wantDiag.ClosestStartLines)
			}
		})
	}
}

func TestDiagnosticJSONBuildersAndSliceHelpers(t *testing.T) {
	t.Run("insertion point diagnostic json", func(t *testing.T) {
		lines := []string{"before", "insert", "after", "tail"}
		hint := &LineHintDiagnostic{
			RequestedLine:      2,
			Tolerance:          1,
			ClosestDistance:    0,
			ClosestLineNumbers: []int{2},
			Applied:            true,
		}

		got := BuildInsertionPointDiagnosticJSON(lines, []int{1, 3}, 1, 1, hint, 1, 1)
		var diag insertionPointDiagnosticJSON
		mustUnmarshalJSON(t, got, &diag)

		if !slices.Equal(diag.CandidateInsertedAtLines, []int{2, 4}) {
			t.Fatalf("CandidateInsertedAtLines=%v want=[2 4]", diag.CandidateInsertedAtLines)
		}
		if diag.AdditionalCandidatesOmitted != 1 {
			t.Fatalf("AdditionalCandidatesOmitted=%d want=1", diag.AdditionalCandidatesOmitted)
		}
		if diag.LineHint == nil || !diag.LineHint.Applied || diag.LineHint.RequestedLine != 2 {
			t.Fatalf("LineHint=%+v want requestedLine=2 applied=true", diag.LineHint)
		}
		if len(diag.SampleCandidates) != 1 {
			t.Fatalf("len(SampleCandidates)=%d want=1", len(diag.SampleCandidates))
		}
		if diag.SampleCandidates[0].InsertedAtLine != 2 {
			t.Fatalf("InsertedAtLine=%d want=2", diag.SampleCandidates[0].InsertedAtLine)
		}
		if !slices.Equal(diag.SampleCandidates[0].BeforeLines, []string{"before"}) {
			t.Fatalf("BeforeLines=%#v want=[before]", diag.SampleCandidates[0].BeforeLines)
		}
		if !slices.Equal(diag.SampleCandidates[0].AfterLines, []string{"insert", "after"}) {
			t.Fatalf("AfterLines=%#v want=[insert after]", diag.SampleCandidates[0].AfterLines)
		}
	})

	t.Run("block match diagnostic json", func(t *testing.T) {
		lines := []string{"before", "matchA", "between", "matchB", "after"}
		hint := &MaybeStartLineDiagnostic{
			RequestedLine:     3,
			Tolerance:         1,
			ClosestDistance:   0,
			ClosestStartLines: []int{3},
			Applied:           true,
		}

		got := BuildBlockMatchDiagnosticJSON(lines, []int{1, 3}, 1, hint, 1, 1)
		var diag blockMatchDiagnosticJSON
		mustUnmarshalJSON(t, got, &diag)

		if !slices.Equal(diag.CandidateStartLines, []int{2, 4}) {
			t.Fatalf("CandidateStartLines=%v want=[2 4]", diag.CandidateStartLines)
		}
		if diag.AdditionalCandidatesOmitted != 1 {
			t.Fatalf("AdditionalCandidatesOmitted=%d want=1", diag.AdditionalCandidatesOmitted)
		}
		if diag.MaybeStartLine == nil || !diag.MaybeStartLine.Applied || diag.MaybeStartLine.RequestedLine != 3 {
			t.Fatalf("MaybeStartLine=%+v want requestedLine=3 applied=true", diag.MaybeStartLine)
		}
		if len(diag.SampleCandidates) != 1 {
			t.Fatalf("len(SampleCandidates)=%d want=1", len(diag.SampleCandidates))
		}
		if diag.SampleCandidates[0].StartLine != 2 || diag.SampleCandidates[0].EndLine != 2 {
			t.Fatalf(
				"StartLine/EndLine=%d/%d want=2/2",
				diag.SampleCandidates[0].StartLine,
				diag.SampleCandidates[0].EndLine,
			)
		}
		if !slices.Equal(diag.SampleCandidates[0].BeforeLines, []string{"before"}) {
			t.Fatalf("BeforeLines=%#v want=[before]", diag.SampleCandidates[0].BeforeLines)
		}
		if !slices.Equal(diag.SampleCandidates[0].MatchLines, []string{"matchA"}) {
			t.Fatalf("MatchLines=%#v want=[matchA]", diag.SampleCandidates[0].MatchLines)
		}
		if !slices.Equal(diag.SampleCandidates[0].AfterLines, []string{"between"}) {
			t.Fatalf("AfterLines=%#v want=[between]", diag.SampleCandidates[0].AfterLines)
		}
	})

	t.Run("one based line numbers and range helpers", func(t *testing.T) {
		if got := OneBasedLineNumbers(nil); got != nil {
			t.Fatalf("OneBasedLineNumbers(nil)=%v want=nil", got)
		}
		if got := OneBasedLineNumbers([]int{0, 2}); !slices.Equal(got, []int{1, 3}) {
			t.Fatalf("OneBasedLineNumbers=%v want=[1 3]", got)
		}

		if got := ReplaceStringRange(
			[]string{"a", "b", "c", "d"},
			1,
			3,
			[]string{"x", "y"},
		); !slices.Equal(
			got,
			[]string{"a", "x", "y", "d"},
		) {
			t.Fatalf("ReplaceStringRange mismatch: got=%#v", got)
		}
		if got := ReplaceStringRange(
			[]string{"a", "b", "c"},
			-5,
			1,
			[]string{"z"},
		); !slices.Equal(
			got,
			[]string{"z", "b", "c"},
		) {
			t.Fatalf("ReplaceStringRange clamp mismatch: got=%#v", got)
		}

		if !StringSlicesEqual(nil, []string{}) {
			t.Fatalf("StringSlicesEqual(nil, empty) want=true")
		}
		if StringSlicesEqual([]string{"a"}, []string{"b"}) {
			t.Fatalf("StringSlicesEqual mismatch want=false")
		}
	})
}

func TestNormalizeLineBlockInput(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "nil stays nil",
			in:   nil,
			want: nil,
		},
		{
			name: "empty slice stays empty",
			in:   []string{},
			want: []string{},
		},
		{
			name: "splits embedded newlines and trims trailing newline",
			in:   []string{"a\nb\n", "c"},
			want: []string{"a", "b", "c"},
		},
		{
			name: "handles CRLF and CR",
			in:   []string{"a\r\nb\r\n", "c\rd"},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "preserves intentional empty lines",
			in:   []string{"a\n\nb"},
			want: []string{"a", "", "b"},
		},
		{
			name: "empty string remains one empty line",
			in:   []string{""},
			want: []string{""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeLineBlockInput(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%#v want=%#v", got, tc.want)
			}
		})
	}
}

func TestFindTrimmedBlockMatches(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		block []string
		want  []int
	}{
		{
			name:  "empty block yields nil",
			lines: []string{"a"},
			block: nil,
			want:  nil,
		},
		{
			name:  "single match with whitespace differences",
			lines: []string{"  a  ", "b", "c"},
			block: []string{"a", " b "},
			want:  []int{0},
		},
		{
			name:  "multiple matches",
			lines: []string{" a ", "b", "x", "a", " b "},
			block: []string{"a", "b"},
			want:  []int{0, 3},
		},
		{
			name:  "no matches",
			lines: []string{"a", "b"},
			block: []string{"b", "a"},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindTrimmedBlockMatches(tc.lines, tc.block)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestFindTrimmedAdjacentBlockMatches(t *testing.T) {
	lines := []string{
		"HEADER",
		" before ",
		textUtilsTestMatchLine,
		" " + textUtilsTestAfterLine + " ",
		textUtilsTestMatchLine,
		textUtilsTestAfterLine,
	}

	tests := []struct {
		name   string
		before []string
		match  []string
		after  []string
		want   []int
	}{
		{
			name:   "match only",
			match:  []string{textUtilsTestMatchLine},
			before: nil,
			after:  nil,
			want:   []int{2, 4},
		},
		{
			name:   "before+match+after must be adjacent",
			before: []string{"before"},
			match:  []string{textUtilsTestMatchLine},
			after:  []string{textUtilsTestAfterLine},
			want:   []int{2},
		},
		{
			name:   "before required but not present => none",
			before: []string{"nope"},
			match:  []string{textUtilsTestMatchLine},
			after:  []string{textUtilsTestAfterLine},
			want:   nil,
		},
		{
			name:   "empty match => nil",
			before: []string{"before"},
			match:  nil,
			after:  []string{textUtilsTestAfterLine},
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindTrimmedAdjacentBlockMatches(lines, tc.before, tc.match, tc.after)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestEnsureNonOverlappingFixedWidth(t *testing.T) {
	tests := []struct {
		name     string
		idxs     []int
		width    int
		wantErr  bool
		contains string
	}{
		{"len<=1 ok", []int{5}, 2, false, ""},
		{"width<=0 ok", []int{1, 2}, 0, false, ""},
		{"non-overlapping ok", []int{0, 3, 6}, 3, false, ""},
		{"overlapping errors", []int{0, 2}, 3, true, "overlapping matches"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureNonOverlappingFixedWidth(tc.idxs, tc.width)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.contains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetTrimmedLines(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil => nil", nil, nil},
		{"empty => nil", []string{}, nil},
		{"trims all", []string{" a ", "\tb\t", "\n"}, []string{"a", "b", ""}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := GetTrimmedLines(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got=%#v want=%#v", got, tc.want)
			}
		})
	}
}

func TestIsBlockEqualsAt(t *testing.T) {
	h := []string{"a", "b", "c", "d"}
	tests := []struct {
		name   string
		needle []string
		start  int
		want   bool
	}{
		{"match at 1", []string{"b", "c"}, 1, true},
		{"mismatch at 0", []string{"b"}, 0, false},
		{"match single", []string{"d"}, 3, true},
		{"out of range start returns false (no panic)", []string{"c"}, 99, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsBlockEqualsAt(h, tc.needle, tc.start)
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

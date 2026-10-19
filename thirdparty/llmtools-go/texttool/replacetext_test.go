package texttool

import (
	"context"
	"errors"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

const (
	replaceTextDisambiguatedInitial = "hdr\nctx1\n\nX\n\nctx2\nctx1\nX\nctx3\n"
	replaceTextDisambiguatedWant    = "hdr\nctx1\n\nREPL\n\nctx2\nctx1\nX\nctx3\n"
	replaceTextTrimSpaceInitial     = "A\n  B  \n"
)

func TestReplaceText_HappyPaths(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name        string
		initial     string
		args        func(path string) ReplaceTextArgs
		wantContent string
		wantMade    int
		wantAtLines []int
	}{
		{
			name:    "replace_single_line_with_two_lines_default_expected_1",
			initial: testTextABC,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("B"),
					NewText: new("X\nY"),
				}
			},
			wantContent: testTextAXYC,
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "replace_disambiguated_by_textAbove_textBelow_with_blank_gap_tolerance",
			initial: replaceTextDisambiguatedInitial,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:      path,
					OldText:   new("X"),
					NewText:   new("REPL"),
					TextAbove: new("ctx1\n\n\n"),
					TextBelow: new("\n\nctx2"),
				}
			},
			wantContent: replaceTextDisambiguatedWant,
			wantMade:    1,
			wantAtLines: []int{4},
		},
		{
			name:    "preserves_crlf_newlines_and_final_newline",
			initial: testTextABCRLF,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("B"),
					NewText: new("X"),
				}
			},
			wantContent: "A\r\nX\r\n",
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "preserves_absence_of_final_newline",
			initial: "A\nB",
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("B"),
					NewText: new("X"),
				}
			},
			wantContent: "A\nX",
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "matching_uses_trimspace_but_newText_written_verbatim",
			initial: replaceTextTrimSpaceInitial,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("B"),
					NewText: new("  X  "),
				}
			},
			wantContent: "A\n  X  \n",
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "multiple_replacements_expected_2_reports_original_line_numbers",
			initial: testTextAXBXC,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:          path,
					OldText:       new("X"),
					NewText:       new("Y"),
					ExpectedCount: new(2),
				}
			},
			wantContent: "A\nY\nB\nY\nC\n",
			wantMade:    2,
			wantAtLines: []int{2, 4},
		},
		{
			name:    "newText_embedded_newlines_splits_into_multiple_lines",
			initial: "A\nX\n",
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("X"),
					NewText: new("Y\nZ"),
				}
			},
			wantContent: "A\nY\nZ\n",
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "blank_line_replacement_uses_explicit_newline_block",
			initial: "A\nX\nB\n",
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("X"),
					NewText: new("\n"),
				}
			},
			wantContent: testTextAEmptyB,
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "blank_line_target_uses_explicit_newline_block",
			initial: testTextAEmptyB,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:      path,
					OldText:   new("\n"),
					NewText:   new("X"),
					TextAbove: new("A"),
					TextBelow: new("B"),
				}
			},
			wantContent: testTextAXB,
			wantMade:    1,
			wantAtLines: []int{2},
		},
		{
			name:    "lineHint_narrows_ambiguous_match",
			initial: testTextAXBXC,
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:     path,
					OldText:  new("X"),
					NewText:  new("Y"),
					LineHint: new(4),
				}
			},
			wantContent: "A\nX\nB\nY\nC\n",
			wantMade:    1,
			wantAtLines: []int{4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempTextFile(t, dir, "repl-*.txt", tt.initial)
			args := tt.args(path)

			out, err := replaceText(t.Context(), args, policy)
			mustNoErr(t, err)

			if out.ReplacementsMade != len(out.ReplacedAtLines) {
				t.Fatalf(
					"invariant failed: ReplacementsMade=%d but len(ReplacedAtLines)=%d",
					out.ReplacementsMade,
					len(out.ReplacedAtLines),
				)
			}
			if out.ReplacementsMade != tt.wantMade {
				t.Fatalf("ReplacementsMade: want %d, got %d", tt.wantMade, out.ReplacementsMade)
			}
			if len(out.ReplacedAtLines) != len(tt.wantAtLines) {
				t.Fatalf(
					"ReplacedAtLines len: want %d, got %d (%v)",
					len(tt.wantAtLines),
					len(out.ReplacedAtLines),
					out.ReplacedAtLines,
				)
			}
			for i := range tt.wantAtLines {
				if out.ReplacedAtLines[i] != tt.wantAtLines[i] {
					t.Fatalf("ReplacedAtLines[%d]: want %d, got %d", i, tt.wantAtLines[i], out.ReplacedAtLines[i])
				}
			}

			got := readFileString(t, path)
			if got != tt.wantContent {
				t.Fatalf("content mismatch\nwant:\n%q\ngot:\n%q", tt.wantContent, got)
			}
		})
	}
}

func TestReplaceText_ErrorCases(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name              string
		setup             func() string
		args              func(path string) ReplaceTextArgs
		wantErrSub        string
		wantIsCtx         bool
		checkContentAfter bool
		wantContentAfter  string
	}{
		{
			name: "oldText_required",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: nil,
					NewText: new("X"),
				}
			},
			wantErrSub: "oldText is required",
		},
		{
			name: "oldText_must_not_be_empty",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new(""),
					NewText: new("X"),
				}
			},
			wantErrSub: "oldText must not be empty",
		},
		{
			name: "newText_required_nil",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("A"),
					NewText: nil,
				}
			},
			wantErrSub: "newText is required",
		},
		{
			name: "newText_must_not_be_empty",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("A"),
					NewText: new(""),
				}
			},
			wantErrSub: "newText must not be empty",
		},
		{
			name: "textAbove_must_not_be_empty",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "A\nX\n")
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:      path,
					OldText:   new("X"),
					NewText:   new("Y"),
					TextAbove: new(""),
				}
			},
			wantErrSub: "textAbove must not be empty",
		},
		{
			name: "textBelow_must_not_be_empty",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "X\nB\n")
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:      path,
					OldText:   new("X"),
					NewText:   new("Y"),
					TextBelow: new(""),
				}
			},
			wantErrSub: "textBelow must not be empty",
		},
		{
			name: "expectedCount_must_be_ge_1",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:          path,
					OldText:       new("A"),
					NewText:       new("X"),
					ExpectedCount: new(0),
				}
			},
			wantErrSub: "expectedCount must be >= 1",
		},
		{
			name: "lineHint_must_be_ge_1",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:     path,
					OldText:  new("A"),
					NewText:  new("X"),
					LineHint: new(0),
				}
			},
			wantErrSub: testErrLineHintMustBeGe1,
		},
		{
			name: "lineHint_may_only_be_used_when_expectedCount_is_1",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "A\nA\n") //nolint:dupword // Test.
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:          path,
					OldText:       new("A"),
					NewText:       new("X"),
					LineHint:      new(1),
					ExpectedCount: new(2),
				}
			},
			wantErrSub: "lineHint may only be used when expectedCount == 1",
		},
		{
			name: "match_count_mismatch_does_not_modify_file",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "A\nA\n") //nolint:dupword // Test.
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("A"),
					NewText: new("X"),
				}
			},
			wantErrSub:        testErrReplaceMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  "A\nA\n", //nolint:dupword // Test.
		},
		{
			name: "lineHint_tie_does_not_break_ambiguity",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAXBXC)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:     path,
					OldText:  new("X"),
					NewText:  new("Y"),
					LineHint: new(3),
				}
			},
			wantErrSub:        testErrReplaceMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  testTextAXBXC,
		},
		{
			name: "textAbove_does_not_skip_nonblank_lines",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "A\nmid\nX\n")
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:      path,
					OldText:   new("X"),
					NewText:   new("Y"),
					TextAbove: new("A"),
				}
			},
			wantErrSub:        testErrReplaceMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  "A\nmid\nX\n",
		},
		{
			name: "overlapping_matches_rejected",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", "X\nX\nX\n") //nolint:dupword // Test.
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:          path,
					OldText:       new("X\nX"),
					NewText:       new("Y\nY"),
					ExpectedCount: new(2),
				}
			},
			wantErrSub:        "overlapping matches detected",
			checkContentAfter: true,
			wantContentAfter:  "X\nX\nX\n", //nolint:dupword // Test.
		},
		{
			name: testNameContextCanceled,
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReplaceTextArgs {
				return ReplaceTextArgs{
					Path:    path,
					OldText: new("A"),
					NewText: new("X"),
				}
			},
			wantIsCtx: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup()
			args := tt.args(path)

			ctx := t.Context()
			if tt.wantIsCtx {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}

			_, err := replaceText(ctx, args, policy)
			if tt.wantIsCtx {
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
				return
			}

			mustErrContains(t, err, tt.wantErrSub)

			if tt.checkContentAfter {
				got := readFileString(t, path)
				if got != tt.wantContentAfter {
					t.Fatalf("file changed on error\nwant:\n%q\ngot:\n%q", tt.wantContentAfter, got)
				}
			}
		})
	}
}

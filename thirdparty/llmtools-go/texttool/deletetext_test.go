package texttool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

const (
	deleteTextDisambiguatedInitial = "hdr\nctx1\n\nX\nctx2\nctx1\nX\nctx3\n"
	deleteTextDisambiguatedWant    = "hdr\nctx1\n\nctx2\nctx1\nX\nctx3\n"
	deleteTextWhitespaceInitial    = "A\n  B  \nC\n"
)

func TestDeleteText_HappyPaths(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name          string
		initial       string
		args          func(path string) DeleteTextArgs
		wantContent   string
		wantDeletions int
		wantDeletedAt []int
	}{
		{
			name:    "delete_single_line_default_expected_1",
			initial: testTextABC,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("B"),
				}
			},
			wantContent:   testTextAC,
			wantDeletions: 1,
			wantDeletedAt: []int{2},
		},
		{
			name:    "delete_disambiguated_by_textAbove_textBelow_with_blank_gap_tolerance",
			initial: deleteTextDisambiguatedInitial,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:      path,
					OldText:   new("X"),
					TextAbove: new("ctx1\n\n\n"),
					TextBelow: new("\nctx2"),
				}
			},
			wantContent:   deleteTextDisambiguatedWant,
			wantDeletions: 1,
			wantDeletedAt: []int{4},
		},
		{
			name:    "delete_all_lines_preserves_final_newline",
			initial: testTextAOnly,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("A"),
				}
			},
			wantContent:   "\n",
			wantDeletions: 1,
			wantDeletedAt: []int{1},
		},
		{
			name:    "preserves_crlf_newlines",
			initial: testTextABCRLF,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("A"),
				}
			},
			wantContent:   "B\r\n",
			wantDeletions: 1,
			wantDeletedAt: []int{1},
		},
		{
			name:    "trimspace_matching_deletes_whitespace_padded_line",
			initial: deleteTextWhitespaceInitial,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("B"),
				}
			},
			wantContent:   testTextAC,
			wantDeletions: 1,
			wantDeletedAt: []int{2},
		},
		{
			name:    "oldText_embedded_newlines_deletes_multiline_block",
			initial: "A\nX\nY\nB\n",
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("X\nY"),
				}
			},
			wantContent:   testTextAB,
			wantDeletions: 1,
			wantDeletedAt: []int{2},
		},
		{
			name:    "multiple_deletions_expected_2_reports_original_line_numbers",
			initial: testTextAXBXC,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:          path,
					OldText:       new("X"),
					ExpectedCount: new(2),
				}
			},
			wantContent:   testTextABC,
			wantDeletions: 2,
			wantDeletedAt: []int{2, 4},
		},
		{
			name:    "oldText_blank_line_can_be_deleted_using_explicit_newline",
			initial: testTextAEmptyB,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:      path,
					OldText:   new("\n"),
					TextAbove: new("A"),
					TextBelow: new("B"),
				}
			},
			wantContent:   testTextAB,
			wantDeletions: 1,
			wantDeletedAt: []int{2},
		},
		{
			name:    "lineHint_narrows_ambiguous_match",
			initial: testTextAXBXC,
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:     path,
					OldText:  new("X"),
					LineHint: new(4),
				}
			},
			wantContent:   "A\nX\nB\nC\n",
			wantDeletions: 1,
			wantDeletedAt: []int{4},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempTextFile(t, dir, "del-*.txt", tt.initial)
			args := tt.args(path)

			out, err := deleteText(t.Context(), args, policy)
			mustNoErr(t, err)

			if out.DeletionsMade != len(out.DeletedAtLines) {
				t.Fatalf(
					"invariant failed: DeletionsMade=%d but len(DeletedAtLines)=%d",
					out.DeletionsMade,
					len(out.DeletedAtLines),
				)
			}
			if out.DeletionsMade != tt.wantDeletions {
				t.Fatalf("DeletionsMade: want %d, got %d", tt.wantDeletions, out.DeletionsMade)
			}
			if len(out.DeletedAtLines) != len(tt.wantDeletedAt) {
				t.Fatalf(
					"DeletedAtLines len: want %d, got %d (%v)",
					len(tt.wantDeletedAt),
					len(out.DeletedAtLines),
					out.DeletedAtLines,
				)
			}
			for i := range tt.wantDeletedAt {
				if out.DeletedAtLines[i] != tt.wantDeletedAt[i] {
					t.Fatalf("DeletedAtLines[%d]: want %d, got %d", i, tt.wantDeletedAt[i], out.DeletedAtLines[i])
				}
			}

			got := readFileString(t, path)
			if got != tt.wantContent {
				t.Fatalf("content mismatch\nwant:\n%q\ngot:\n%q", tt.wantContent, got)
			}
		})
	}
}

func TestDeleteText_ErrorCases(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name              string
		setup             func(t *testing.T) string
		args              func(path string) DeleteTextArgs
		wantErrSub        string
		wantAnyErr        bool
		wantIsCtx         bool
		checkContentAfter bool
		wantContentAfter  string
	}{
		{
			name: "oldText_required",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{Path: path, OldText: nil}
			},
			wantErrSub: "oldText is required",
		},
		{
			name: "oldText_must_not_be_empty",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{Path: path, OldText: new("")}
			},
			wantErrSub: "oldText must not be empty",
		},
		{
			name: "textAbove_must_not_be_empty",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "A\nX\n")
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:      path,
					OldText:   new("X"),
					TextAbove: new(""),
				}
			},
			wantErrSub: "textAbove must not be empty",
		},
		{
			name: "textBelow_must_not_be_empty",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "X\nB\n")
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:      path,
					OldText:   new("X"),
					TextBelow: new(""),
				}
			},
			wantErrSub: "textBelow must not be empty",
		},
		{
			name: "expectedCount_must_be_ge_1",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:          path,
					OldText:       new("A"),
					ExpectedCount: new(0),
				}
			},
			wantErrSub: "expectedCount must be >= 1",
		},
		{
			name: "lineHint_must_be_ge_1",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:     path,
					OldText:  new("A"),
					LineHint: new(0),
				}
			},
			wantErrSub: testErrLineHintMustBeGe1,
		},
		{
			name: "lineHint_may_only_be_used_when_expectedCount_is_1",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "A\nA\n") //nolint:dupword // Test.
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:          path,
					OldText:       new("A"),
					LineHint:      new(1),
					ExpectedCount: new(2),
				}
			},
			wantErrSub: "lineHint may only be used when expectedCount == 1",
		},
		{
			name: "match_count_mismatch_does_not_modify_file",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "A\nX\nA\nX\n")
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("A"),
				}
			},
			wantErrSub:        testErrDeleteMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  "A\nX\nA\nX\n",
		},
		{
			name: "lineHint_tie_does_not_break_ambiguity",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAXBXC)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:     path,
					OldText:  new("X"),
					LineHint: new(3),
				}
			},
			wantErrSub:        testErrDeleteMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  testTextAXBXC,
		},
		{
			name: "textAbove_does_not_skip_nonblank_lines",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "A\nmid\nX\n")
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:      path,
					OldText:   new("X"),
					TextAbove: new("A"),
				}
			},
			wantErrSub:        testErrDeleteMatchCountMismatch,
			checkContentAfter: true,
			wantContentAfter:  "A\nmid\nX\n",
		},
		{
			name: "overlapping_matches_rejected",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", "X\nX\nX\n") //nolint:dupword // Test.
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:          path,
					OldText:       new("X\nX"),
					ExpectedCount: new(2),
				}
			},
			wantErrSub:        "overlapping matches detected",
			checkContentAfter: true,
			wantContentAfter:  "X\nX\nX\n", //nolint:dupword // Test.
		},
		{
			name: "invalid_utf8_rejected",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempBytesFile(t, dir, "x-*.txt", []byte{0xff, 0xfe, 0xfd})
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{Path: path, OldText: new("A")}
			},
			wantErrSub: "not valid UTF-8",
		},
		{
			name: "file_not_found",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(dir, testMissingFileName)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{Path: path, OldText: new("A")}
			},
			wantAnyErr: true,
		},
		{
			name: testNameContextCanceled,
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) DeleteTextArgs {
				return DeleteTextArgs{
					Path:    path,
					OldText: new("A"),
				}
			},
			wantIsCtx: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			args := tt.args(path)

			ctx := t.Context()
			if tt.wantIsCtx {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}

			_, err := deleteText(ctx, args, policy)
			if tt.wantIsCtx {
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
				return
			}
			if tt.wantAnyErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
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

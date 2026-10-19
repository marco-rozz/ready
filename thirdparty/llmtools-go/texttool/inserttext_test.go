package texttool

import (
	"context"
	"errors"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

const (
	insertTextAnchorTrimSpaceInitial = "a\n  ANCHOR  \nb\n"
	insertTextExactBoundaryInitial   = "## Intro\nParagraph\n## Usage\nParagraph\n"
	insertTextBoundaryDriftInitial   = "<!-- A END -->\n\n## Section B\n"
	insertTextAnchorListInitial      = "ANCHOR\nx\nANCHOR\ny\n"
)

func TestInsertText_HappyPaths(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	type want struct {
		content               string
		insertedAtLine        int
		insertedLineCount     int
		textAboveMatchedAtPtr *int
		textBelowMatchedAtPtr *int
	}

	tests := []struct {
		name    string
		initial string
		args    InsertTextArgs
		want    want
	}{
		{
			name:    "position_end_inserts_at_end_preserves_final_newline",
			initial: testTextAB,
			args: InsertTextArgs{
				Position: whereEnd,
				Text:     new("X"),
			},
			want: want{
				content:           "A\nB\nX\n",
				insertedAtLine:    3,
				insertedLineCount: 1,
			},
		},
		{
			name:    "position_start_inserts_at_start",
			initial: testTextABC,
			args: InsertTextArgs{
				Position: whereStart,
				Text:     new("X"),
			},
			want: want{
				content:           "X\nA\nB\nC\n",
				insertedAtLine:    1,
				insertedLineCount: 1,
			},
		},
		{
			name:    "between_with_textBelow_inserts_before_matching_block_trimspace_match",
			initial: insertTextAnchorTrimSpaceInitial,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("X"),
				TextBelow: new("ANCHOR"),
			},
			want: want{
				content:               "a\nX\n  ANCHOR  \nb\n",
				insertedAtLine:        2,
				insertedLineCount:     1,
				textBelowMatchedAtPtr: new(2),
			},
		},
		{
			name:    "between_with_textAbove_inserts_after_matching_block_trimspace_match",
			initial: insertTextAnchorTrimSpaceInitial,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("X"),
				TextAbove: new("ANCHOR"),
			},
			want: want{
				content:               "a\n  ANCHOR  \nX\nb\n",
				insertedAtLine:        3,
				insertedLineCount:     1,
				textAboveMatchedAtPtr: new(2),
			},
		},
		{
			name:    "between_with_textAbove_and_textBelow_inserts_at_exact_boundary",
			initial: insertTextExactBoundaryInitial,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("Inserted"),
				TextAbove: new("## Usage"),
				TextBelow: new("Paragraph"),
			},
			want: want{
				content:               "## Intro\nParagraph\n## Usage\nInserted\nParagraph\n",
				insertedAtLine:        4,
				insertedLineCount:     1,
				textAboveMatchedAtPtr: new(3),
				textBelowMatchedAtPtr: new(4),
			},
		},
		{
			name:    "between_with_blank_gap_inserts_at_start_of_gap",
			initial: testTextAEmptyB,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("X"),
				TextAbove: new("A"),
				TextBelow: new("B"),
			},
			want: want{
				content:               "A\nX\n\nB\n",
				insertedAtLine:        2,
				insertedLineCount:     1,
				textAboveMatchedAtPtr: new(1),
				textBelowMatchedAtPtr: new(3),
			},
		},
		{
			name:    "between_tolerates_blank_line_drift_on_boundary_blocks",
			initial: insertTextBoundaryDriftInitial,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("\nInserted after A"),
				TextAbove: new("<!-- A END -->\n\n"),
				TextBelow: new("\n## Section B"),
			},
			want: want{
				content:               "<!-- A END -->\n\nInserted after A\n\n## Section B\n",
				insertedAtLine:        2,
				insertedLineCount:     2,
				textAboveMatchedAtPtr: new(1),
				textBelowMatchedAtPtr: new(3),
			},
		},
		{
			name:    "between_lineHint_narrows_ambiguous_location",
			initial: insertTextAnchorListInitial,
			args: InsertTextArgs{
				Position:  whereBetween,
				Text:      new("X"),
				TextBelow: new("ANCHOR"),
				LineHint:  new(3),
			},
			want: want{
				content:               "ANCHOR\nx\nX\nANCHOR\ny\n",
				insertedAtLine:        3,
				insertedLineCount:     1,
				textBelowMatchedAtPtr: new(3),
			},
		},
		{
			name:    "text_with_embedded_newlines_splits_into_multiple_lines",
			initial: testTextAB,
			args: InsertTextArgs{
				Position: whereEnd,
				Text:     new("X\nY"),
			},
			want: want{
				content:           "A\nB\nX\nY\n",
				insertedAtLine:    3,
				insertedLineCount: 2,
			},
		},
		{
			name:    "preserves_crlf_newlines_and_final_newline",
			initial: testTextABCRLF,
			args: InsertTextArgs{
				Position: whereStart,
				Text:     new("X"),
			},
			want: want{
				content:           "X\r\nA\r\nB\r\n",
				insertedAtLine:    1,
				insertedLineCount: 1,
			},
		},
		{
			name:    "empty_file_no_final_newline_preserved",
			initial: testTextEmpty,
			args: InsertTextArgs{
				Position: whereEnd,
				Text:     new("A"),
			},
			want: want{
				content:           "A",
				insertedAtLine:    1,
				insertedLineCount: 1,
			},
		},
		{
			name:    "file_with_single_empty_line_and_final_newline_keeps_final_newline",
			initial: "\n",
			args: InsertTextArgs{
				Position: whereStart,
				Text:     new("A"),
			},
			want: want{
				content:           "A\n\n",
				insertedAtLine:    1,
				insertedLineCount: 1,
			},
		},
		{
			name:    "non_empty_file_without_final_newline_preserved",
			initial: "A",
			args: InsertTextArgs{
				Position: whereEnd,
				Text:     new("B"),
			},
			want: want{
				content:           "A\nB",
				insertedAtLine:    2,
				insertedLineCount: 1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempTextFile(t, dir, "insert-*.txt", tt.initial)
			tt.args.Path = path

			out, err := insertText(t.Context(), tt.args, policy)
			mustNoErr(t, err)

			got := readFileString(t, path)
			if got != tt.want.content {
				t.Fatalf("content mismatch\nwant:\n%q\ngot:\n%q", tt.want.content, got)
			}

			if out.InsertedAtLine != tt.want.insertedAtLine {
				t.Fatalf("InsertedAtLine: want %d, got %d", tt.want.insertedAtLine, out.InsertedAtLine)
			}
			if out.InsertedLineCount != tt.want.insertedLineCount {
				t.Fatalf("InsertedLineCount: want %d, got %d", tt.want.insertedLineCount, out.InsertedLineCount)
			}

			if tt.want.textAboveMatchedAtPtr == nil {
				if out.TextAboveMatchedAtLine != nil {
					t.Fatalf("TextAboveMatchedAtLine: want nil, got %v", *out.TextAboveMatchedAtLine)
				}
			} else {
				if out.TextAboveMatchedAtLine == nil {
					t.Fatalf("TextAboveMatchedAtLine: want %d, got nil", *tt.want.textAboveMatchedAtPtr)
				}
				if *out.TextAboveMatchedAtLine != *tt.want.textAboveMatchedAtPtr {
					t.Fatalf(
						"TextAboveMatchedAtLine: want %d, got %d",
						*tt.want.textAboveMatchedAtPtr,
						*out.TextAboveMatchedAtLine,
					)
				}
			}

			if tt.want.textBelowMatchedAtPtr == nil {
				if out.TextBelowMatchedAtLine != nil {
					t.Fatalf("TextBelowMatchedAtLine: want nil, got %v", *out.TextBelowMatchedAtLine)
				}
			} else {
				if out.TextBelowMatchedAtLine == nil {
					t.Fatalf("TextBelowMatchedAtLine: want %d, got nil", *tt.want.textBelowMatchedAtPtr)
				}
				if *out.TextBelowMatchedAtLine != *tt.want.textBelowMatchedAtPtr {
					t.Fatalf(
						"TextBelowMatchedAtLine: want %d, got %d",
						*tt.want.textBelowMatchedAtPtr,
						*out.TextBelowMatchedAtLine,
					)
				}
			}
		})
	}
}

func TestInsertText_ErrorCases(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name              string
		setupFile         func() string
		args              func(path string) InsertTextArgs
		wantErrSub        string
		wantIsCtx         bool
		checkContentAfter bool
		wantContentAfter  string
	}{
		{
			name: "text_required",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:     path,
					Position: whereEnd,
				}
			},
			wantErrSub: "text is required",
		},
		{
			name: "position_required",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path: path,
					Text: new("X"),
				}
			},
			wantErrSub: "position is required",
		},
		{
			name: "invalid_position",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:     path,
					Position: "middle",
					Text:     new("X"),
				}
			},
			wantErrSub: "invalid position value",
		},
		{
			name: "boundary_fields_forbidden_when_position_end",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereEnd,
					Text:      new("X"),
					TextBelow: new("A"),
				}
			},
			wantErrSub: `textAbove/textBelow/lineHint must be omitted`,
		},
		{
			name: "between_requires_textAbove_or_textBelow",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:     path,
					Position: whereBetween,
					Text:     new("X"),
				}
			},
			wantErrSub: `requires textAbove or textBelow`,
		},
		{
			name: "lineHint_must_be_positive",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereBetween,
					Text:      new("X"),
					TextBelow: new("A"),
					LineHint:  new(0),
				}
			},
			wantErrSub: testErrLineHintMustBeGe1,
		},
		{
			name: "insertion_point_no_match",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAB)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereBetween,
					Text:      new("X"),
					TextBelow: new("NOPE"),
				}
			},
			wantErrSub: "no insertion point matched",
		},
		{
			name: "between_does_not_skip_nonblank_lines",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", "A\nnote\nB\n")
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereBetween,
					Text:      new("X"),
					TextAbove: new("A"),
					TextBelow: new("B"),
				}
			},
			wantErrSub: "no insertion point matched",
		},
		{
			name: "insertion_point_ambiguous_match",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", insertTextAnchorListInitial)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereBetween,
					Text:      new("X"),
					TextBelow: new("ANCHOR"),
				}
			},
			wantErrSub: "ambiguous insertion point",
		},
		{
			name: "position_end_error_does_not_modify_file",
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:      path,
					Position:  whereEnd,
					Text:      new("X"),
					TextAbove: new("A"),
				}
			},
			wantErrSub:        `textAbove/textBelow/lineHint must be omitted`,
			checkContentAfter: true,
			wantContentAfter:  testTextAOnly,
		},
		{
			name: testNameContextCanceled,
			setupFile: func() string {
				return writeTempTextFile(t, dir, "ins-*.txt", testTextAOnly)
			},
			args: func(path string) InsertTextArgs {
				return InsertTextArgs{
					Path:     path,
					Position: whereEnd,
					Text:     new("X"),
				}
			},
			wantIsCtx: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setupFile()
			args := tt.args(path)

			ctx := t.Context()
			if tt.wantIsCtx {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}

			_, err := insertText(ctx, args, policy)
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

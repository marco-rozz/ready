package texttool

import (
	"context"
	"errors"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

func TestReadTextRange_HappyPaths(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name      string
		initial   string
		args      func(path string) ReadTextRangeArgs
		wantStart int
		wantEnd   int
		wantCount int
		wantEOF   bool
		wantFirst *ReadTextRangeLine
		wantLast  *ReadTextRangeLine
	}{
		{
			name:    "no_args_returns_entire_short_file_with_defaults",
			initial: testTextABC,
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{Path: path}
			},
			wantStart: 1,
			wantEnd:   3,
			wantCount: 3,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 1, Text: "A"},
			wantLast:  &ReadTextRangeLine{LineNumber: 3, Text: "C"},
		},
		{
			name:    "startLine_only_reads_from_given_line_to_eof",
			initial: "A\nB\nC\nD\n",
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(3),
				}
			},
			wantStart: 3,
			wantEnd:   4,
			wantCount: 2,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 3, Text: "C"},
			wantLast:  &ReadTextRangeLine{LineNumber: 4, Text: "D"},
		},
		{
			name:    "startLine_and_lineCount_reads_requested_window_not_at_eof",
			initial: "A\nB\nC\nD\nE\n",
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(2),
					LineCount: new(2),
				}
			},
			wantStart: 2,
			wantEnd:   3,
			wantCount: 2,
			wantEOF:   false,
			wantFirst: &ReadTextRangeLine{LineNumber: 2, Text: "B"},
			wantLast:  &ReadTextRangeLine{LineNumber: 3, Text: "C"},
		},
		{
			name:    "lineCount_larger_than_remaining_returns_to_eof",
			initial: "A\nB\nC\nD\nE\n",
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(4),
					LineCount: new(10),
				}
			},
			wantStart: 4,
			wantEnd:   5,
			wantCount: 2,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 4, Text: "D"},
			wantLast:  &ReadTextRangeLine{LineNumber: 5, Text: "E"},
		},
		{
			name:    "startLine_at_last_line_returns_one_line_and_eof",
			initial: testTextABC,
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(3),
				}
			},
			wantStart: 3,
			wantEnd:   3,
			wantCount: 1,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 3, Text: "C"},
			wantLast:  &ReadTextRangeLine{LineNumber: 3, Text: "C"},
		},
		{
			name:    "file_with_single_empty_line_and_final_newline",
			initial: "\n",
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{Path: path}
			},
			wantStart: 1,
			wantEnd:   1,
			wantCount: 1,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 1, Text: ""},
			wantLast:  &ReadTextRangeLine{LineNumber: 1, Text: ""},
		},
		{
			name:    "explicit_startLine_1_on_empty_file_returns_empty_and_eof",
			initial: testTextEmpty,
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(1),
					LineCount: new(10),
				}
			},
			wantStart: 0,
			wantEnd:   0,
			wantCount: 0,
			wantEOF:   true,
		},
		{
			name:    "exactly_max_output_lines_allowed",
			initial: makeNLines(16000, func(i int) string { return "x" }, "\n", true),
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					LineCount: new(16000),
				}
			},
			wantStart: 1,
			wantEnd:   16000,
			wantCount: 16000,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 1, Text: "x"},
			wantLast:  &ReadTextRangeLine{LineNumber: 16000, Text: "x"},
		},
		{
			name:    "omitted_lineCount_uses_default_and_may_not_reach_eof",
			initial: makeNLines(2001, func(i int) string { return "x" }, "\n", true),
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path: path,
				}
			},
			wantStart: 1,
			wantEnd:   1000,
			wantCount: 1000,
			wantEOF:   false,
			wantFirst: &ReadTextRangeLine{LineNumber: 1, Text: "x"},
			wantLast:  &ReadTextRangeLine{LineNumber: 1000, Text: "x"},
		},
		{
			name:    "omitted_lineCount_from_late_start_can_reach_eof",
			initial: makeNLines(2001, func(i int) string { return "x" }, "\n", true),
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(1500),
				}
			},
			wantStart: 1500,
			wantEnd:   2001,
			wantCount: 502,
			wantEOF:   true,
			wantFirst: &ReadTextRangeLine{LineNumber: 1500, Text: "x"},
			wantLast:  &ReadTextRangeLine{LineNumber: 2001, Text: "x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempTextFile(t, dir, "range-*.txt", tt.initial)
			args := tt.args(path)

			out, err := readTextRange(t.Context(), args, policy)
			mustNoErr(t, err)

			if len(out.Lines) != out.LinesReturned {
				t.Fatalf("invariant failed: len(Lines)=%d but LinesReturned=%d", len(out.Lines), out.LinesReturned)
			}
			if out.StartLine != tt.wantStart || out.EndLine != tt.wantEnd {
				t.Fatalf("Start/End: want %d/%d got %d/%d", tt.wantStart, tt.wantEnd, out.StartLine, out.EndLine)
			}
			if out.LinesReturned != tt.wantCount {
				t.Fatalf("LinesReturned: want %d, got %d", tt.wantCount, out.LinesReturned)
			}
			if out.EOFReached != tt.wantEOF {
				t.Fatalf("EOFReached: want %v, got %v", tt.wantEOF, out.EOFReached)
			}

			if tt.wantCount == 0 {
				if len(out.Lines) != 0 {
					t.Fatalf("expected zero output lines, got %d", len(out.Lines))
				}
				return
			}

			if out.EndLine-out.StartLine+1 != out.LinesReturned {
				t.Fatalf(
					"invariant failed: StartLine=%d EndLine=%d LinesReturned=%d",
					out.StartLine,
					out.EndLine,
					out.LinesReturned,
				)
			}

			for i, line := range out.Lines {
				wantLineNumber := out.StartLine + i
				if line.LineNumber != wantLineNumber {
					t.Fatalf("Lines[%d].LineNumber: want %d, got %d", i, wantLineNumber, line.LineNumber)
				}
			}

			if tt.wantFirst != nil {
				if len(out.Lines) < 1 {
					t.Fatalf("expected at least 1 line, got 0")
				}
				got := out.Lines[0]
				if got.LineNumber != tt.wantFirst.LineNumber || got.Text != tt.wantFirst.Text {
					t.Fatalf("first line mismatch: want %+v got %+v", *tt.wantFirst, got)
				}
			}

			if tt.wantLast != nil {
				if len(out.Lines) < 1 {
					t.Fatalf("expected at least 1 line, got 0")
				}
				got := out.Lines[len(out.Lines)-1]
				if got.LineNumber != tt.wantLast.LineNumber || got.Text != tt.wantLast.Text {
					t.Fatalf("last line mismatch: want %+v got %+v", *tt.wantLast, got)
				}
			}
		})
	}
}

func TestReadTextRange_ErrorCases(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name       string
		setup      func() string
		args       func(path string) ReadTextRangeArgs
		wantErrSub string
		wantIsCtx  bool
	}{
		{
			name: "startLine_must_be_ge_1",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(0),
				}
			},
			wantErrSub: "startLine must be >= 1",
		},
		{
			name: "lineCount_must_be_ge_1",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					LineCount: new(0),
				}
			},
			wantErrSub: "lineCount must be >= 1",
		},
		{
			name: "lineCount_too_large",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					LineCount: new(16001),
				}
			},
			wantErrSub: "lineCount too large",
		},
		{
			name: "startLine_out_of_bounds_for_non_empty_file",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAB)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(3),
				}
			},
			wantErrSub: "out of bounds for file with 2 lines",
		},
		{
			name: "startLine_out_of_bounds_for_empty_file",
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextEmpty)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{
					Path:      path,
					StartLine: new(2),
				}
			},
			wantErrSub: "out of bounds for empty file",
		},
		{
			name: testNameContextCanceled,
			setup: func() string {
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) ReadTextRangeArgs {
				return ReadTextRangeArgs{Path: path}
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

			_, err := readTextRange(ctx, args, policy)
			if tt.wantIsCtx {
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
				return
			}

			mustErrContains(t, err, tt.wantErrSub)
		})
	}
}

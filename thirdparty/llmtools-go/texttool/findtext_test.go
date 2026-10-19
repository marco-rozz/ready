package texttool

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestFindText_HappyPaths(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name        string
		initial     string
		args        func(path string) FindTextArgs
		wantMatches int
		wantReached bool
		assert      func(t *testing.T, out *FindTextOut)
	}{
		{
			name:    "substring_default_queryType_trims_query_and_coerces_context_to_1",
			initial: " alpha \nbeta\n gamma alpha \ndelta\n",
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        "  alpha  ",
					ContextLines: 0,
					MaxMatches:   10,
				}
			},
			wantMatches: 2,
			wantReached: false,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				m0 := out.Matches[0]
				assertMatchRange(t, m0, 1, 2, 1, 7)
				if len(m0.Context) != 2 {
					t.Fatalf("match0 context len: want 2 got %d", len(m0.Context))
				}
				assertContextLine(t, m0.Context[0], 1, " alpha ")
				assertContextLine(t, m0.Context[1], 2, "beta")

				m1 := out.Matches[1]
				assertMatchRange(t, m1, 3, 8, 3, 13)
				if len(m1.Context) != 3 {
					t.Fatalf("match1 context len: want 3 got %d", len(m1.Context))
				}
				assertContextLine(t, m1.Context[0], 2, "beta")
				assertContextLine(t, m1.Context[1], 3, " gamma alpha ")
				assertContextLine(t, m1.Context[2], 4, "delta")
			},
		},
		{
			name:    "substring_multiline_query_matches_across_lines",
			initial: "func a() {\nreturn 1\n}\nnext\n",
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        "\nreturn 1\n}\n",
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 1,
			wantReached: false,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				m := out.Matches[0]
				assertMatchRange(t, m, 2, 1, 3, 2)
				if len(m.Context) != 4 {
					t.Fatalf("context len: want 4 got %d", len(m.Context))
				}
				assertContextLine(t, m.Context[0], 1, "func a() {")
				assertContextLine(t, m.Context[1], 2, "return 1")
				assertContextLine(t, m.Context[2], 3, "}")
				assertContextLine(t, m.Context[3], 4, "next")
			},
		},
		{
			name:    "substring_query_crlf_newlines_are_normalized",
			initial: "a\nb\nc\n",
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					QueryType:    "substring",
					Query:        "b\r\nc",
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 1,
			wantReached: false,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()
				assertMatchRange(t, out.Matches[0], 2, 1, 3, 2)
			},
		},
		{
			name:    "regex_queryType_is_trimmed_case_insensitive_and_matches_whole_file",
			initial: "prefix\nfoo()\nbar()\nsuffix\n",
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					QueryType:    "  ReGeX  ",
					Query:        "foo\\(\\)\r\nbar\\(\\)",
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 1,
			wantReached: false,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				m := out.Matches[0]
				assertMatchRange(t, m, 2, 1, 3, 6)
				if len(m.Context) != 4 {
					t.Fatalf("context len: want 4 got %d", len(m.Context))
				}
				assertContextLine(t, m.Context[0], 1, "prefix")
				assertContextLine(t, m.Context[1], 2, "foo()")
				assertContextLine(t, m.Context[2], 3, "bar()")
				assertContextLine(t, m.Context[3], 4, "suffix")
			},
		},
		{
			name:    "multiple_matches_on_same_line_have_distinct_columns",
			initial: testTextHitTrio,
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        testQueryHit,
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 3,
			wantReached: false,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				assertMatchRange(t, out.Matches[0], 1, 1, 1, 4)
				assertMatchRange(t, out.Matches[1], 1, 6, 1, 9)
				assertMatchRange(t, out.Matches[2], 1, 11, 1, 14)

				for i, m := range out.Matches {
					if len(m.Context) != 1 {
						t.Fatalf("match %d context len: want 1 got %d", i, len(m.Context))
					}
					assertContextLine(t, m.Context[0], 1, strings.TrimSpace(testTextHitTrio))
				}
			},
		},
		{
			name:    "maxMatches_enforced_and_reachedMaxMatches_set",
			initial: testTextHitTrio,
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        testQueryHit,
					ContextLines: 1,
					MaxMatches:   2,
				}
			},
			wantMatches: 2,
			wantReached: true,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				if out.AdditionalMatchesOmitted != 1 {
					t.Fatalf("AdditionalMatchesOmitted: want 1 got %d", out.AdditionalMatchesOmitted)
				}
				assertMatchRange(t, out.Matches[0], 1, 1, 1, 4)
				assertMatchRange(t, out.Matches[1], 1, 6, 1, 9)
			},
		},
		{
			name:    "maxMatches_0_defaults_to_10_and_sets_reachedMaxMatches",
			initial: makeNLines(11, func(i int) string { return "hit" }, "\n", true),
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        testQueryHit,
					ContextLines: 1,
					MaxMatches:   0,
				}
			},
			wantMatches: 10,
			wantReached: true,
			assert: func(t *testing.T, out *FindTextOut) {
				t.Helper()

				if out.AdditionalMatchesOmitted != 1 {
					t.Fatalf("AdditionalMatchesOmitted: want 1 got %d", out.AdditionalMatchesOmitted)
				}
				assertMatchRange(t, out.Matches[9], 10, 1, 10, 4)
			},
		},
		{
			name:    "non_empty_file_no_matches_returns_empty",
			initial: testTextAB,
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        testQueryNope,
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 0,
			wantReached: false,
		},
		{
			name:    "empty_file_returns_empty_deterministically",
			initial: testTextEmpty,
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        testQueryLowerX,
					ContextLines: 1,
					MaxMatches:   10,
				}
			},
			wantMatches: 0,
			wantReached: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempTextFile(t, dir, "find-*.txt", tt.initial)
			args := tt.args(path)

			out, err := findText(t.Context(), args, policy)
			mustNoErr(t, err)

			if out.MatchesReturned != len(out.Matches) {
				t.Fatalf("invariant failed: MatchesReturned=%d len(Matches)=%d", out.MatchesReturned, len(out.Matches))
			}
			if out.MatchesReturned != tt.wantMatches {
				t.Fatalf("MatchesReturned: want %d, got %d", tt.wantMatches, out.MatchesReturned)
			}
			if out.ReachedMaxMatches != tt.wantReached {
				t.Fatalf("ReachedMaxMatches: want %v, got %v", tt.wantReached, out.ReachedMaxMatches)
			}
			if tt.assert != nil {
				tt.assert(t, out)
			}
		})
	}
}

func TestFindText_ErrorAndBoundaryCases(t *testing.T) {
	dir := newWorkDir(t)
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name       string
		setup      func(t *testing.T) string
		args       func(path string) FindTextArgs
		wantErrSub string
		wantIsCtx  bool
	}{
		{
			name: "invalid_queryType",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:      path,
					QueryType: "wat",
					Query:     "A",
				}
			},
			wantErrSub: "invalid queryType",
		},
		{
			name: "substring_requires_query",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:      path,
					QueryType: "substring",
					Query:     " \n\t ",
				}
			},
			wantErrSub: "query is required for queryType=substring",
		},
		{
			name: "regex_requires_query",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:      path,
					QueryType: findTypeRegex,
					Query:     " \n\t ",
				}
			},
			wantErrSub: "query is required for queryType=regex",
		},
		{
			name: "regex_compile_error",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:      path,
					QueryType: findTypeRegex,
					Query:     "(",
				}
			},
			wantErrSub: "error parsing regexp",
		},
		{
			name: "regex_zero_length_matches_only",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:      path,
					QueryType: findTypeRegex,
					Query:     "^",
				}
			},
			wantErrSub: "regex matched only empty strings",
		},
		{
			name: "contextLines_too_large",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        "A",
					ContextLines: 2001,
				}
			},
			wantErrSub: "contextLines too large",
		},
		{
			name: "maxMatches_too_large",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:       path,
					Query:      "A",
					MaxMatches: 501,
				}
			},
			wantErrSub: "maxMatches too large",
		},
		{
			name: "response_too_large_guard",
			setup: func(t *testing.T) string {
				t.Helper()
				content := makeNLines(5000, func(i int) string {
					if i == 2500 {
						return "HIT"
					}
					return "x"
				}, "\n", true)
				return writeTempTextFile(t, dir, "big-*.txt", content)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:         path,
					Query:        "HIT",
					ContextLines: 2000,
					MaxMatches:   10,
				}
			},
			wantErrSub: "response too large",
		},
		{
			name: "file_not_found",
			setup: func(t *testing.T) string {
				t.Helper()
				return dir + string(filepathSep()) + testMissingFileName
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:  path,
					Query: "A",
				}
			},
			wantErrSub: "",
		},
		{
			name: "invalid_utf8_rejected",
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempBytesFile(t, dir, "bad-*.txt", []byte{0xff, 0xfe, 0xfd})
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:  path,
					Query: "A",
				}
			},
			wantErrSub: "not valid UTF-8",
		},
		{
			name: "symlink_file_rejected",
			setup: func(t *testing.T) string {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink behavior is platform/privilege-dependent on Windows")
				}
				target := writeTempTextFile(t, dir, "target-*.txt", testTextAOnly)
				link := dir + string(filepathSep()) + "link-find.txt"
				if err := osSymlink(target, link); err != nil {
					t.Skipf("os.Symlink not available: %v", err)
				}
				abs, err := filepathAbs(link)
				if err != nil {
					t.Fatalf("Abs(%q): %v", link, err)
				}
				return abs
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:  path,
					Query: "A",
				}
			},
			wantErrSub: "symlink",
		},
		{
			name: testNameContextCanceled,
			setup: func(t *testing.T) string {
				t.Helper()
				return writeTempTextFile(t, dir, "x-*.txt", testTextAOnly)
			},
			args: func(path string) FindTextArgs {
				return FindTextArgs{
					Path:  path,
					Query: "A",
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

			_, err := findText(ctx, args, policy)
			if tt.wantIsCtx {
				if err == nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
				return
			}
			if tt.wantErrSub == "" {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			mustErrContains(t, err, tt.wantErrSub)
		})
	}
}

func assertMatchRange(
	t *testing.T,
	got FindTextMatch,
	wantStartLine, wantStartCol, wantEndLine, wantEndCol int,
) {
	t.Helper()
	if got.MatchStartLine != wantStartLine ||
		got.MatchStartColumn != wantStartCol ||
		got.MatchEndLine != wantEndLine ||
		got.MatchEndColumn != wantEndCol {
		t.Fatalf(
			"match range: want %d:%d..%d:%d got %d:%d..%d:%d",
			wantStartLine,
			wantStartCol,
			wantEndLine,
			wantEndCol,
			got.MatchStartLine,
			got.MatchStartColumn,
			got.MatchEndLine,
			got.MatchEndColumn,
		)
	}
}

func assertContextLine(t *testing.T, got FindTextLine, wantLineNumber int, wantText string) {
	t.Helper()
	if got.LineNumber != wantLineNumber || got.Text != wantText {
		t.Fatalf(
			"context line: want {lineNumber:%d text:%q} got {lineNumber:%d text:%q}",
			wantLineNumber,
			wantText,
			got.LineNumber,
			got.Text,
		)
	}
}

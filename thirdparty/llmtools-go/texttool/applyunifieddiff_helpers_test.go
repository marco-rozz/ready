package texttool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/spec"
)

func TestApplyUnifiedDiffExplicitTargetPathVariants(t *testing.T) {
	file := parsedPatchFile{FileKey: "file-1", OldPath: "old.txt", NewPath: "new.txt"}

	tests := []struct {
		name   string
		target ApplyUnifiedDiffFileTarget
	}{
		{
			name:   "new_path_only_matches_new_path",
			target: ApplyUnifiedDiffFileTarget{NewPath: "new.txt", TargetPath: "actual.txt"},
		},
		{
			name:   "old_path_only_matches_old_path",
			target: ApplyUnifiedDiffFileTarget{OldPath: "old.txt", TargetPath: "actual.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := findExplicitTarget(file, []ApplyUnifiedDiffFileTarget{tt.target})
			mustNoErr(t, err)
			if !ok || got.TargetPath != "actual.txt" {
				t.Fatalf("explicit path variant match failed: %#v ok=%v", got, ok)
			}
		})
	}
}

func TestApplyUnifiedDiffValidationHelpers(t *testing.T) {
	tests := []struct {
		name       string
		args       ApplyUnifiedDiffArgs
		wantErrSub string
	}{
		{
			name:       "diffText_required",
			args:       ApplyUnifiedDiffArgs{},
			wantErrSub: "diffText is required",
		},
		{
			name:       "diffText_must_be_utf8",
			args:       ApplyUnifiedDiffArgs{DiffText: string([]byte{0xff, 0xfe})},
			wantErrSub: "not valid UTF-8",
		},
		{
			name:       "diffText_too_large",
			args:       ApplyUnifiedDiffArgs{DiffText: strings.Repeat("x", hardUnifiedDiffBytes+1)},
			wantErrSub: "diffText too large",
		},
		{
			name: "too_many_fileTargets",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: make([]ApplyUnifiedDiffFileTarget, hardUnifiedDiffTargets+1),
			},
			wantErrSub: "too many fileTargets",
		},
		{
			name: "too_many_candidatePaths",
			args: ApplyUnifiedDiffArgs{
				DiffText:       "diff --git a/a b/a\n",
				CandidatePaths: make([]string, hardUnifiedDiffCandidates+1),
			},
			wantErrSub: "too many candidatePaths",
		},
		{
			name: "fileTarget_targetPath_required",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{{FileKey: "file-1"}},
			},
			wantErrSub: "fileTargets[0].targetPath is required",
		},
		{
			name: "fileTarget_targetPath_must_not_contain_nul",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{{FileKey: "file-1", TargetPath: "bad\x00path"}},
			},
			wantErrSub: "contains NUL byte",
		},
		{
			name: "fileTarget_fileKey_must_not_contain_nul",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{{FileKey: "file-\x001", TargetPath: "a.txt"}},
			},
			wantErrSub: "fileTargets[0].fileKey contains NUL byte",
		},
		{
			name: "fileTarget_oldPath_must_not_contain_nul",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{{OldPath: "bad\x00path", TargetPath: "a.txt"}},
			},
			wantErrSub: "fileTargets[0].oldPath contains NUL byte",
		},
		{
			name: "fileTarget_newPath_must_not_contain_nul",
			args: ApplyUnifiedDiffArgs{
				DiffText:    "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{{NewPath: "bad\x00path", TargetPath: "a.txt"}},
			},
			wantErrSub: "fileTargets[0].newPath contains NUL byte",
		},
		{
			name: "duplicate_fileKey_rejected",
			args: ApplyUnifiedDiffArgs{
				DiffText: "diff --git a/a b/a\n",
				FileTargets: []ApplyUnifiedDiffFileTarget{
					{FileKey: "file-1", TargetPath: "a.txt"},
					{FileKey: "file-1", TargetPath: "b.txt"},
				},
			},
			wantErrSub: "duplicate fileTargets fileKey",
		},
		{
			name: "candidatePath_must_not_be_empty",
			args: ApplyUnifiedDiffArgs{
				DiffText:       "diff --git a/a b/a\n",
				CandidatePaths: []string{" "},
			},
			wantErrSub: "candidatePaths[0] must not be empty",
		},
		{
			name: "candidatePath_must_not_contain_nul",
			args: ApplyUnifiedDiffArgs{
				DiffText:       "diff --git a/a b/a\n",
				CandidatePaths: []string{"bad\x00path"},
			},
			wantErrSub: "candidatePaths[0] contains NUL byte",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateApplyUnifiedDiffArgs(tt.args)
			mustErrContains(t, err, tt.wantErrSub)
		})
	}
}

func TestApplyUnifiedDiffParseHelpers(t *testing.T) {
	t.Run("diff_path_token_and_git_path_parsing", func(t *testing.T) {
		oldPath, newPath := parseGitDiffPaths(`"a/foo bar.txt" "b/foo bar.txt"`)
		if oldPath != "foo bar.txt" || newPath != "foo bar.txt" {
			t.Fatalf("parseGitDiffPaths stripped paths incorrectly: got %q/%q", oldPath, newPath)
		}

		if got := parseDiffPathToken("foo.txt\tignored"); got != "foo.txt" {
			t.Fatalf("parseDiffPathToken with tab suffix: want %q got %q", "foo.txt", got)
		}
		if got := parseDiffPathToken(pathDevNull); got != pathDevNull {
			t.Fatalf("parseDiffPathToken /dev/null: want %q got %q", pathDevNull, got)
		}

		first, remaining := readDiffPathToken(`"quoted path.txt" trailing`)
		if first != "quoted path.txt" || remaining != "trailing" {
			t.Fatalf("readDiffPathToken quoted: got %q / %q", first, remaining)
		}

		first, remaining = readDiffPathToken(`"unterminated\`)
		if first != `unterminated\` || remaining != "" {
			t.Fatalf("readDiffPathToken fallback: got %q / %q", first, remaining)
		}
	})

	t.Run("normalize_unified_header_path_pair_only_strips_git_style_pairs", func(t *testing.T) {
		oldPath, newPath := normalizeUnifiedHeaderPathPair("a/plain.txt", "b/plain.txt")
		if oldPath != "plain.txt" || newPath != "plain.txt" {
			t.Fatalf("expected git-style prefixes to be stripped, got %q/%q", oldPath, newPath)
		}

		oldPath, newPath = normalizeUnifiedHeaderPathPair("a/plain.txt", "a/plain.txt")
		if oldPath != "a/plain.txt" || newPath != "a/plain.txt" {
			t.Fatalf("plain paths must not be stripped, got %q/%q", oldPath, newPath)
		}

		oldPath, newPath = normalizeUnifiedHeaderPathPair(pathDevNull, "b/new.txt")
		if oldPath != pathDevNull || newPath != "new.txt" {
			t.Fatalf("dev/null pairing must strip new prefix only, got %q/%q", oldPath, newPath)
		}
	})

	t.Run("hunk_header_parsing_and_diagnostics", func(t *testing.T) {
		standard, diag, hasDiag := parseHunkHeader("@@ -12,3 +34,2 @@ function")
		if hasDiag || diag != (ApplyUnifiedDiffDiagnostic{}) ||
			standard.OldStart != 12 || standard.OldCount != 3 ||
			standard.NewStart != 34 || standard.NewCount != 2 {
			t.Fatalf("unexpected standard header parse: %#v diag=%#v hasDiag=%v", standard, diag, hasDiag)
		}

		loose, diag, hasDiag := parseHunkHeader("@@ 12 34 @@")
		if !hasDiag ||
			diag.Level != ApplyUnifiedDiffDiagnosticLevelWarning ||
			diag.Code != "non_standard_hunk_header" ||
			!strings.Contains(diag.Message, "non-standard hunk header") ||
			loose.OldStart != 12 || loose.OldCount != 1 ||
			loose.NewStart != 34 || loose.NewCount != 1 {
			t.Fatalf("unexpected loose header parse: %#v diag=%#v hasDiag=%v", loose, diag, hasDiag)
		}

		malformed, diag, hasDiag := parseHunkHeader("@@ broken @@")
		if !hasDiag ||
			diag.Level != ApplyUnifiedDiffDiagnosticLevelWarning ||
			diag.Code != "malformed_hunk_header" ||
			!strings.Contains(diag.Message, "malformed hunk header") ||
			malformed.OldCount != -1 || malformed.NewCount != -1 {
			t.Fatalf("unexpected malformed header parse: %#v diag=%#v hasDiag=%v", malformed, diag, hasDiag)
		}
	})

	t.Run("file_mode_and_no_newline_markers", func(t *testing.T) {
		perm, ok := parseGitFileModePerm("new file mode 100644")
		if !ok || perm != 0o644 {
			t.Fatalf("parseGitFileModePerm: want 0644 true, got %#o %v", perm, ok)
		}
		if _, ok := parseGitFileModePerm("new file mode not-octal"); ok {
			t.Fatalf("parseGitFileModePerm should reject invalid mode")
		}

		tests := []struct {
			name     string
			kind     byte
			wantOld  bool
			wantNew  bool
			wantPat  bool
			wantFile bool
		}{
			{
				name:     "plus_sets_new_no_final_newline",
				kind:     '+',
				wantOld:  false,
				wantNew:  true,
				wantPat:  false,
				wantFile: false,
			},
			{
				name:     "minus_sets_old_no_final_newline",
				kind:     '-',
				wantOld:  true,
				wantNew:  false,
				wantPat:  true,
				wantFile: true,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				file := &parsedPatchFile{}
				hunk := &parsedHunk{Lines: []parsedHunkLine{{Kind: tt.kind, Text: "X"}}}
				markNoNewlineAtEndOfFile(file, hunk)

				if got := hunk.Lines[0].NoNewlineAtEOF; got != true {
					t.Fatalf("hunk line should be marked no-newline-at-eof")
				}

				if (file.OldNoFinalNewline != nil) != tt.wantOld || (file.NewNoFinalNewline != nil) != tt.wantNew {
					t.Fatalf(
						"unexpected file newline markers: old=%v new=%v",
						file.OldNoFinalNewline != nil,
						file.NewNoFinalNewline != nil,
					)
				}

				if file.OldNoFinalNewline != nil && *file.OldNoFinalNewline != tt.wantOld {
					t.Fatalf("OldNoFinalNewline mismatch: got %v want %v", *file.OldNoFinalNewline, tt.wantOld)
				}
				if file.NewNoFinalNewline != nil && *file.NewNoFinalNewline != tt.wantNew {
					t.Fatalf("NewNoFinalNewline mismatch: got %v want %v", *file.NewNoFinalNewline, tt.wantNew)
				}

				gotPatched := patchedFileHasFinalNewline(true, 0, *file, []string{"X"})
				gotNew := newFileHasFinalNewline(*file, []string{"X"})
				if gotPatched != tt.wantPat || gotNew != tt.wantFile {
					t.Fatalf("unexpected final-newline helpers: patched=%v new=%v", gotPatched, gotNew)
				}
			})
		}
	})
}

func TestApplyUnifiedDiffTargetHelpers(t *testing.T) {
	t.Run("explicit_dev_null_create_is_create_like_even_with_context_lines", func(t *testing.T) {
		file := parsedPatchFile{
			OldPath: pathDevNull,
			NewPath: "new.txt",
			Hunks: []parsedHunk{
				{
					Lines: []parsedHunkLine{
						{Kind: ' ', Text: "alpha"},
						{Kind: ' ', Text: "beta"},
					},
				},
			},
		}

		if !file.isCreateLike() {
			t.Fatalf("expected explicit /dev/null patch to be create-like")
		}
		if !file.canCreateWhenMissing() {
			t.Fatalf("expected explicit /dev/null patch to create when missing")
		}
	})

	t.Run("patch_path_candidates", func(t *testing.T) {
		tests := []struct {
			name string
			file parsedPatchFile
			want []string
		}{
			{
				name: "create_like_uses_new_path",
				file: parsedPatchFile{OldPath: pathDevNull, NewPath: "new.txt"},
				want: []string{"new.txt"},
			},
			{
				name: "delete_like_uses_old_path",
				file: parsedPatchFile{OldPath: "old.txt", NewPath: pathDevNull},
				want: []string{"old.txt"},
			},
			{
				name: "normal_returns_new_then_old",
				file: parsedPatchFile{OldPath: "old.txt", NewPath: "new.txt"},
				want: []string{"new.txt", "old.txt"},
			},
			{
				name: "rename_prefers_old_path_when_metadata_is_ignored",
				file: parsedPatchFile{OldPath: "old.txt", NewPath: "new.txt", IsRename: true},
				want: []string{"old.txt", "new.txt"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := patchPathCandidates(tt.file)
				if len(got) != len(tt.want) {
					t.Fatalf("len: want %d got %d (%v)", len(tt.want), len(got), got)
				}
				for i := range tt.want {
					if got[i] != tt.want[i] {
						t.Fatalf("got[%d]=%q want %q (%v)", i, got[i], tt.want[i], got)
					}
				}
			})
		}
	})

	t.Run("explicit_target_matching", func(t *testing.T) {
		file := parsedPatchFile{FileKey: "file-1", OldPath: "old.txt", NewPath: "new.txt"}

		got, ok, err := findExplicitTarget(
			file,
			[]ApplyUnifiedDiffFileTarget{{FileKey: "file-1", TargetPath: "actual.txt"}},
		)
		mustNoErr(t, err)
		if !ok || got.TargetPath != "actual.txt" {
			t.Fatalf("fileKey match failed: %#v ok=%v", got, ok)
		}

		got, ok, err = findExplicitTarget(
			file,
			[]ApplyUnifiedDiffFileTarget{{OldPath: "old.txt", NewPath: "new.txt", TargetPath: "actual.txt"}},
		)
		mustNoErr(t, err)
		if !ok || got.TargetPath != "actual.txt" {
			t.Fatalf("old/new path match failed: %#v ok=%v", got, ok)
		}

		_, ok, err = findExplicitTarget(file, []ApplyUnifiedDiffFileTarget{
			{OldPath: "old.txt", TargetPath: "one.txt"},
			{OldPath: "old.txt", TargetPath: "two.txt"},
		})
		if err == nil || ok {
			t.Fatalf("expected duplicate explicit targets to error, got ok=%v err=%v", ok, err)
		}
		mustErrContains(t, err, "multiple fileTargets match")
	})

	t.Run("candidate_path_matching", func(t *testing.T) {
		cases := []struct {
			name          string
			patchPaths    []string
			infos         []candidatePathInfo
			requireExists bool
			want          []string
		}{
			{
				name:       "exact_match",
				patchPaths: []string{"nested/target.txt"},
				infos: []candidatePathInfo{{
					Path:         "nested/target.txt",
					ResolvedPath: "nested/target.txt",
					NormPath:     normalizePathForCompare("nested/target.txt"),
					NormResolved: normalizePathForCompare("nested/target.txt"),
					BasePath:     basenameForCompare("nested/target.txt"),
					BaseResolved: basenameForCompare("nested/target.txt"),
					Exists:       true,
				}},
				requireExists: true,
				want:          []string{"nested/target.txt"},
			},
			{
				name:       "suffix_match",
				patchPaths: []string{"prefix/target.txt"},
				infos: []candidatePathInfo{{
					Path:         "other/prefix/target.txt",
					ResolvedPath: "other/prefix/target.txt",
					NormPath:     normalizePathForCompare("other/prefix/target.txt"),
					NormResolved: normalizePathForCompare("other/prefix/target.txt"),
					BasePath:     basenameForCompare("other/prefix/target.txt"),
					BaseResolved: basenameForCompare("other/prefix/target.txt"),
					Exists:       true,
				}},
				requireExists: true,
				want:          []string{"other/prefix/target.txt"},
			},
			{
				name:       "basename_match",
				patchPaths: []string{"src/target.txt"},
				infos: []candidatePathInfo{{
					Path:         "other/target.txt",
					ResolvedPath: "other/target.txt",
					NormPath:     normalizePathForCompare("other/target.txt"),
					NormResolved: normalizePathForCompare("other/target.txt"),
					BasePath:     basenameForCompare("other/target.txt"),
					BaseResolved: basenameForCompare("other/target.txt"),
					Exists:       true,
				}},
				requireExists: true,
				want:          []string{"other/target.txt"},
			},
		}

		for _, tt := range cases {
			t.Run(tt.name, func(t *testing.T) {
				got := matchCandidatePaths(tt.patchPaths, tt.infos, tt.requireExists)
				if len(got) != len(tt.want) {
					t.Fatalf("len: want %d got %d (%v)", len(tt.want), len(got), got)
				}
				for i := range tt.want {
					if got[i] != tt.want[i] {
						t.Fatalf("got[%d]=%q want %q (%v)", i, got[i], tt.want[i], got)
					}
				}
			})
		}

		if got := matchCandidatePaths(
			[]string{"nested/target.txt"},
			[]candidatePathInfo{
				{Path: "nested/target.txt", NormPath: normalizePathForCompare("nested/target.txt"), Exists: false},
			},
			true,
		); got != nil {
			t.Fatalf("requireExists should exclude missing candidates, got %v", got)
		}
	})
}

func TestApplyUnifiedDiffStatusHelpers(t *testing.T) {
	t.Run("filePlanAction_mutates", func(t *testing.T) {
		tests := map[filePlanAction]bool{
			filePlanActionNoop:          false,
			filePlanActionCreate:        true,
			filePlanActionWriteExisting: true,
			filePlanActionChmodExisting: true,

			filePlanActionDelete: true,
		}
		for action, want := range tests {
			if got := action.mutates(); got != want {
				t.Fatalf("%s.mutates(): want %v got %v", action, want, got)
			}
		}
	})

	t.Run("summaries_and_reusable_targets", func(t *testing.T) {
		files := []ApplyUnifiedDiffFileOut{
			{
				FileKey:             "file-1",
				TargetPath:          "a.txt",
				Hunks:               2,
				AppliedHunks:        1,
				AlreadyAppliedHunks: 0,
				AddedLines:          3,
				DeletedLines:        1,
			},
			{
				FileKey:             "file-2",
				TargetPath:          "",
				Hunks:               1,
				AppliedHunks:        0,
				AlreadyAppliedHunks: 1,
				AddedLines:          1,
				DeletedLines:        0,
			},
		}

		sum := summarizeApplyUnifiedDiffFiles(files)
		if sum.Files != 2 || sum.Hunks != 3 || sum.AppliedHunks != 1 || sum.AlreadyAppliedHunks != 1 ||
			sum.AddedLines != 4 ||
			sum.DeletedLines != 1 {
			t.Fatalf("unexpected summary: %#v", sum)
		}

		targets := reusableFileTargets(files)
		if len(targets) != 1 || targets[0].TargetPath != "a.txt" || targets[0].FileKey != "file-1" {
			t.Fatalf("unexpected reusable targets: %#v", targets)
		}
	})

	t.Run("aggregate_planned_status", func(t *testing.T) {
		if ok, status, msg := aggregatePlannedStatus(
			nil,
			false,
		); ok || status != ApplyUnifiedDiffStatusError ||
			msg != errNoFilesWereProcessed {
			t.Fatalf("empty planned aggregate mismatch: ok=%v status=%s msg=%q", ok, status, msg)
		}

		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusNeedsInfo}},
			false,
		); ok ||
			status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("needs-info planned aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusError}},
			false,
		); ok ||
			status != ApplyUnifiedDiffStatusError {
			t.Fatalf("error planned aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusConflict}},
			false,
		); ok ||
			status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("conflict planned aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusAlreadyApplied, AlreadyAppliedHunks: 1}},
			false,
		); !ok ||
			status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("already-applied planned aggregate mismatch: ok=%v status=%s", ok, status)
		}
		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusAlreadyApplied}},
			false,
		); !ok ||
			status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("no-hunk already-applied planned aggregate mismatch: ok=%v status=%s", ok, status)
		}
		if ok, status, _ := aggregatePlannedStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusApplicable, AppliedHunks: 1}},
			true,
		); !ok ||
			status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("dry-run applicable planned aggregate mismatch: ok=%v status=%s", ok, status)
		}
	})

	t.Run("aggregate_final_status", func(t *testing.T) {
		if ok, status, msg := aggregateFinalStatus(
			nil,
		); ok || status != ApplyUnifiedDiffStatusError ||
			msg != errNoFilesWereProcessed {
			t.Fatalf("empty final aggregate mismatch: ok=%v status=%s msg=%q", ok, status, msg)
		}

		if ok, status, _ := aggregateFinalStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusApplied}},
		); !ok ||
			status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("applied final aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, _ := aggregateFinalStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusApplicable}},
		); !ok ||
			status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("applicable final aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, _ := aggregateFinalStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusAlreadyApplied}},
		); !ok ||
			status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("already-applied final aggregate mismatch: ok=%v status=%s", ok, status)
		}

		if ok, status, msg := aggregateFinalStatus(
			[]ApplyUnifiedDiffFileOut{
				{Status: ApplyUnifiedDiffStatusApplied},
				{Status: ApplyUnifiedDiffStatusConflict},
			},
		); ok || status != ApplyUnifiedDiffStatusConflict ||
			!strings.Contains(msg, "partially applied") {
			t.Fatalf("partial final aggregate mismatch: ok=%v status=%s msg=%q", ok, status, msg)
		}

		if ok, status, _ := aggregateFinalStatus(
			[]ApplyUnifiedDiffFileOut{{Status: ApplyUnifiedDiffStatusError}},
		); ok ||
			status != ApplyUnifiedDiffStatusError {
			t.Fatalf("error final aggregate mismatch: ok=%v status=%s", ok, status)
		}
	})

	t.Run("duplicate_mutating_targets_are_marked_conflict_and_noop", func(t *testing.T) {
		plans := []fileApplyPlan{
			{
				Action:       filePlanActionWriteExisting,
				DisplayPath:  "one.txt",
				ResolvedPath: "same.txt",
				Result: ApplyUnifiedDiffFileOut{
					FileKey:    "file-1",
					Status:     ApplyUnifiedDiffStatusApplicable,
					TargetPath: "same.txt",
				},
			},
			{
				Action:       filePlanActionCreate,
				DisplayPath:  "two.txt",
				ResolvedPath: "same.txt",
				Result: ApplyUnifiedDiffFileOut{
					FileKey:    "file-2",
					Status:     ApplyUnifiedDiffStatusApplicable,
					TargetPath: "same.txt",
				},
			},
		}
		files := []ApplyUnifiedDiffFileOut{plans[0].Result, plans[1].Result}

		if !hasExecutableFilePlans(plans, files) {
			t.Fatalf("expected executable plans before conflict marking")
		}

		markDuplicateMutableTargets(plans, files)

		if hasExecutableFilePlans(plans, files) {
			t.Fatalf("duplicate targets should remove executable plans")
		}
		for i, file := range files {
			if file.Status != ApplyUnifiedDiffStatusConflict {
				t.Fatalf("files[%d] status: want conflict got %s", i, file.Status)
			}
			if len(file.Diagnostics) == 0 ||
				file.Diagnostics[0].Code != "duplicate_target" ||
				file.Diagnostics[0].Level != ApplyUnifiedDiffDiagnosticLevelError {
				t.Fatalf("files[%d] diagnostics missing duplicate target note: %#v", i, file.Diagnostics)
			}
		}
		if plans[0].Action != filePlanActionNoop || plans[1].Action != filePlanActionNoop {
			t.Fatalf("duplicate marking should noop both plans: %#v", plans)
		}
	})
}

func TestApplyUnifiedDiffPostApplyVerification(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)
	p := tt.snapshotPolicy()

	t.Run("create_plan_must_leave_file_on_disk", func(t *testing.T) {
		missingPath := filepath.Join(dir, "missing-after-create.txt")
		err := verifyExecutedFilePlan(
			p,
			fileApplyPlan{
				Action:       filePlanActionCreate,
				ResolvedPath: missingPath,
				Content:      "created\n",
				Perm:         0o644,
			},
		)
		if err == nil || !strings.Contains(err.Error(), "created file is not present on disk") {
			t.Fatalf("expected missing created-file verification error, got %v", err)
		}
	})

	t.Run("create_end_to_end_resolved_path_exists", func(t *testing.T) {
		rel := "verify-created-on-disk.txt"
		diff := makePatchText(
			"--- /dev/null",
			"+++ b/"+rel,
			"@@ -0,0 +1,2 @@",
			"+created line one",
			"+created line two",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || out.Files[0].ResolvedPath == "" {
			t.Fatalf("created file should report resolved path: %#v", out.Files)
		}
		if _, err := os.Stat(out.Files[0].ResolvedPath); err != nil {
			t.Fatalf("created file should exist at reported resolved path %q: %v", out.Files[0].ResolvedPath, err)
		}
		if got := readFileString(t, out.Files[0].ResolvedPath); got != "created line one\ncreated line two\n" {
			t.Fatalf("created file content mismatch: got %q", got)
		}
	})
}

func TestTextToolConstructorAccessorsAndWrapper(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	canonicalize := func(p string) string {
		p = filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		return p
	}

	if got, want := canonicalize(tt.snapshotPolicy().WorkBaseDir()), canonicalize(dir); got != want {
		t.Fatalf("snapshot policy work dir: want %q got %q", want, got)
	}
	if !tt.snapshotPolicy().BlockSymlinks() {
		t.Fatalf("snapshot policy should have block symlinks enabled")
	}

	t.Run("tool_accessors_return_expected_clones", func(t *testing.T) {
		tools := []spec.Tool{
			tt.DeleteTextTool(),
			tt.FindTextTool(),
			tt.InsertTextTool(),
			tt.ReadTextRangeTool(),
			tt.ReplaceTextTool(),
			tt.ApplyUnifiedDiffTool(),
		}
		wantSlugs := []string{
			"deletetext",
			"findtext",
			"inserttext",
			"readtextrange",
			"replacetext",
			"applyunifieddiff",
		}
		for i, tool := range tools {
			if tool.Slug != wantSlugs[i] {
				t.Fatalf("tool %d slug: want %q got %q", i, wantSlugs[i], tool.Slug)
			}
		}

		_ = tt.ApplyUnifiedDiffTool()

		if fresh := tt.ApplyUnifiedDiffTool(); fresh.Slug == "mutated" {
			t.Fatalf("ApplyUnifiedDiffTool should return an independent clone")
		}
	})

	t.Run("canceled_context_is_returned_by_apply_unified_diff_wrapper", func(t *testing.T) {
		path := writeTextFile(t, dir, "canceled.txt", "A\nB\n")
		diff := makePatchText(
			"--- a/canceled.txt",
			"+++ b/canceled.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := tt.ApplyUnifiedDiff(ctx, ApplyUnifiedDiffArgs{DiffText: diff})
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		if got := readFileString(t, path); got != "A\nB\n" {
			t.Fatalf("canceled context must not modify file: got %q", got)
		}
	})
}

func writeTextFile(t *testing.T, root, rel, content string) string {
	t.Helper()

	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", full, err)
	}
	return full
}

func makePatchText(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func mustNewTextTool(t *testing.T, workBaseDir string) *TextTool {
	t.Helper()

	tt, err := NewTextTool(
		WithWorkBaseDir(workBaseDir),
		WithBlockSymlinks(true),
	)
	mustNoErr(t, err)
	return tt
}

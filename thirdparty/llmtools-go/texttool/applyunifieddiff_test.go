package texttool

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestApplyUnifiedDiffEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	t.Run("plain_header_dry_run_apply_and_reapply", func(t *testing.T) {
		path := writeTextFile(t, dir, "alpha.txt", "A\nB\nC\n")
		diff := makePatchText(
			"--- a/alpha.txt",
			"+++ b/alpha.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff, DryRun: true})
		mustNoErr(t, err)
		if !out.OK || !out.DryRun || out.Status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("dry run status mismatch: ok=%v dryRun=%v status=%s", out.OK, out.DryRun, out.Status)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("dry run file status mismatch: %#v", out.Files)
		}
		if len(out.FileTargets) != 1 || out.FileTargets[0].TargetPath != "alpha.txt" {
			t.Fatalf("dry run file targets mismatch: %#v", out.FileTargets)
		}
		if got := readFileString(t, path); got != "A\nB\nC\n" {
			t.Fatalf("dry run should not modify file: got %q", got)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if !out.OK || out.Status != ApplyUnifiedDiffStatusApplied || out.DryRun {
			t.Fatalf("apply status mismatch: ok=%v dryRun=%v status=%s", out.OK, out.DryRun, out.Status)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("apply content mismatch: got %q", got)
		}
		if out.Summary.Files != 1 || out.Summary.Hunks != 1 || out.Summary.AppliedHunks != 1 ||
			out.Summary.AlreadyAppliedHunks != 0 ||
			out.Summary.AddedLines != 1 ||
			out.Summary.DeletedLines != 1 {
			t.Fatalf("apply summary mismatch: %#v", out.Summary)
		}
		if len(out.FileTargets) != 1 || out.FileTargets[0].TargetPath != "alpha.txt" {
			t.Fatalf("apply file targets mismatch: %#v", out.FileTargets)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied || !out.OK {
			t.Fatalf("reapply status mismatch: ok=%v status=%s", out.OK, out.Status)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusAlreadyApplied ||
			out.Files[0].AlreadyAppliedHunks != 1 {
			t.Fatalf("reapply file status mismatch: %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("reapply should not change file: got %q", got)
		}
	})

	t.Run("fuzzy_trimmed_match_works_and_strict_fails", func(t *testing.T) {
		path := writeTextFile(t, dir, "trimmed.txt", "A\n  B  \nC\n")
		diff := makePatchText(
			"--- a/trimmed.txt",
			"+++ b/trimmed.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("fuzzy apply status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("fuzzy apply content mismatch: got %q", got)
		}

		strictPath := writeTextFile(t, dir, "strict.txt", "A\n  B  \nC\n")
		strictDiff := makePatchText(
			"--- a/strict.txt",
			"+++ b/strict.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)
		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: strictDiff, Strict: true})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("strict apply status mismatch: %s", out.Status)
		}
		if got := readFileString(t, strictPath); got != "A\n  B  \nC\n" {
			t.Fatalf("strict failure must not modify file: got %q", got)
		}
	})

	t.Run("prefers_matching_old_block_over_nearby_new_block", func(t *testing.T) {
		path := writeTextFile(t, dir, "prefer-old.txt", "A\nX\nC\nA\nB\nC\n")
		diff := makePatchText(
			"--- a/prefer-old.txt",
			"+++ b/prefer-old.txt",
			"@@ -4,3 +4,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("status: want applied got %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\nA\nX\nC\n" {
			t.Fatalf("old block should have been patched, got %q", got)
		}
	})

	t.Run("crlf_existing_file_keeps_crlf", func(t *testing.T) {
		path := writeTextFile(t, dir, "crlf.txt", "A\r\nB\r\nC\r\n")
		diff := makePatchText(
			"--- a/crlf.txt",
			"+++ b/crlf.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("status: want applied got %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\r\nX\r\nC\r\n" {
			t.Fatalf("CRLF should be preserved, got %q", got)
		}
	})
	t.Run("create_patch_creates_file_with_no_newline_marker_and_is_idempotent", func(t *testing.T) {
		if runtime.GOOS == toolutil.GOOSWindows {
			t.Skip("win mode issue")
		}
		path := filepath.Join(dir, "created.txt")
		diff := makePatchText(
			"diff --git a/created.txt b/created.txt",
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/created.txt",
			"@@ -0,0 +1,1 @@",
			"+created",
			"\\ No newline at end of file",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("create status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "created" {
			t.Fatalf("created content mismatch: got %q", got)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("create reapply status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "created" {
			t.Fatalf("create reapply should not change file: got %q", got)
		}
	})

	t.Run("llm_add_only_missing_file_without_new_file_mode_creates_file", func(t *testing.T) {
		path := filepath.Join(dir, "llm-add-only.txt")
		diff := makePatchText(
			"diff --git a/llm-add-only.txt b/llm-add-only.txt",
			"@@ -0,0 +1,2 @@",
			"+hello",
			"+world",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("add-only create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "hello\nworld\n" {
			t.Fatalf("add-only created content mismatch: got %q", got)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("add-only created file should exist on disk: %v", err)
		}
	})
	t.Run("create_patch_with_omitted_prefixes_creates_file_from_dev_null_header", func(t *testing.T) {
		path := filepath.Join(dir, "create-omitted-prefix.txt")
		diff := makePatchText(
			"diff --git a/create-omitted-prefix.txt b/create-omitted-prefix.txt",
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/create-omitted-prefix.txt",
			"@@ -0,0 +1,3 @@",
			"alpha",
			"beta",
			"gamma",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("create fallback status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "alpha\nbeta\ngamma\n" {
			t.Fatalf("create fallback content mismatch: got %q", got)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected created file to exist on disk: %v", err)
		}
	})

	t.Run("mixed_edit_order_minus_then_plus_and_plus_then_minus", func(t *testing.T) {
		cases := []struct {
			name     string
			fileName string
			diff     string
		}{
			{
				name:     "minus_then_plus",
				fileName: "order-minus-plus.txt",
				diff: makePatchText(
					"diff --git a/order-minus-plus.txt b/order-minus-plus.txt",
					"--- a/order-minus-plus.txt",
					"+++ b/order-minus-plus.txt",
					"@@ -1,3 +1,3 @@",
					" prefix anchor line",
					"-old value",
					"+new value",
					" suffix anchor line",
				),
			},
			{
				name:     "plus_then_minus",
				fileName: "order-plus-minus.txt",
				diff: makePatchText(
					"diff --git a/order-plus-minus.txt b/order-plus-minus.txt",
					"--- a/order-plus-minus.txt",
					"+++ b/order-plus-minus.txt",
					"@@ -1,3 +1,3 @@",
					" prefix anchor line",
					"+new value",
					"-old value",
					" suffix anchor line",
				),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				path := writeTextFile(t, dir, tc.fileName, "prefix anchor line\nold value\nsuffix anchor line\n")

				out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: tc.diff})
				mustNoErr(t, err)
				if out.Status != ApplyUnifiedDiffStatusApplied {
					t.Fatalf("mixed-order status mismatch: %s files=%#v", out.Status, out.Files)
				}
				if got := readFileString(t, path); got != "prefix anchor line\nnew value\nsuffix anchor line\n" {
					t.Fatalf("mixed-order content mismatch: got %q", got)
				}
			})
		}
	})

	t.Run("contiguous_mixed_edit_blocks_permutations", func(t *testing.T) {
		cases := []struct {
			name     string
			fileName string
			body     []string
		}{
			{
				name:     "plus_minus_plus_minus",
				fileName: "mixed-plus-minus-plus-minus.txt",
				body: []string{
					" prefix anchor line",
					"+new value one",
					"-old value one",
					"+new value two",
					"-old value two",
					" suffix anchor line",
				},
			},
			{
				name:     "minus_plus_minus_plus",
				fileName: "mixed-minus-plus-minus-plus.txt",
				body: []string{
					" prefix anchor line",
					"-old value one",
					"+new value one",
					"-old value two",
					"+new value two",
					" suffix anchor line",
				},
			},
			{
				name:     "plus_minus_minus_plus",
				fileName: "mixed-plus-minus-minus-plus.txt",
				body: []string{
					" prefix anchor line",
					"+new value one",
					"-old value one",
					"-old value two",
					"+new value two",
					" suffix anchor line",
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				path := writeTextFile(
					t,
					dir,
					tc.fileName,
					"prefix anchor line\nold value one\nold value two\nsuffix anchor line\n",
				)

				lines := []string{
					"diff --git a/" + tc.fileName + " b/" + tc.fileName,
					"--- a/" + tc.fileName,
					"+++ b/" + tc.fileName,
					"@@ -1,4 +1,4 @@",
				}
				lines = append(lines, tc.body...)

				out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: makePatchText(lines...)})
				mustNoErr(t, err)
				if out.Status != ApplyUnifiedDiffStatusApplied {
					t.Fatalf("status mismatch: %s files=%#v", out.Status, out.Files)
				}
				if got := readFileString(
					t,
					path,
				); got != "prefix anchor line\nnew value one\nnew value two\nsuffix anchor line\n" {
					t.Fatalf("content mismatch: got %q", got)
				}
			})
		}
	})
	t.Run("insert_only_into_existing_empty_file_adds_final_newline", func(t *testing.T) {
		path := writeTextFile(t, dir, "existing-empty.txt", "")
		diff := makePatchText(
			"diff --git a/existing-empty.txt b/existing-empty.txt",
			"--- a/existing-empty.txt",
			"+++ b/existing-empty.txt",
			"@@ -0,0 +1,1 @@",
			"+insert",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("empty-file insert status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "insert\n" {
			t.Fatalf("empty-file insert should end with newline, got %q", got)
		}
	})

	t.Run("create_patch_dry_run_rejects_too_many_new_parent_dirs", func(t *testing.T) {
		rel := "deep-new/a/b/c/d/e/f/g/h/i/too-deep.txt"
		diff := makePatchText(
			"diff --git a/"+rel+" b/"+rel,
			"--- /dev/null",
			"+++ b/"+rel,
			"@@ -0,0 +1,1 @@",
			"+x",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff, DryRun: true})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusError {
			t.Fatalf("deep create dry-run status: want error got %s files=%#v", out.Status, out.Files)
		}
		if _, err := os.Stat(filepath.Join(dir, "deep-new")); !os.IsNotExist(err) {
			t.Fatalf("dry-run must not create parent dirs, stat err=%v", err)
		}
	})

	t.Run("create_patch_conflicts_when_target_already_exists_with_different_content", func(t *testing.T) {
		path := writeTextFile(t, dir, "create-conflict.txt", "different\n")
		diff := makePatchText(
			"diff --git a/create-conflict.txt b/create-conflict.txt",
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/create-conflict.txt",
			"@@ -0,0 +1,1 @@",
			"+created",
			"\\ No newline at end of file",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("create conflict status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "different\n" {
			t.Fatalf("create conflict must not modify file: got %q", got)
		}
	})

	t.Run("already_applied_content_with_new_mode_still_updates_mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("windows chmod does not reliably expose unix executable mode bits")
		}
		path := writeTextFile(t, dir, "mode-already.txt", "A\nX\n")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod setup: %v", err)
		}
		diff := makePatchText(
			"diff --git a/mode-already.txt b/mode-already.txt",
			"old mode 100644",
			"new mode 100755",
			"--- a/mode-already.txt",
			"+++ b/mode-already.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)
		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("mode update status: want applied got %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("mode update should not change content, got %q", got)
		}
		st, err := os.Stat(path)
		mustNoErr(t, err)
		if st.Mode().Perm() != 0o755 {
			t.Fatalf("mode update mismatch: want 0755 got %#o", st.Mode().Perm())
		}
	})
	t.Run("delete_patch_deletes_file_and_is_idempotent", func(t *testing.T) {
		path := writeTextFile(t, dir, "delete.txt", "gone\n")
		diff := makePatchText(
			"diff --git a/delete.txt b/delete.txt",
			"deleted file mode 100644",
			"--- a/delete.txt",
			"+++ /dev/null",
			"@@ -1,1 +0,0 @@",
			"-gone",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("delete status mismatch: %s", out.Status)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("delete should remove file, stat err=%v", err)
		}

		_, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)

		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("delete reapply should keep file absent, stat err=%v", err)
		}
	})

	t.Run("delete_patch_conflicts_when_content_remains", func(t *testing.T) {
		path := writeTextFile(t, dir, "delete-conflict.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/delete-conflict.txt b/delete-conflict.txt",
			"deleted file mode 100644",
			"--- a/delete-conflict.txt",
			"+++ /dev/null",
			"@@ -1,1 +0,0 @@",
			"-A",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("delete conflict status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nB\n" {
			t.Fatalf("delete conflict must not modify file: got %q", got)
		}
	})

	t.Run("explicit_file_target_by_fileKey", func(t *testing.T) {
		path := writeTextFile(t, dir, "actual.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/ghost/actual.txt b/ghost/actual.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText: diff,
			FileTargets: []ApplyUnifiedDiffFileTarget{{
				FileKey:    "file-1",
				TargetPath: "actual.txt",
			}},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("explicit target status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("explicit target content mismatch: got %q", got)
		}
		if len(out.FileTargets) != 1 || out.FileTargets[0].TargetPath != "actual.txt" ||
			out.FileTargets[0].FileKey != "file-1" {
			t.Fatalf("explicit target file targets mismatch: %#v", out.FileTargets)
		}
	})

	t.Run("missing_target_without_candidates_returns_needs_info", func(t *testing.T) {
		diff := makePatchText(
			"diff --git a/ghost/missing.txt b/ghost/missing.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("missing target status mismatch: %s", out.Status)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("missing target file status mismatch: %#v", out.Files)
		}
	})

	t.Run("single_candidate_fallback_resolves_when_diff_path_is_unknown", func(t *testing.T) {
		path := writeTextFile(t, dir, "fallback.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/ghost/dir/unmatched.txt b/ghost/dir/unmatched.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{"fallback.txt"},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("single-candidate fallback status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("single-candidate fallback content mismatch: got %q", got)
		}
		if len(out.Files) != 1 || out.Files[0].TargetPath != "fallback.txt" {
			t.Fatalf("single-candidate fallback target mismatch: %#v", out.Files)
		}
	})

	t.Run("candidate_paths_resolve_by_basename", func(t *testing.T) {
		path := writeTextFile(t, dir, "nested/target.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/ghost/dir/target.txt b/ghost/dir/target.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{"nested/target.txt"},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("candidate basename status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("candidate basename content mismatch: got %q", got)
		}
		if len(out.Files) != 1 || out.Files[0].TargetPath != "nested/target.txt" {
			t.Fatalf("candidate basename target mismatch: %#v", out.Files)
		}
	})

	t.Run("ambiguous_candidate_paths_return_needs_info", func(t *testing.T) {
		writeTextFile(t, dir, "one/dup.txt", "A\nB\n")
		writeTextFile(t, dir, "two/dup.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/ghost/dir/dup.txt b/ghost/dir/dup.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{"one/dup.txt", "two/dup.txt"},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("ambiguous candidates status mismatch: %s", out.Status)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("ambiguous candidates file status mismatch: %#v", out.Files)
		}
	})

	t.Run("duplicate_file_sections_for_same_path_are_coalesced", func(t *testing.T) {
		path := writeTextFile(t, dir, "duplicate-sections.txt", "A\nB\nC\nD\n")
		diff := makePatchText(
			"diff --git a/duplicate-sections.txt b/duplicate-sections.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
			"diff --git a/duplicate-sections.txt b/duplicate-sections.txt",
			"@@ -3,2 +3,2 @@",
			" C",
			"-D",
			"+Y",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("duplicate section status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || out.Files[0].Hunks != 2 {
			t.Fatalf("duplicate sections should be coalesced into one two-hunk file: %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\nY\n" {
			t.Fatalf("duplicate section content mismatch: got %q", got)
		}
	})

	t.Run("duplicate_mutating_targets_conflict", func(t *testing.T) {
		path := writeTextFile(t, dir, "shared.txt", "A\nB\nC\n")
		diff := makePatchText(
			"diff --git a/one.txt b/one.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
			"diff --git a/two.txt b/two.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+Y",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText: diff,
			FileTargets: []ApplyUnifiedDiffFileTarget{
				{FileKey: "file-1", TargetPath: "shared.txt"},
				{FileKey: "file-2", TargetPath: "shared.txt"},
			},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("duplicate target status mismatch: %s", out.Status)
		}
		if got := readFileString(t, path); got != "A\nB\nC\n" {
			t.Fatalf("duplicate target conflict must not modify file: got %q", got)
		}
		if len(out.Files) != 2 || out.Files[0].Status != ApplyUnifiedDiffStatusConflict ||
			out.Files[1].Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("duplicate target file statuses mismatch: %#v", out.Files)
		}
	})

	t.Run("binary_patch_is_reported_conflict", func(t *testing.T) {
		diff := makePatchText(
			"diff --git a/bin.dat b/bin.dat",
			"Binary files a/bin.dat and b/bin.dat differ",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("binary patch status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("binary patch file status mismatch: %#v", out.Files)
		}
	})

	t.Run("rename_patch_with_text_hunks_applies_to_existing_old_path_without_renaming", func(t *testing.T) {
		oldPath := writeTextFile(t, dir, "rename-old.txt", "A\n")
		newPath := filepath.Join(dir, "rename-new.txt")
		diff := makePatchText(
			"diff --git a/rename-old.txt b/rename-new.txt",
			"similarity index 50%",
			"rename from rename-old.txt",
			"rename to rename-new.txt",
			"--- a/rename-old.txt",
			"+++ b/rename-new.txt",
			"@@ -1,1 +1,1 @@",
			"-A",
			"+B",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("rename text-hunk status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, oldPath); got != "B\n" {
			t.Fatalf("rename text hunk should patch old path when new path is absent, got %q", got)
		}
		if _, err := os.Stat(newPath); !os.IsNotExist(err) {
			t.Fatalf("rename metadata must not create/rename new path, stat err=%v", err)
		}
		if len(out.Files) != 1 ||
			out.Files[0].TargetPath != "rename-old.txt" ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"rename_metadata_ignored",
			) {
			t.Fatalf("rename diagnostics/target mismatch: %#v", out.Files)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("rename text-hunk reapply status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, oldPath); got != "B\n" {
			t.Fatalf("rename text-hunk reapply should not change content, got %q", got)
		}
	})

	t.Run("rename_patch_prefers_existing_old_path_when_rename_metadata_is_ignored", func(t *testing.T) {
		oldPath := writeTextFile(t, dir, "rename-both-old.txt", "A\n")
		newPath := writeTextFile(t, dir, "rename-both-new.txt", "A\n")
		diff := makePatchText(
			"diff --git a/rename-both-old.txt b/rename-both-new.txt",
			"similarity index 50%",
			"rename from rename-both-old.txt",
			"rename to rename-both-new.txt",
			"--- a/rename-both-old.txt",
			"+++ b/rename-both-new.txt",
			"@@ -1,1 +1,1 @@",
			"-A",
			"+B",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("rename both-paths status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, oldPath); got != "B\n" {
			t.Fatalf("rename text hunk should patch old path when rename metadata is ignored, got %q", got)
		}
		if got := readFileString(t, newPath); got != "A\n" {
			t.Fatalf("rename text hunk should not patch new path when old path exists, got %q", got)
		}
		if len(out.Files) != 1 ||
			out.Files[0].TargetPath != "rename-both-old.txt" ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"rename_metadata_ignored",
			) {
			t.Fatalf("rename both-paths diagnostics/target mismatch: %#v", out.Files)
		}
	})

	t.Run("copy_patches_are_rejected", func(t *testing.T) {
		diff := makePatchText(
			"diff --git a/src.txt b/copy.txt",
			"copy from src.txt",
			"copy to copy.txt",
			"--- a/src.txt",
			"+++ b/copy.txt",
			"@@ -1,1 +1,1 @@",
			"-C",
			"+D",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("copy status mismatch: %s", out.Status)
		}
		if len(out.Files) != 1 ||
			out.Files[0].Status != ApplyUnifiedDiffStatusConflict ||
			!hasDiagnostic(out.Files[0].Diagnostics, ApplyUnifiedDiffDiagnosticLevelError, "copy_unsupported") {
			t.Fatalf("copy file status/diagnostics mismatch: %#v", out.Files)
		}
	})

	t.Run("unrecognized_text_returns_needs_info", func(t *testing.T) {
		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: "this is not a patch\n"})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusNeedsInfo ||
			len(out.Diagnostics) == 0 ||
			out.Diagnostics[0].Code != "no_patch_headers_found" ||
			out.Diagnostics[0].Level != ApplyUnifiedDiffDiagnosticLevelWarning {
			t.Fatalf("unrecognized text status/diagnostics mismatch: status=%s diags=%v", out.Status, out.Diagnostics)
		}
	})

	t.Run("metadata_only_single_file_returns_already_applied_noop", func(t *testing.T) {
		diff := makePatchText(
			"diff --git a/empty.txt b/empty.txt",
			"--- a/empty.txt",
			"+++ b/empty.txt",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied ||
			len(out.Files) != 1 ||
			out.Files[0].Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("metadata-only single-file mismatch: status=%s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("metadata_only_file_in_multi_file_diff_is_reported_already_applied", func(t *testing.T) {
		path := writeTextFile(t, dir, "multi.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/empty.txt b/empty.txt",
			"--- a/empty.txt",
			"+++ b/empty.txt",
			"diff --git a/multi.txt b/multi.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied || len(out.Files) != 2 {
			t.Fatalf("metadata-only multi-file mismatch: status=%s files=%#v", out.Status, out.Files)
		}
		if out.Files[0].Status != ApplyUnifiedDiffStatusAlreadyApplied ||
			out.Files[1].Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("metadata-only multi-file file statuses mismatch: %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("metadata-only multi-file content mismatch: got %q", got)
		}
	})
	t.Run("markdown_fenced_diff_with_prose_before_and_after", func(t *testing.T) {
		path := writeTextFile(t, dir, "fenced.md", "A\nB\nC\n")
		diff := strings.Join([]string{
			"Here is the patch:",
			"",
			"```diff",
			"diff --git a/fenced.md b/fenced.md",
			"--- a/fenced.md",
			"+++ b/fenced.md",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
			"```",
			"",
			"That should fix it.",
			"",
		}, "\n")

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("fenced diff status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("fenced diff content mismatch: got %q", got)
		}
	})

	t.Run("malformed_hunk_header_still_applies_by_body_match", func(t *testing.T) {
		path := writeTextFile(t, dir, "malformed-header.txt", "A\nB\nC\n")
		diff := makePatchText(
			"diff --git a/malformed-header.txt b/malformed-header.txt",
			"--- a/malformed-header.txt",
			"+++ b/malformed-header.txt",
			"@@ broken @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("malformed header status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || !containsDiagnostic(out.Files[0].Diagnostics, "malformed hunk header") {
			t.Fatalf("expected malformed-header diagnostic, got %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("malformed header content mismatch: got %q", got)
		}
	})

	t.Run("omitted_hunk_body_prefixes_are_context_with_diagnostic", func(t *testing.T) {
		path := writeTextFile(t, dir, "omitted-prefix.txt", "A\nB\nC\n")
		diff := makePatchText(
			"diff --git a/omitted-prefix.txt b/omitted-prefix.txt",
			"--- a/omitted-prefix.txt",
			"+++ b/omitted-prefix.txt",
			"@@ -1,3 +1,3 @@",
			"A",
			"-B",
			"+X",
			"C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("omitted-prefix status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || !containsDiagnostic(out.Files[0].Diagnostics, "without unified-diff prefixes") {
			t.Fatalf("expected omitted-prefix diagnostic, got %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("omitted-prefix content mismatch: got %q", got)
		}
	})

	t.Run("hunk_header_counts_too_small_are_warning_hints_only", func(t *testing.T) {
		path := writeTextFile(t, dir, "count-too-small.txt", "A\nB\nC\n")
		diff := makePatchText(
			"diff --git a/count-too-small.txt b/count-too-small.txt",
			"--- a/count-too-small.txt",
			"+++ b/count-too-small.txt",
			"@@ -1,1 +1,1 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("count-too-small status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("count-too-small content mismatch: got %q", got)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(out.Files[0].Diagnostics, ApplyUnifiedDiffDiagnosticLevelWarning, "hunk_body_overflow") {
			t.Fatalf("missing hunk_body_overflow warning: %#v", out.Files)
		}
	})

	t.Run("hunk_header_counts_too_large_are_warning_hints_only", func(t *testing.T) {
		path := writeTextFile(t, dir, "count-too-large.txt", "A\nB\nC\n")
		diff := makePatchText(
			"diff --git a/count-too-large.txt b/count-too-large.txt",
			"--- a/count-too-large.txt",
			"+++ b/count-too-large.txt",
			"@@ -1,99 +1,99 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("count-too-large status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\n" {
			t.Fatalf("count-too-large content mismatch: got %q", got)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"hunk_header_count_mismatch",
			) {
			t.Fatalf("missing hunk_header_count_mismatch warning: %#v", out.Files)
		}
	})

	t.Run("multi_hunk_partial_already_applied_file", func(t *testing.T) {
		path := writeTextFile(t, dir, "partial-already.txt", "A\nX\nC\nD\nE\nF\n")
		diff := makePatchText(
			"diff --git a/partial-already.txt b/partial-already.txt",
			"--- a/partial-already.txt",
			"+++ b/partial-already.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
			"@@ -4,3 +4,3 @@",
			" D",
			"-E",
			"+Y",
			" F",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("partial-already status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 ||
			out.Files[0].AppliedHunks != 1 ||
			out.Files[0].AlreadyAppliedHunks != 1 {
			t.Fatalf("partial-already hunk counts mismatch: %#v", out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\nC\nD\nY\nF\n" {
			t.Fatalf("partial-already content mismatch: got %q", got)
		}
	})

	t.Run("multi_hunk_failure_does_not_write_first_matched_hunk", func(t *testing.T) {
		path := writeTextFile(t, dir, "partial-failure-no-write.txt", "A\nB\nC\nD\nE\nF\n")
		diff := makePatchText(
			"diff --git a/partial-failure-no-write.txt b/partial-failure-no-write.txt",
			"--- a/partial-failure-no-write.txt",
			"+++ b/partial-failure-no-write.txt",
			"@@ -1,3 +1,3 @@",
			" A",
			"-B",
			"+X",
			" C",
			"@@ -4,3 +4,3 @@",
			" D",
			"-missing",
			"+Y",
			" F",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("partial failure should conflict: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nB\nC\nD\nE\nF\n" {
			t.Fatalf("partial failure must not write first hunk: got %q", got)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"matched_but_not_written",
			) {
			t.Fatalf("missing matched_but_not_written diagnostic: %#v", out.Files)
		}
	})

	t.Run("delete_patch_missing_file_is_already_applied", func(t *testing.T) {
		diff := makePatchText(
			"diff --git a/missing-delete.txt b/missing-delete.txt",
			"deleted file mode 100644",
			"--- a/missing-delete.txt",
			"+++ /dev/null",
			"@@ -1,1 +0,0 @@",
			"-gone",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied ||
			len(out.Files) != 1 ||
			out.Files[0].Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("missing delete should be already applied: status=%s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("existing_non_utf8_file_returns_error", func(t *testing.T) {
		full := filepath.Join(dir, "non-utf8.txt")
		if err := os.WriteFile(full, []byte{0xff, 0xfe, 0xfd}, 0o600); err != nil {
			t.Fatalf("write non-utf8 fixture: %v", err)
		}
		diff := makePatchText(
			"diff --git a/non-utf8.txt b/non-utf8.txt",
			"--- a/non-utf8.txt",
			"+++ b/non-utf8.txt",
			"@@ -1,1 +1,1 @@",
			"-old",
			"+new",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusError {
			t.Fatalf("non-utf8 status mismatch: want error got %s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("symlink_target_is_refused_on_unix", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires privileges on some Windows setups")
		}

		writeTextFile(t, dir, "real-symlink-target.txt", "A\nB\n")
		linkPath := filepath.Join(dir, "link-target.txt")
		if err := os.Symlink("real-symlink-target.txt", linkPath); err != nil {
			t.Fatalf("create symlink: %v", err)
		}

		diff := makePatchText(
			"diff --git a/link-target.txt b/link-target.txt",
			"--- a/link-target.txt",
			"+++ b/link-target.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusError {
			t.Fatalf("symlink target status mismatch: want error got %s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("quoted_paths_with_spaces_end_to_end", func(t *testing.T) {
		path := writeTextFile(t, dir, "path with spaces.txt", "A\nB\n")
		diff := makePatchText(
			`diff --git "a/path with spaces.txt" "b/path with spaces.txt"`,
			`--- "a/path with spaces.txt"`,
			`+++ "b/path with spaces.txt"`,
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("quoted paths status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "A\nX\n" {
			t.Fatalf("quoted paths content mismatch: got %q", got)
		}
	})

	t.Run("candidate_suffix_precedence_over_basename", func(t *testing.T) {
		basenameOnly := writeTextFile(t, dir, "other/target.txt", "A\nB\n")
		suffix := writeTextFile(t, dir, "repo/src/target.txt", "A\nB\n")
		diff := makePatchText(
			"diff --git a/src/target.txt b/src/target.txt",
			"--- a/src/target.txt",
			"+++ b/src/target.txt",
			"@@ -1,2 +1,2 @@",
			" A",
			"-B",
			"+X",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{"other/target.txt", "repo/src/target.txt"},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("suffix precedence status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, suffix); got != "A\nX\n" {
			t.Fatalf("suffix candidate should have been patched, got %q", got)
		}
		if got := readFileString(t, basenameOnly); got != "A\nB\n" {
			t.Fatalf("basename-only candidate must not be patched, got %q", got)
		}
	})

	t.Run("ambiguous_fuzzy_hunk_refuses_to_apply", func(t *testing.T) {
		path := writeTextFile(
			t,
			dir,
			"ambiguous-fuzzy.txt",
			strings.Join([]string{
				"  A  ",
				"  B  ",
				"  C  ",
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				strings.Repeat("pad", 1),
				"  A  ",
				"  B  ",
				"  C  ",
				"",
			}, "\n"),
		)
		diff := makePatchText(
			"diff --git a/ambiguous-fuzzy.txt b/ambiguous-fuzzy.txt",
			"--- a/ambiguous-fuzzy.txt",
			"+++ b/ambiguous-fuzzy.txt",
			"@@ -10,3 +10,3 @@",
			" A",
			"-B",
			"+X",
			" C",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("ambiguous fuzzy should conflict, got status=%s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); strings.Contains(got, "\nX\n") {
			t.Fatalf("ambiguous fuzzy conflict must not modify file, got %q", got)
		}
	})

	t.Run("duplicate_exact_old_block_uses_unique_near_line_hint", func(t *testing.T) {
		pad := make([]string, 0, 20)
		for range 20 {
			pad = append(pad, "pad line")
		}

		contentLines := make([]string, 0, 7+len(pad))
		contentLines = append(contentLines,
			"duplicate prefix line",
			"duplicate old line",
			"duplicate suffix line",
		)
		contentLines = append(contentLines, pad...)
		contentLines = append(contentLines,
			"duplicate prefix line",
			"duplicate old line",
			"duplicate suffix line",
			"",
		)

		path := writeTextFile(t, dir, "line-hint-disambiguates.txt", strings.Join(contentLines, "\n"))
		diff := makePatchText(
			"diff --git a/line-hint-disambiguates.txt b/line-hint-disambiguates.txt",
			"--- a/line-hint-disambiguates.txt",
			"+++ b/line-hint-disambiguates.txt",
			"@@ -24,3 +24,3 @@",
			" duplicate prefix line",
			"-duplicate old line",
			"+duplicate new line",
			" duplicate suffix line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("line-hint status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != strings.Join([]string{
			"duplicate prefix line",
			"duplicate old line",
			"duplicate suffix line",
			strings.Join(pad, "\n"),
			"duplicate prefix line",
			"duplicate new line",
			"duplicate suffix line",
			"",
		}, "\n") {
			t.Fatalf("line-hint content mismatch: got %q", got)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"exact_old_block_ambiguous_hint_selected",
			) {
			t.Fatalf("missing line-hint ambiguity diagnostic: %#v", out.Files)
		}
	})
}

func TestApplyUnifiedDiffAllowedRootEscapeReturnsError(t *testing.T) {
	parent := t.TempDir()
	allowed := filepath.Join(parent, "allowed")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed root: %v", err)
	}
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("A\nB\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	tt, err := NewTextTool(
		WithWorkBaseDir(allowed),
		WithAllowedRoots([]string{allowed}),
		WithBlockSymlinks(true),
	)
	mustNoErr(t, err)

	diff := makePatchText(
		"diff --git a/../outside.txt b/../outside.txt",
		"--- a/../outside.txt",
		"+++ b/../outside.txt",
		"@@ -1,2 +1,2 @@",
		" A",
		"-B",
		"+X",
	)

	out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
	mustNoErr(t, err)
	if out.Status != ApplyUnifiedDiffStatusError {
		t.Fatalf("allowed-root escape status mismatch: want error got %s files=%#v", out.Status, out.Files)
	}
	if got := readFileString(t, outside); got != "A\nB\n" {
		t.Fatalf("outside file must not be modified, got %q", got)
	}
}

func TestApplyUnifiedDiffModeOnlyPatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows chmod does not reliably expose unix executable mode bits")
	}

	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)
	path := writeTextFile(t, dir, "mode-only.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}

	diff := makePatchText(
		"diff --git a/mode-only.sh b/mode-only.sh",
		"old mode 100644",
		"new mode 100755",
	)

	out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
	mustNoErr(t, err)
	if out.Status != ApplyUnifiedDiffStatusApplied {
		t.Fatalf("mode-only apply status mismatch: %s files=%#v", out.Status, out.Files)
	}
	if got := readFileString(t, path); got != "#!/bin/sh\necho hi\n" {
		t.Fatalf("mode-only patch must not rewrite content, got %q", got)
	}
	st, err := os.Stat(path)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode-only chmod mismatch: want 0755 got %#o", st.Mode().Perm())
	}

	out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
	mustNoErr(t, err)
	if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
		t.Fatalf("mode-only reapply status mismatch: %s files=%#v", out.Status, out.Files)
	}

	createExisting := writeTextFile(t, dir, "create-existing-mode.sh", "echo hi\n")
	if err := os.Chmod(createExisting, 0o644); err != nil {
		t.Fatalf("chmod create-existing setup: %v", err)
	}
	createDiff := makePatchText(
		"diff --git a/create-existing-mode.sh b/create-existing-mode.sh",
		"new file mode 100755",
		"--- /dev/null",
		"+++ b/create-existing-mode.sh",
		"@@ -0,0 +1,1 @@",
		"+echo hi",
	)

	out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: createDiff})
	mustNoErr(t, err)
	if out.Status != ApplyUnifiedDiffStatusApplied {
		t.Fatalf("create-existing mode update status mismatch: %s files=%#v", out.Status, out.Files)
	}
	if got := readFileString(t, createExisting); got != "echo hi\n" {
		t.Fatalf("create-existing mode update must not change content, got %q", got)
	}
	st, err = os.Stat(createExisting)
	mustNoErr(t, err)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("create-existing mode mismatch: want 0755 got %#o", st.Mode().Perm())
	}

	out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: createDiff})
	mustNoErr(t, err)
	if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
		t.Fatalf("create-existing mode reapply status mismatch: %s files=%#v", out.Status, out.Files)
	}
}

func TestApplyUnifiedDiffSingleEditFallbackCases(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	tests := []struct {
		name              string
		fileName          string
		initialContent    string
		diff              string
		wantStatus        ApplyUnifiedDiffStatus
		wantContent       string
		wantDiagCode      string
		wantDiagLevel     ApplyUnifiedDiffDiagnosticLevel
		reapply           bool
		wantReapplyStatus ApplyUnifiedDiffStatus
	}{
		{
			name:           "unique_suffix_recovers_stale_prefix_and_reapplies",
			fileName:       "suffix-rescue.txt",
			initialContent: "alpha\nold\nunique-tail\nomega\n",
			diff: makePatchText(
				"diff --git a/suffix-rescue.txt b/suffix-rescue.txt",
				"--- a/suffix-rescue.txt",
				"+++ b/suffix-rescue.txt",
				"@@ -1,3 +1,3 @@",
				" stale-prefix",
				"-old",
				"+new",
				" unique-tail",
			),
			wantStatus:        ApplyUnifiedDiffStatusApplied,
			wantContent:       "alpha\nnew\nunique-tail\nomega\n",
			reapply:           true,
			wantReapplyStatus: ApplyUnifiedDiffStatusAlreadyApplied,
		},
		{
			name:           "unique_prefix_recovers_stale_suffix",
			fileName:       "prefix-rescue.txt",
			initialContent: "unique-head\nold\nomega\n",
			diff: makePatchText(
				"diff --git a/prefix-rescue.txt b/prefix-rescue.txt",
				"--- a/prefix-rescue.txt",
				"+++ b/prefix-rescue.txt",
				"@@ -1,3 +1,3 @@",
				" unique-head",
				"+new",
				"-old",
				" stale-suffix",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "unique-head\nnew\nomega\n",
		},
		{
			name:           "unique_prefix_with_ambiguous_suffix_uses_prefix_fallback",
			fileName:       "ambiguous-suffix.txt",
			initialContent: "prefix\nold\nx\nsuffix\nx\nsuffix\n",
			diff: makePatchText(
				"diff --git a/ambiguous-suffix.txt b/ambiguous-suffix.txt",
				"--- a/ambiguous-suffix.txt",
				"+++ b/ambiguous-suffix.txt",
				"@@ -1,3 +1,3 @@",
				" prefix",
				"-old",
				"+new",
				" suffix",
			),
			wantStatus:    ApplyUnifiedDiffStatusApplied,
			wantContent:   "prefix\nnew\nx\nsuffix\nx\nsuffix\n",
			wantDiagCode:  "single_edit_suffix_ambiguous",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "unique_suffix_with_ambiguous_prefix_uses_suffix_fallback",
			fileName:       "ambiguous-prefix.txt",
			initialContent: "anchor\nx\nanchor\nx\nold\nsuffix\n",
			diff: makePatchText(
				"diff --git a/ambiguous-prefix.txt b/ambiguous-prefix.txt",
				"--- a/ambiguous-prefix.txt",
				"+++ b/ambiguous-prefix.txt",
				"@@ -1,3 +1,3 @@",
				" anchor",
				"-old",
				"+new",
				" suffix",
			),
			wantStatus:    ApplyUnifiedDiffStatusApplied,
			wantContent:   "anchor\nx\nanchor\nx\nnew\nsuffix\n",
			wantDiagCode:  "single_edit_prefix_ambiguous",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "both_unique_compatible_applies",
			fileName:       "compatible-unique.txt",
			initialContent: "prefix\nold\nsuffix\n",
			diff: makePatchText(
				"diff --git a/compatible-unique.txt b/compatible-unique.txt",
				"--- a/compatible-unique.txt",
				"+++ b/compatible-unique.txt",
				"@@ -1,3 +1,3 @@",
				" prefix",
				"-old",
				"+new",
				" suffix",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "prefix\nnew\nsuffix\n",
		},
		{
			name:           "both_unique_incompatible_conflicts",
			fileName:       "incompatible-unique.txt",
			initialContent: "prefix\nalpha\nsuffix\n",
			diff: makePatchText(
				"diff --git a/incompatible-unique.txt b/incompatible-unique.txt",
				"--- a/incompatible-unique.txt",
				"+++ b/incompatible-unique.txt",
				"@@ -1,3 +1,3 @@",
				" prefix",
				"-old",
				"+new",
				" suffix",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "prefix\nalpha\nsuffix\n",
			wantDiagCode:  "single_edit_incompatible_context",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "both_nonunique_compatible_but_ambiguous_conflicts",
			fileName:       "ambiguous-compat.txt",
			initialContent: "anchor\nold\ntail\nanchor\nold\ntail\n",
			diff: makePatchText(
				"diff --git a/ambiguous-compat.txt b/ambiguous-compat.txt",
				"--- a/ambiguous-compat.txt",
				"+++ b/ambiguous-compat.txt",
				"@@ -1,3 +1,3 @@",
				" anchor",
				"-old",
				"+new",
				" tail",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "anchor\nold\ntail\nanchor\nold\ntail\n",
			wantDiagCode:  "single_edit_ambiguous",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "both_nonunique_incompatible_conflicts",
			fileName:       "incompatible-nonunique.txt",
			initialContent: "prefix\nalpha\nsuffix\nprefix\nbeta\nsuffix\n",
			diff: makePatchText(
				"diff --git a/incompatible-nonunique.txt b/incompatible-nonunique.txt",
				"--- a/incompatible-nonunique.txt",
				"+++ b/incompatible-nonunique.txt",
				"@@ -1,3 +1,3 @@",
				" prefix",
				"-old",
				"+new",
				" suffix",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "prefix\nalpha\nsuffix\nprefix\nbeta\nsuffix\n",
			wantDiagCode:  "single_edit_non_unique_context",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "delete_only_single_block_applies",
			fileName:       "delete-only.txt",
			initialContent: "before\nremove\nafter\n",
			diff: makePatchText(
				"diff --git a/delete-only.txt b/delete-only.txt",
				"--- a/delete-only.txt",
				"+++ b/delete-only.txt",
				"@@ -1,3 +1,2 @@",
				" before",
				"-remove",
				" after",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "before\nafter\n",
		},
		{
			name:           "insert_only_single_block_applies",
			fileName:       "insert-only.txt",
			initialContent: "before\nafter\n",
			diff: makePatchText(
				"diff --git a/insert-only.txt b/insert-only.txt",
				"--- a/insert-only.txt",
				"+++ b/insert-only.txt",
				"@@ -1,2 +1,3 @@",
				" before",
				"+insert",
				" after",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "before\ninsert\nafter\n",
		},
		{
			name:           "insert_only_already_applied_with_extra_line_before_suffix",
			fileName:       "insert-already-extra-line.txt",
			initialContent: "unique prefix anchor line\ninserted line one\ninserted line two\nintervening local line\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/insert-already-extra-line.txt b/insert-already-extra-line.txt",
				"--- a/insert-already-extra-line.txt",
				"+++ b/insert-already-extra-line.txt",
				"@@ -1,2 +1,4 @@",
				" unique prefix anchor line",
				"+inserted line one",
				"+inserted line two",
				" unique suffix anchor line",
			),
			wantStatus:    ApplyUnifiedDiffStatusAlreadyApplied,
			wantContent:   "unique prefix anchor line\ninserted line one\ninserted line two\nintervening local line\nunique suffix anchor line\n",
			wantDiagCode:  "single_edit_incompatible_context_already_applied",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "context_free_insert_only_rejected_in_non_empty_file",
			fileName:       "no-context.txt",
			initialContent: "before\nafter\n",
			diff: makePatchText(
				"diff --git a/no-context.txt b/no-context.txt",
				"--- a/no-context.txt",
				"+++ b/no-context.txt",
				"@@ -1,0 +1,1 @@",
				"+insert",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "before\nafter\n",
			wantDiagCode:  "context_free_insert_unsafe",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelError,
		},
		{
			name:           "context_free_insert_only_applies_to_empty_file",
			fileName:       "empty-insert.txt",
			initialContent: "",
			diff: makePatchText(
				"diff --git a/empty-insert.txt b/empty-insert.txt",
				"--- a/empty-insert.txt",
				"+++ b/empty-insert.txt",
				"@@ -0,0 +1,1 @@",
				"+insert",
			),
			wantStatus:    ApplyUnifiedDiffStatusApplied,
			wantContent:   "insert\n",
			wantDiagCode:  "inserted_hunk",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelInfo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTextFile(t, dir, tc.fileName, tc.initialContent)

			out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: tc.diff})
			mustNoErr(t, err)
			if out.Status != tc.wantStatus {
				t.Fatalf("status mismatch: want %s got %s files=%#v", tc.wantStatus, out.Status, out.Files)
			}
			if len(out.Files) != 1 {
				t.Fatalf("expected one file result, got %#v", out.Files)
			}
			if out.Files[0].Status != tc.wantStatus {
				t.Fatalf(
					"file status mismatch: want %s got %s file=%#v",
					tc.wantStatus,
					out.Files[0].Status,
					out.Files[0],
				)
			}
			if got := readFileString(t, path); got != tc.wantContent {
				t.Fatalf("content mismatch: want %q got %q", tc.wantContent, got)
			}

			if tc.wantDiagCode != "" && !hasDiagnostic(out.Files[0].Diagnostics, tc.wantDiagLevel, tc.wantDiagCode) {
				t.Fatalf(
					"missing diagnostic %q level=%s in %#v",
					tc.wantDiagCode,
					tc.wantDiagLevel,
					out.Files[0].Diagnostics,
				)
			}

			if tc.reapply {
				out2, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: tc.diff})
				mustNoErr(t, err)
				if out2.Status != tc.wantReapplyStatus {
					t.Fatalf(
						"reapply status mismatch: want %s got %s files=%#v",
						tc.wantReapplyStatus,
						out2.Status,
						out2.Files,
					)
				}
				if len(out2.Files) != 1 || out2.Files[0].Status != tc.wantReapplyStatus {
					t.Fatalf("reapply file status mismatch: %#v", out2.Files)
				}
				if out2.Files[0].AlreadyAppliedHunks != 1 || out2.Files[0].AppliedHunks != 0 {
					t.Fatalf("reapply hunk counters mismatch: %#v", out2.Files[0])
				}
				if got := readFileString(t, path); got != tc.wantContent {
					t.Fatalf("reapply should not change content: want %q got %q", tc.wantContent, got)
				}
			}
		})
	}
}

func TestApplyUnifiedDiffSingleEditGroupSplit(t *testing.T) {
	mk := func(lines ...parsedHunkLine) parsedHunk {
		return parsedHunk{Lines: lines}
	}

	tests := []struct {
		name     string
		hunk     parsedHunk
		wantOK   bool
		wantPref []string
		wantOld  []string
		wantNew  []string
		wantSuff []string
	}{
		{
			name: "plus_then_minus",
			hunk: mk(
				parsedHunkLine{Kind: ' ', Text: "pre"},
				parsedHunkLine{Kind: '+', Text: "new"},
				parsedHunkLine{Kind: '-', Text: "old"},
				parsedHunkLine{Kind: ' ', Text: "post"},
			),
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  []string{"old"},
			wantNew:  []string{"new"},
			wantSuff: []string{"post"},
		},
		{
			name: "minus_then_plus",
			hunk: mk(
				parsedHunkLine{Kind: ' ', Text: "pre"},
				parsedHunkLine{Kind: '-', Text: "old"},
				parsedHunkLine{Kind: '+', Text: "new"},
				parsedHunkLine{Kind: ' ', Text: "post"},
			),
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  []string{"old"},
			wantNew:  []string{"new"},
			wantSuff: []string{"post"},
		},
		{
			name: "multiple_edit_groups_rejected",
			hunk: mk(
				parsedHunkLine{Kind: ' ', Text: "pre"},
				parsedHunkLine{Kind: '+', Text: "one"},
				parsedHunkLine{Kind: ' ', Text: "mid"},
				parsedHunkLine{Kind: '-', Text: "two"},
				parsedHunkLine{Kind: ' ', Text: "post"},
			),
			wantOK: false,
		},
		{
			name: "context_free_insertion_rejected",
			hunk: mk(
				parsedHunkLine{Kind: '+', Text: "insert"},
			),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hunkSpec, ok := splitSingleEditGroupHunk(tt.hunk)
			gotPref, gotOld, gotNew, gotSuff := hunkSpec.Prefix, hunkSpec.OldEdit, hunkSpec.NewEdit, hunkSpec.Suffix
			if ok != tt.wantOK {
				t.Fatalf("ok mismatch: want %v got %v", tt.wantOK, ok)
			}
			if !tt.wantOK {
				return
			}
			if !slicesEqual(gotPref, tt.wantPref) || !slicesEqual(gotOld, tt.wantOld) ||
				!slicesEqual(gotNew, tt.wantNew) {
				t.Fatalf(
					"split mismatch: got prefix=%v old=%v new=%v want prefix=%v old=%v new=%v",
					gotPref, gotOld, gotNew, tt.wantPref, tt.wantOld, tt.wantNew,
				)
			}
			if !slicesEqual(gotSuff, tt.wantSuff) {
				t.Fatalf("suffix mismatch: got %v want %v", gotSuff, tt.wantSuff)
			}
		})
	}
}

func TestApplyUnifiedDiffSingleEditMatcherDiagnostics(t *testing.T) {
	spec := singleEditHunkSpec{
		Prefix:  []string{"pre"},
		OldEdit: []string{"old"},
		NewEdit: []string{"new"},
		Suffix:  []string{"post"},
	}

	t.Run("one_sided_prefix_with_ambiguous_suffix", func(t *testing.T) {
		lines := []string{"pre", "old", "post", "x", "post"}
		matches, diags := oneSidedSingleEditMatches(lines, spec, []int{0}, []int{2, 4}, compareExact, "exact", false)

		if len(matches) != 1 || matches[0].EditStart != 1 {
			t.Fatalf("unexpected matches: %#v", matches)
		}
		if !hasDiagnostic(diags, ApplyUnifiedDiffDiagnosticLevelWarning, "single_edit_suffix_ambiguous") {
			t.Fatalf("missing suffix ambiguity diagnostic: %#v", diags)
		}
	})

	t.Run("one_sided_suffix_with_ambiguous_prefix", func(t *testing.T) {
		lines := []string{"pre", "x", "pre", "old", "post"}
		matches, diags := oneSidedSingleEditMatches(lines, spec, []int{0, 2}, []int{4}, compareExact, "exact", false)

		if len(matches) != 1 || matches[0].EditStart != 3 {
			t.Fatalf("unexpected matches: %#v", matches)
		}
		if !hasDiagnostic(diags, ApplyUnifiedDiffDiagnosticLevelWarning, "single_edit_prefix_ambiguous") {
			t.Fatalf("missing prefix ambiguity diagnostic: %#v", diags)
		}
	})

	t.Run("select_unique_single_edit_match_reports_ambiguity", func(t *testing.T) {
		_, ok, ambiguous, diags := selectUniqueSingleEditMatch(
			[]singleEditMatch{{EditStart: 1}, {EditStart: 5}},
			"demo",
		)
		if ok || !ambiguous {
			t.Fatalf("expected ambiguity, got ok=%v ambiguous=%v diags=%#v", ok, ambiguous, diags)
		}
		if !hasDiagnostic(diags, ApplyUnifiedDiffDiagnosticLevelWarning, "single_edit_ambiguous") {
			t.Fatalf("missing ambiguity diagnostic: %#v", diags)
		}
	})
}

func TestApplyUnifiedDiffSingleEditGroupMatchCases(t *testing.T) {
	mkSpec := func(prefix, oldEdit, newEdit, suffix []string) singleEditHunkSpec {
		return singleEditHunkSpec{
			Prefix:  prefix,
			OldEdit: oldEdit,
			NewEdit: newEdit,
			Suffix:  suffix,
		}
	}

	strongPrefix := []string{"prefix anchor line"}
	strongSuffix := []string{"suffix anchor line"}
	strongOld := []string{"old value"}
	strongNew := []string{"new value"}

	tests := []struct {
		name          string
		spec          singleEditHunkSpec
		lines         []string
		wantOK        bool
		wantStart     int
		wantDiagCodes []string
	}{
		{
			name:      "both_unique_compatible_applies",
			spec:      mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines:     []string{"prefix anchor line", "old value", "suffix anchor line"},
			wantOK:    true,
			wantStart: 1,
		},
		{
			name:      "unique_prefix_with_weak_suffix_applies",
			spec:      mkSpec(strongPrefix, strongOld, strongNew, []string{"x"}),
			lines:     []string{"prefix anchor line", "old value", "x"},
			wantOK:    true,
			wantStart: 1,
		},
		{
			name:      "unique_suffix_with_weak_prefix_applies",
			spec:      mkSpec([]string{"x"}, strongOld, strongNew, strongSuffix),
			lines:     []string{"x", "old value", "suffix anchor line"},
			wantOK:    true,
			wantStart: 1,
		},
		{
			name: "unique_prefix_with_ambiguous_suffix_falls_back",
			spec: mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines: []string{
				"prefix anchor line",
				"old value",
				"noise",
				"suffix anchor line",
				"noise",
				"suffix anchor line",
			},
			wantOK:        true,
			wantStart:     1,
			wantDiagCodes: []string{"single_edit_suffix_ambiguous"},
		},
		{
			name: "unique_suffix_with_ambiguous_prefix_falls_back",
			spec: mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines: []string{
				"prefix anchor line",
				"noise",
				"prefix anchor line",
				"noise",
				"old value",
				"suffix anchor line",
			},
			wantOK:        true,
			wantStart:     4,
			wantDiagCodes: []string{"single_edit_prefix_ambiguous"},
		},
		{
			name:          "both_unique_incompatible_conflicts",
			spec:          mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines:         []string{"prefix anchor line", "other value", "suffix anchor line"},
			wantOK:        false,
			wantDiagCodes: []string{"single_edit_incompatible_context"},
		},
		{
			name: "both_nonunique_compatible_but_ambiguous_conflicts",
			spec: mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines: []string{
				"prefix anchor line",
				"old value",
				"suffix anchor line",
				"prefix anchor line",
				"old value",
				"suffix anchor line",
			},
			wantOK:        false,
			wantDiagCodes: []string{"single_edit_ambiguous"},
		},
		{
			name: "both_nonunique_incompatible_conflicts",
			spec: mkSpec(strongPrefix, strongOld, strongNew, strongSuffix),
			lines: []string{
				"prefix anchor line",
				"alpha value",
				"suffix anchor line",
				"prefix anchor line",
				"beta value",
				"suffix anchor line",
			},
			wantOK:        false,
			wantDiagCodes: []string{"single_edit_non_unique_context"},
		},
		{
			name:      "delete_only_single_block_applies",
			spec:      mkSpec(strongPrefix, []string{"delete value"}, nil, strongSuffix),
			lines:     []string{"prefix anchor line", "delete value", "suffix anchor line"},
			wantOK:    true,
			wantStart: 1,
		},
		{
			name:      "insert_only_single_block_applies",
			spec:      mkSpec(strongPrefix, nil, []string{"insert value"}, strongSuffix),
			lines:     []string{"prefix anchor line", "insert value", "suffix anchor line"},
			wantOK:    true,
			wantStart: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, ok, diags := findSingleEditGroupMatch(tt.lines, tt.spec)
			if ok != tt.wantOK {
				t.Fatalf("ok mismatch: want %v got %v diags=%#v", tt.wantOK, ok, diags)
			}
			if tt.wantOK && match.EditStart != tt.wantStart {
				t.Fatalf("match start mismatch: want %d got %d", tt.wantStart, match.EditStart)
			}
			for _, code := range tt.wantDiagCodes {
				if !hasDiagnostic(diags, ApplyUnifiedDiffDiagnosticLevelWarning, code) {
					t.Fatalf("missing diagnostic %q in %#v", code, diags)
				}
			}
		})
	}
}

func TestApplyUnifiedDiffContextFreeInsertHunk(t *testing.T) {
	hunk := parsedHunk{
		Lines: []parsedHunkLine{{Kind: '+', Text: "insert"}},
	}

	applyResult, err := applyHunkToLines(nil, hunk, 0, 0, true)
	lines, applied, already := applyResult.Lines, applyResult.NewLen, applyResult.AlreadyApplied
	mustNoErr(t, err)
	if applied != 1 || already || len(lines) != 1 || lines[0] != "insert" {
		t.Fatalf("unexpected empty-file insert result: applied=%d already=%v lines=%v", applied, already, lines)
	}

	_, err = applyHunkToLines([]string{"before"}, hunk, 0, 0, true)
	if err == nil {
		t.Fatalf("expected context-free insert into non-empty file to fail")
	}
}

func TestApplyUnifiedDiffSingleEditGroupSplitOrderVariants(t *testing.T) {
	tests := []struct {
		name     string
		hunk     parsedHunk
		wantOK   bool
		wantPref []string
		wantOld  []string
		wantNew  []string
		wantSuff []string
	}{
		{
			name: "plus_then_minus",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: ' ', Text: "pre"},
					{Kind: '+', Text: "new"},
					{Kind: '-', Text: "old"},
					{Kind: ' ', Text: "post"},
				},
			},
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  []string{"old"},
			wantNew:  []string{"new"},
			wantSuff: []string{"post"},
		},
		{
			name: "minus_then_plus",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: ' ', Text: "pre"},
					{Kind: '-', Text: "old"},
					{Kind: '+', Text: "new"},
					{Kind: ' ', Text: "post"},
				},
			},
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  []string{"old"},
			wantNew:  []string{"new"},
			wantSuff: []string{"post"},
		},
		{
			name: "delete_only_block",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: ' ', Text: "pre"},
					{Kind: '-', Text: "old"},
					{Kind: ' ', Text: "post"},
				},
			},
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  []string{"old"},
			wantNew:  nil,
			wantSuff: []string{"post"},
		},
		{
			name: "insert_only_block",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: ' ', Text: "pre"},
					{Kind: '+', Text: "new"},
					{Kind: ' ', Text: "post"},
				},
			},
			wantOK:   true,
			wantPref: []string{"pre"},
			wantOld:  nil,
			wantNew:  []string{"new"},
			wantSuff: []string{"post"},
		},
		{
			name: "multiple_edit_groups_rejected",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: ' ', Text: "pre"},
					{Kind: '+', Text: "one"},
					{Kind: ' ', Text: "mid"},
					{Kind: '-', Text: "two"},
					{Kind: ' ', Text: "post"},
				},
			},
			wantOK: false,
		},
		{
			name: "context_free_insert_rejected",
			hunk: parsedHunk{
				Lines: []parsedHunkLine{
					{Kind: '+', Text: "insert"},
				},
			},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := splitSingleEditGroupHunk(tt.hunk)
			if ok != tt.wantOK {
				t.Fatalf("ok mismatch: want %v got %v", tt.wantOK, ok)
			}
			if !tt.wantOK {
				return
			}
			if !equalStrings(got.Prefix, tt.wantPref) ||
				!equalStrings(got.OldEdit, tt.wantOld) ||
				!equalStrings(got.NewEdit, tt.wantNew) ||
				!equalStrings(got.Suffix, tt.wantSuff) {
				t.Fatalf(
					"split mismatch: got=%#v want prefix=%v old=%v new=%v suffix=%v",
					got,
					tt.wantPref,
					tt.wantOld,
					tt.wantNew,
					tt.wantSuff,
				)
			}
		})
	}
}

func TestApplyUnifiedDiffSingleEditGroupMatcherCoverage(t *testing.T) {
	spec := singleEditHunkSpec{
		Prefix:  []string{"prefix anchor line"},
		OldEdit: []string{"old value"},
		NewEdit: []string{"new value"},
		Suffix:  []string{"suffix anchor line"},
	}

	tests := []struct {
		name      string
		lines     []string
		wantOK    bool
		wantStart int
		wantCodes []string
	}{
		{
			name: "unique_prefix_with_ambiguous_suffix_uses_prefix_fallback",
			lines: []string{
				"prefix anchor line",
				"old value",
				"suffix anchor line",
				"noise line",
				"suffix anchor line",
			},
			wantOK:    true,
			wantStart: 1,
			wantCodes: []string{"single_edit_suffix_ambiguous"},
		},
		{
			name: "unique_suffix_with_ambiguous_prefix_uses_suffix_fallback",
			lines: []string{
				"prefix anchor line",
				"noise line",
				"prefix anchor line",
				"old value",
				"suffix anchor line",
			},
			wantOK:    true,
			wantStart: 3,
			wantCodes: []string{"single_edit_prefix_ambiguous"},
		},
		{
			name:      "both_unique_compatible_applies",
			lines:     []string{"prefix anchor line", "old value", "suffix anchor line"},
			wantOK:    true,
			wantStart: 1,
		},
		{
			name:      "both_unique_incompatible_conflicts",
			lines:     []string{"prefix anchor line", "alpha value", "suffix anchor line"},
			wantOK:    false,
			wantCodes: []string{"single_edit_incompatible_context"},
		},
		{
			name: "both_nonunique_compatible_but_ambiguous_conflicts",
			lines: []string{
				"prefix anchor line",
				"old value",
				"suffix anchor line",
				"prefix anchor line",
				"old value",
				"suffix anchor line",
			},
			wantOK:    false,
			wantCodes: []string{"single_edit_ambiguous"},
		},
		{
			name: "both_nonunique_incompatible_conflicts",
			lines: []string{
				"prefix anchor line",
				"alpha value",
				"suffix anchor line",
				"prefix anchor line",
				"beta value",
				"suffix anchor line",
			},
			wantOK:    false,
			wantCodes: []string{"single_edit_non_unique_context"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			match, ok, diags := findSingleEditGroupMatch(tt.lines, spec)
			if ok != tt.wantOK {
				t.Fatalf("ok mismatch: want %v got %v diags=%#v", tt.wantOK, ok, diags)
			}
			if ok && match.EditStart != tt.wantStart {
				t.Fatalf("match start mismatch: want %d got %d", tt.wantStart, match.EditStart)
			}
			for _, code := range tt.wantCodes {
				if !hasDiagCode(diags, ApplyUnifiedDiffDiagnosticLevelWarning, code) {
					t.Fatalf("missing diagnostic %q in %#v", code, diags)
				}
			}
		})
	}
}

func TestApplyUnifiedDiffCreateDeleteRegression(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	t.Run("create_from_dev_null_creates_nested_parent_dirs_and_file", func(t *testing.T) {
		createdPath := filepath.Join(dir, "nested", "branch", "created.txt")
		diff := makePatchText(
			"diff --git a/nested/branch/created.txt b/nested/branch/created.txt",
			"--- /dev/null",
			"+++ b/nested/branch/created.txt",
			"@@ -0,0 +1,4 @@",
			" header line",
			"+body line one",
			"+body line two",
			" footer line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("create file status mismatch: %#v", out.Files)
		}
		if got := readFileString(t, createdPath); got != "header line\nbody line one\nbody line two\nfooter line\n" {
			t.Fatalf("created content mismatch: got %q", got)
		}
		if _, err := os.Stat(createdPath); err != nil {
			t.Fatalf("created file should exist on disk: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(createdPath)); err != nil {
			t.Fatalf("parent directory should exist on disk: %v", err)
		}
		if !hasDiagCode(
			out.Files[0].Diagnostics,
			ApplyUnifiedDiffDiagnosticLevelWarning,
			"create_fallback_from_hunk_body",
		) {
			t.Fatalf("expected create fallback diagnostic, got %#v", out.Files[0].Diagnostics)
		}
	})

	t.Run("delete_patch_removes_nested_file", func(t *testing.T) {
		deletedPath := writeTextFile(t, dir, "nested/delete-me.txt", "header line\nremove me\nfooter line\n")
		diff := makePatchText(
			"diff --git a/nested/delete-me.txt b/nested/delete-me.txt",
			"--- a/nested/delete-me.txt",
			"+++ /dev/null",
			"@@ -1,3 +0,0 @@",
			" header line",
			"-remove me",
			" footer line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("delete status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 || out.Files[0].Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("delete file status mismatch: %#v", out.Files)
		}
		if _, err := os.Stat(deletedPath); !os.IsNotExist(err) {
			t.Fatalf("deleted file should be removed from disk, stat err=%v", err)
		}
	})
}

func TestApplyUnifiedDiffSingleEditFallbackExtendedEndToEnd(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	tests := []struct {
		name              string
		fileName          string
		initialContent    string
		diff              string
		wantStatus        ApplyUnifiedDiffStatus
		wantContent       string
		wantDiagCode      string
		wantDiagLevel     ApplyUnifiedDiffDiagnosticLevel
		reapply           bool
		wantReapplyStatus ApplyUnifiedDiffStatus
	}{
		{
			name:           "insert_only_unique_suffix_recovers_stale_prefix",
			fileName:       "insert-suffix-rescue.txt",
			initialContent: "intro line\nunique suffix anchor line\noutro line\n",
			diff: makePatchText(
				"diff --git a/insert-suffix-rescue.txt b/insert-suffix-rescue.txt",
				"--- a/insert-suffix-rescue.txt",
				"+++ b/insert-suffix-rescue.txt",
				"@@ -10,2 +10,4 @@",
				" stale prefix anchor line",
				"+inserted line one",
				"+inserted line two",
				" unique suffix anchor line",
			),
			wantStatus:        ApplyUnifiedDiffStatusApplied,
			wantContent:       "intro line\ninserted line one\ninserted line two\nunique suffix anchor line\noutro line\n",
			reapply:           true,
			wantReapplyStatus: ApplyUnifiedDiffStatusAlreadyApplied,
		},
		{
			name:           "insert_only_unique_prefix_recovers_stale_suffix",
			fileName:       "insert-prefix-rescue.txt",
			initialContent: "unique prefix anchor line\noutro line\n",
			diff: makePatchText(
				"diff --git a/insert-prefix-rescue.txt b/insert-prefix-rescue.txt",
				"--- a/insert-prefix-rescue.txt",
				"+++ b/insert-prefix-rescue.txt",
				"@@ -10,2 +10,4 @@",
				" unique prefix anchor line",
				"+inserted line one",
				"+inserted line two",
				" stale suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "unique prefix anchor line\ninserted line one\ninserted line two\noutro line\n",
		},
		{
			name:           "delete_only_unique_prefix_recovers_stale_suffix",
			fileName:       "delete-prefix-rescue.txt",
			initialContent: "intro line\nunique prefix anchor line\nremove line one\nremove line two\noutro line\n",
			diff: makePatchText(
				"diff --git a/delete-prefix-rescue.txt b/delete-prefix-rescue.txt",
				"--- a/delete-prefix-rescue.txt",
				"+++ b/delete-prefix-rescue.txt",
				"@@ -10,4 +10,2 @@",
				" unique prefix anchor line",
				"-remove line one",
				"-remove line two",
				" stale suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "intro line\nunique prefix anchor line\noutro line\n",
		},
		{
			name:           "delete_only_unique_suffix_recovers_stale_prefix",
			fileName:       "delete-suffix-rescue.txt",
			initialContent: "intro line\nremove line one\nremove line two\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/delete-suffix-rescue.txt b/delete-suffix-rescue.txt",
				"--- a/delete-suffix-rescue.txt",
				"+++ b/delete-suffix-rescue.txt",
				"@@ -10,4 +10,2 @@",
				" stale prefix anchor line",
				"-remove line one",
				"-remove line two",
				" unique suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "intro line\nunique suffix anchor line\n",
		},
		{
			name:           "insert_only_both_unique_but_not_adjacent_conflicts",
			fileName:       "insert-incompatible-unique.txt",
			initialContent: "unique prefix anchor line\nexisting middle line\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/insert-incompatible-unique.txt b/insert-incompatible-unique.txt",
				"--- a/insert-incompatible-unique.txt",
				"+++ b/insert-incompatible-unique.txt",
				"@@ -1,2 +1,3 @@",
				" unique prefix anchor line",
				"+inserted line",
				" unique suffix anchor line",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "unique prefix anchor line\nexisting middle line\nunique suffix anchor line\n",
			wantDiagCode:  "single_edit_incompatible_context",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
		{
			name:           "mixed_plus_then_minus_unique_suffix_recovers_stale_prefix",
			fileName:       "mixed-plus-minus-suffix.txt",
			initialContent: "intro line\nold value line\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/mixed-plus-minus-suffix.txt b/mixed-plus-minus-suffix.txt",
				"--- a/mixed-plus-minus-suffix.txt",
				"+++ b/mixed-plus-minus-suffix.txt",
				"@@ -10,3 +10,3 @@",
				" stale prefix anchor line",
				"+new value line",
				"-old value line",
				" unique suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "intro line\nnew value line\nunique suffix anchor line\n",
		},
		{
			name:           "mixed_minus_then_plus_unique_prefix_recovers_stale_suffix",
			fileName:       "mixed-minus-plus-prefix.txt",
			initialContent: "unique prefix anchor line\nold value line\noutro line\n",
			diff: makePatchText(
				"diff --git a/mixed-minus-plus-prefix.txt b/mixed-minus-plus-prefix.txt",
				"--- a/mixed-minus-plus-prefix.txt",
				"+++ b/mixed-minus-plus-prefix.txt",
				"@@ -10,3 +10,3 @@",
				" unique prefix anchor line",
				"-old value line",
				"+new value line",
				" stale suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "unique prefix anchor line\nnew value line\noutro line\n",
		},
		{
			name:           "weak_prefix_counterpart_ignored_when_suffix_is_unique",
			fileName:       "weak-prefix-unique-suffix.txt",
			initialContent: "intro line\nold value line\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/weak-prefix-unique-suffix.txt b/weak-prefix-unique-suffix.txt",
				"--- a/weak-prefix-unique-suffix.txt",
				"+++ b/weak-prefix-unique-suffix.txt",
				"@@ -10,3 +10,3 @@",
				" x",
				"-old value line",
				"+new value line",
				" unique suffix anchor line",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "intro line\nnew value line\nunique suffix anchor line\n",
		},
		{
			name:           "weak_suffix_counterpart_ignored_when_prefix_is_unique",
			fileName:       "weak-suffix-unique-prefix.txt",
			initialContent: "unique prefix anchor line\nold value line\noutro line\n",
			diff: makePatchText(
				"diff --git a/weak-suffix-unique-prefix.txt b/weak-suffix-unique-prefix.txt",
				"--- a/weak-suffix-unique-prefix.txt",
				"+++ b/weak-suffix-unique-prefix.txt",
				"@@ -10,3 +10,3 @@",
				" unique prefix anchor line",
				"-old value line",
				"+new value line",
				" x",
			),
			wantStatus:  ApplyUnifiedDiffStatusApplied,
			wantContent: "unique prefix anchor line\nnew value line\noutro line\n",
		},
		{
			name:           "both_unique_but_different_places_conflicts",
			fileName:       "unique-context-different-places.txt",
			initialContent: "unique prefix anchor line\nunrelated middle line\nold value line\nunique suffix anchor line\n",
			diff: makePatchText(
				"diff --git a/unique-context-different-places.txt b/unique-context-different-places.txt",
				"--- a/unique-context-different-places.txt",
				"+++ b/unique-context-different-places.txt",
				"@@ -1,3 +1,3 @@",
				" unique prefix anchor line",
				"-old value line",
				"+new value line",
				" unique suffix anchor line",
			),
			wantStatus:    ApplyUnifiedDiffStatusConflict,
			wantContent:   "unique prefix anchor line\nunrelated middle line\nold value line\nunique suffix anchor line\n",
			wantDiagCode:  "single_edit_incompatible_context",
			wantDiagLevel: ApplyUnifiedDiffDiagnosticLevelWarning,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTextFile(t, dir, tc.fileName, tc.initialContent)

			out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: tc.diff})
			mustNoErr(t, err)
			if out.Status != tc.wantStatus {
				t.Fatalf("status mismatch: want %s got %s files=%#v", tc.wantStatus, out.Status, out.Files)
			}
			if len(out.Files) != 1 {
				t.Fatalf("expected one file result, got %#v", out.Files)
			}
			if out.Files[0].Status != tc.wantStatus {
				t.Fatalf(
					"file status mismatch: want %s got %s file=%#v",
					tc.wantStatus,
					out.Files[0].Status,
					out.Files[0],
				)
			}
			if got := readFileString(t, path); got != tc.wantContent {
				t.Fatalf("content mismatch: want %q got %q", tc.wantContent, got)
			}
			if tc.wantDiagCode != "" && !hasDiagnostic(out.Files[0].Diagnostics, tc.wantDiagLevel, tc.wantDiagCode) {
				t.Fatalf(
					"missing diagnostic %q level=%s in %#v",
					tc.wantDiagCode,
					tc.wantDiagLevel,
					out.Files[0].Diagnostics,
				)
			}

			if tc.reapply {
				out2, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: tc.diff})
				mustNoErr(t, err)
				if out2.Status != tc.wantReapplyStatus {
					t.Fatalf(
						"reapply status mismatch: want %s got %s files=%#v",
						tc.wantReapplyStatus,
						out2.Status,
						out2.Files,
					)
				}
				if got := readFileString(t, path); got != tc.wantContent {
					t.Fatalf("reapply content mismatch: want %q got %q", tc.wantContent, got)
				}
			}
		})
	}
}

func TestApplyUnifiedDiffFuzzySingleEditFallbackExtended(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	t.Run("trimmed_single_edit_uses_unique_suffix", func(t *testing.T) {
		path := writeTextFile(
			t,
			dir,
			"trimmed-single-edit.txt",
			"intro line\n  old value line  \nunique suffix anchor line\n",
		)
		diff := makePatchText(
			"diff --git a/trimmed-single-edit.txt b/trimmed-single-edit.txt",
			"--- a/trimmed-single-edit.txt",
			"+++ b/trimmed-single-edit.txt",
			"@@ -100,3 +100,3 @@",
			" stale prefix anchor line",
			"-old value line",
			"+new value line",
			" unique suffix anchor line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "intro line\nnew value line\nunique suffix anchor line\n" {
			t.Fatalf("trimmed fallback content mismatch: got %q", got)
		}
	})

	t.Run("unique_old_edit_body_applies_when_context_is_stale", func(t *testing.T) {
		path := writeTextFile(
			t,
			dir,
			"old-body-only.txt",
			"intro line\nold body unique replacement target\noutro line\n",
		)
		diff := makePatchText(
			"diff --git a/old-body-only.txt b/old-body-only.txt",
			"--- a/old-body-only.txt",
			"+++ b/old-body-only.txt",
			"@@ -100,3 +100,3 @@",
			" stale prefix anchor line",
			"-old body unique replacement target",
			"+new body unique replacement target",
			" stale suffix anchor line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "intro line\nnew body unique replacement target\noutro line\n" {
			t.Fatalf("old-body fallback content mismatch: got %q", got)
		}
	})

	t.Run("unique_new_edit_body_detects_already_applied_when_context_is_stale", func(t *testing.T) {
		path := writeTextFile(
			t,
			dir,
			"new-body-already.txt",
			"intro line\nnew body unique replacement target\noutro line\n",
		)
		diff := makePatchText(
			"diff --git a/new-body-already.txt b/new-body-already.txt",
			"--- a/new-body-already.txt",
			"+++ b/new-body-already.txt",
			"@@ -100,3 +100,3 @@",
			" stale prefix anchor line",
			"-old body unique replacement target",
			"+new body unique replacement target",
			" stale suffix anchor line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("status mismatch: want already_applied got %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, path); got != "intro line\nnew body unique replacement target\noutro line\n" {
			t.Fatalf("already-applied fallback changed content: got %q", got)
		}
	})
}

func TestApplyUnifiedDiffCreateDeleteFilesystemCoverage(t *testing.T) {
	dir := t.TempDir()
	tt := mustNewTextTool(t, dir)

	t.Run("plain_dev_null_create_dry_run_does_not_write_then_apply_creates", func(t *testing.T) {
		rel := "plain-created.txt"
		full := filepath.Join(dir, rel)
		diff := makePatchText(
			"--- /dev/null",
			"+++ b/"+rel,
			"@@ -0,0 +1,2 @@",
			"+created line one",
			"+created line two",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff, DryRun: true})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("dry-run create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Fatalf("dry-run must not create file, stat err=%v", err)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("apply create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, full); got != "created line one\ncreated line two\n" {
			t.Fatalf("created content mismatch: got %q", got)
		}
	})

	t.Run("nested_dev_null_create_creates_parent_dirs_and_file", func(t *testing.T) {
		rel := "created/nested/file.txt"
		full := filepath.Join(dir, filepath.FromSlash(rel))
		diff := makePatchText(
			"diff --git a/"+rel+" b/"+rel,
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/"+rel,
			"@@ -0,0 +1,3 @@",
			"+alpha",
			"+beta",
			"+gamma",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("nested create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, full); got != "alpha\nbeta\ngamma\n" {
			t.Fatalf("nested create content mismatch: got %q", got)
		}
		if _, err := os.Stat(filepath.Dir(full)); err != nil {
			t.Fatalf("parent dir should exist: %v", err)
		}
	})

	t.Run("create_patch_infers_absolute_target_from_candidate_file_same_directory", func(t *testing.T) {
		appRoot := filepath.Join(dir, "app-cwd-for-create-inference")
		repoRoot := filepath.Join(dir, "repo-for-create-inference")
		if err := os.MkdirAll(appRoot, 0o755); err != nil {
			t.Fatalf("mkdir app root: %v", err)
		}

		candidate := writeTextFile(t, repoRoot, "src/existing.txt", "existing\n")
		tt := mustNewTextTool(t, appRoot)

		diff := makePatchText(
			"--- /dev/null",
			"+++ b/src/created-from-candidate.txt",
			"@@ -0,0 +1,2 @@",
			"+created line one",
			"+created line two",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{candidate},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("candidate-inferred create status mismatch: %s files=%#v", out.Status, out.Files)
		}

		wantPath := canonicalizePath(filepath.Join(repoRoot, "src", "created-from-candidate.txt"))
		if got := readFileString(t, wantPath); got != "created line one\ncreated line two\n" {
			t.Fatalf("candidate-inferred created content mismatch: got %q", got)
		}
		if _, err := os.Stat(filepath.Join(appRoot, "src", "created-from-candidate.txt")); !os.IsNotExist(err) {
			t.Fatalf(
				"create must not fall back to app work dir when candidatePaths infer repo target, stat err=%v",
				err,
			)
		}
		if len(out.Files) != 1 ||
			canonicalizePath(out.Files[0].ResolvedPath) != wantPath ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelInfo,
				"create_target_inferred_from_candidate_paths",
			) {
			t.Fatalf("candidate-inferred target/diagnostics mismatch: %#v", out.Files)
		}
	})

	t.Run("create_patch_infers_absolute_target_from_candidate_root_directory", func(t *testing.T) {
		appRoot := filepath.Join(dir, "app-cwd-for-root-create-inference")
		repoRoot := filepath.Join(dir, "repo-root-create-inference")
		if err := os.MkdirAll(appRoot, 0o755); err != nil {
			t.Fatalf("mkdir app root: %v", err)
		}
		if err := os.MkdirAll(repoRoot, 0o755); err != nil {
			t.Fatalf("mkdir repo root: %v", err)
		}

		tt := mustNewTextTool(t, appRoot)
		diff := makePatchText(
			"--- /dev/null",
			"+++ b/src/created-from-root.txt",
			"@@ -0,0 +1,1 @@",
			"+created from root",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{repoRoot},
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("root-inferred create status mismatch: %s files=%#v", out.Status, out.Files)
		}

		wantPath := canonicalizePath(filepath.Join(repoRoot, "src", "created-from-root.txt"))
		if got := readFileString(t, wantPath); got != "created from root\n" {
			t.Fatalf("root-inferred created content mismatch: got %q", got)
		}
	})

	t.Run("create_patch_with_ambiguous_candidate_roots_returns_needs_info", func(t *testing.T) {
		appRoot := filepath.Join(dir, "app-cwd-for-ambiguous-create")
		repoOne := filepath.Join(dir, "repo-one-ambiguous")
		repoTwo := filepath.Join(dir, "repo-two-ambiguous")
		if err := os.MkdirAll(appRoot, 0o755); err != nil {
			t.Fatalf("mkdir app root: %v", err)
		}
		candidateOne := writeTextFile(t, repoOne, "src/existing.txt", "one\n")
		candidateTwo := writeTextFile(t, repoTwo, "src/existing.txt", "two\n")
		tt := mustNewTextTool(t, appRoot)

		diff := makePatchText(
			"--- /dev/null",
			"+++ b/src/created-ambiguous.txt",
			"@@ -0,0 +1,1 @@",
			"+ambiguous",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{
			DiffText:       diff,
			CandidatePaths: []string{candidateOne, candidateTwo},
			DryRun:         true,
		})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusNeedsInfo {
			t.Fatalf("ambiguous create should need info, got %s files=%#v", out.Status, out.Files)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"create_target_ambiguous_from_candidate_paths",
			) {
			t.Fatalf("ambiguous create diagnostics mismatch: %#v", out.Files)
		}
	})

	t.Run("git_metadata_only_empty_create_creates_file_and_is_idempotent", func(t *testing.T) {
		if runtime.GOOS == toolutil.GOOSWindows {
			t.Skip("win mode issue")
		}
		rel := "metadata-empty-created.txt"
		full := filepath.Join(dir, rel)
		diff := makePatchText(
			"diff --git a/"+rel+" b/"+rel,
			"new file mode 100644",
			"index 0000000..e69de29",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("metadata-only empty create status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, full); got != "" {
			t.Fatalf("metadata-only empty create content mismatch: got %q", got)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("metadata-only empty create reapply status mismatch: %s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("git_metadata_only_empty_delete_deletes_file_and_is_idempotent", func(t *testing.T) {
		rel := "metadata-empty-delete.txt"
		full := writeTextFile(t, dir, rel, "")
		diff := makePatchText(
			"diff --git a/"+rel+" b/"+rel,
			"deleted file mode 100644",
			"index e69de29..0000000",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("metadata-only empty delete status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Fatalf("metadata-only empty delete should remove file, stat err=%v", err)
		}

		out, err = tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusAlreadyApplied {
			t.Fatalf("metadata-only empty delete reapply status mismatch: %s files=%#v", out.Status, out.Files)
		}
	})

	t.Run("git_metadata_only_delete_non_empty_file_conflicts", func(t *testing.T) {
		rel := "metadata-nonempty-delete.txt"
		full := writeTextFile(t, dir, rel, "still here\n")
		diff := makePatchText(
			"diff --git a/"+rel+" b/"+rel,
			"deleted file mode 100644",
			"index e69de29..0000000",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusConflict {
			t.Fatalf("metadata-only non-empty delete should conflict: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, full); got != "still here\n" {
			t.Fatalf("metadata-only non-empty delete must not modify file: got %q", got)
		}
	})

	t.Run("delete_to_dev_null_with_context_lines_removes_file", func(t *testing.T) {
		full := writeTextFile(t, dir, "delete-with-context.txt", "header line\nremove me\nfooter line\n")
		diff := makePatchText(
			"diff --git a/delete-with-context.txt b/delete-with-context.txt",
			"--- a/delete-with-context.txt",
			"+++ /dev/null",
			"@@ -1,3 +0,0 @@",
			" header line",
			"-remove me",
			" footer line",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplied {
			t.Fatalf("delete status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if _, err := os.Stat(full); !os.IsNotExist(err) {
			t.Fatalf("delete should remove file, stat err=%v", err)
		}
		if len(out.Files) != 1 ||
			!hasDiagnostic(
				out.Files[0].Diagnostics,
				ApplyUnifiedDiffDiagnosticLevelWarning,
				"delete_context_treated_as_deleted",
			) {
			t.Fatalf("missing delete-context diagnostic: %#v", out.Files)
		}
	})

	t.Run("delete_to_dev_null_dry_run_does_not_remove_file", func(t *testing.T) {
		full := writeTextFile(t, dir, "dry-run-delete.txt", "gone\n")
		diff := makePatchText(
			"diff --git a/dry-run-delete.txt b/dry-run-delete.txt",
			"--- a/dry-run-delete.txt",
			"+++ /dev/null",
			"@@ -1,1 +0,0 @@",
			"-gone",
		)

		out, err := tt.ApplyUnifiedDiff(t.Context(), ApplyUnifiedDiffArgs{DiffText: diff, DryRun: true})
		mustNoErr(t, err)
		if out.Status != ApplyUnifiedDiffStatusApplicable {
			t.Fatalf("dry-run delete status mismatch: %s files=%#v", out.Status, out.Files)
		}
		if got := readFileString(t, full); got != "gone\n" {
			t.Fatalf("dry-run delete must not remove or alter file, got %q", got)
		}
	})
}

func hasDiagCode(diags []ApplyUnifiedDiffDiagnostic, level ApplyUnifiedDiffDiagnosticLevel, code string) bool {
	for _, diag := range diags {
		if diag.Level == level && diag.Code == code {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
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

func slicesEqual(a, b []string) bool {
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

func hasDiagnostic(diags []ApplyUnifiedDiffDiagnostic, level ApplyUnifiedDiffDiagnosticLevel, code string) bool {
	for _, diag := range diags {
		if diag.Code != code {
			continue
		}
		if level == "" || diag.Level == level {
			return true
		}
	}
	return false
}

func containsDiagnostic(diags []ApplyUnifiedDiffDiagnostic, sub string) bool {
	for _, diag := range diags {
		if strings.Contains(diag.Code, sub) || strings.Contains(diag.Message, sub) {
			return true
		}
	}
	return false
}

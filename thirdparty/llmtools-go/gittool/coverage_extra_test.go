package gittool

import (
	"path/filepath"
	"testing"
)

func TestHelperValidationAndBounds(t *testing.T) {
	if err := validateLocalBranchName("@"); err == nil {
		t.Fatal("validateLocalBranchName(@) error = nil, want error")
	}
	if err := validateLocalBranchName("topic\x00bad"); err == nil {
		t.Fatal("validateLocalBranchName(NUL) error = nil, want error")
	}
	if err := validateLocalBranchName("topic\nbad"); err == nil {
		t.Fatal("validateLocalBranchName(control) error = nil, want error")
	}

	if err := validateTagName("@"); err == nil {
		t.Fatal("validateTagName(@) error = nil, want error")
	}
	if err := validateTagName("tag\x00bad"); err == nil {
		t.Fatal("validateTagName(NUL) error = nil, want error")
	}
	if err := validateTagName("tag\nbad"); err == nil {
		t.Fatal("validateTagName(control) error = nil, want error")
	}

	if err := validateRevision("HEAD\x00bad"); err == nil {
		t.Fatal("validateRevision(NUL) error = nil, want error")
	}
	if err := validateRevision("HEAD\nbad"); err == nil {
		t.Fatal("validateRevision(control) error = nil, want error")
	}

	if err := validateTagPattern("pattern\x00bad"); err == nil {
		t.Fatal("validateTagPattern(NUL) error = nil, want error")
	}
	if err := validateTagPattern("pattern\nbad"); err == nil {
		t.Fatal("validateTagPattern(control) error = nil, want error")
	}

	if got := normalizePositiveInt(0, 7, 1, 10); got != 7 {
		t.Fatalf("normalizePositiveInt(0, def) = %d, want 7", got)
	}
	if got := normalizePositiveInt(-1, 7, 1, 10); got != 1 {
		t.Fatalf("normalizePositiveInt(-1) = %d, want 1", got)
	}
	if got := normalizePositiveInt(11, 7, 1, 10); got != 10 {
		t.Fatalf("normalizePositiveInt(11) = %d, want 10", got)
	}

	if got := normalizeOptionalInt(nil, 7, 1, 10); got != 7 {
		t.Fatalf("normalizeOptionalInt(t.Context()) = %d, want 7", got)
	}
	zero := 0
	if got := normalizeOptionalInt(&zero, 7, 1, 10); got != 1 {
		t.Fatalf("normalizeOptionalInt(0) = %d, want 1", got)
	}
	tooHigh := 11
	if got := normalizeOptionalInt(&tooHigh, 7, 1, 10); got != 10 {
		t.Fatalf("normalizeOptionalInt(11) = %d, want 10", got)
	}
}

func TestGitToolOptionsNilContextAndSymlinkBlockingAll(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	_ = mustInitRepo(t, repoAbs, false)

	tool, err := NewGitTool(
		WithWorkBaseDir(base),
		WithAllowedRoots([]string{base}),
		WithBlockSymlinks(true),
		WithDefaultAuthor("Fallback Name", "fallback@example.invalid"),
	)
	if err != nil {
		t.Fatalf("NewGitTool() error = %v", err)
	}
	snap := tool.snapshot()
	if !snap.blockSymlinks {
		t.Fatal("snapshot.blockSymlinks = false, want true")
	}
	if snap.defaultAuthorName != "Fallback Name" || snap.defaultAuthorEmail != "fallback@example.invalid" {
		t.Fatalf(
			"snapshot default author = (%q, %q), want fallback author",
			snap.defaultAuthorName,
			snap.defaultAuthorEmail,
		)
	}

	status, err := tool.Status(t.Context(), StatusArgs{RepoPath: repoRel})
	if err != nil {
		t.Fatalf("Status(t.Context()) error = %v", err)
	}
	assertSamePath(t, status.RepoPath, repoAbs)
	if !status.UnbornHead {
		t.Fatal("Status(unborn).UnbornHead = false, want true")
	}
	if status.Branch != "" {
		t.Fatalf("Status(unborn).Branch = %q, want empty", status.Branch)
	}
	if status.HeadHash != "" {
		t.Fatalf("Status(unborn).HeadHash = %q, want empty", status.HeadHash)
	}
	if !status.IsClean {
		t.Fatal("Status(unborn).IsClean = false, want true")
	}
	if len(status.Entries) != 0 {
		t.Fatalf("Status(unborn).Entries = %#v, want empty", status.Entries)
	}

	_, err = tool.Add(t.Context(), AddArgs{RepoPath: repoRel, All: true})
	if err == nil {
		t.Fatal("Add(all=true with symlink blocking) error = nil, want error")
	}
}

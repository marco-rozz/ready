package gittool

import (
	"path/filepath"
	"testing"
)

func TestGrepTruncatesByMaxFiles(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, "a.txt", "alpha one\n")
	mustWriteFile(t, repoAbs, "b.txt", "alpha two\n")
	_ = mustCommitAll(t, repo, "grep fixtures")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Grep(ctx, GrepArgs{
		RepoPath:   repoRel,
		Pattern:    "alpha",
		MaxFiles:   1,
		MaxMatches: 10,
	})
	if err != nil {
		t.Fatalf("Grep(maxFiles=1) error = %v", err)
	}
	if !out.Truncated || !out.OutputMeta.Truncated {
		t.Fatalf("Grep(maxFiles=1).Truncated = (%v, %v), want true", out.Truncated, out.OutputMeta.Truncated)
	}
	if out.FilesVisited != 2 {
		t.Fatalf("Grep(maxFiles=1).FilesVisited = %d, want 2", out.FilesVisited)
	}
	if out.Count != 1 || len(out.Matches) != 1 {
		t.Fatalf("Grep(maxFiles=1).Matches = %#v, want 1 match", out.Matches)
	}
	if out.Matches[0].Path != "a.txt" {
		t.Fatalf("Grep(maxFiles=1).Matches[0].Path = %q, want %q", out.Matches[0].Path, "a.txt")
	}
}

func TestGrepMatchesTextIgnoresBinaryAndTruncates(t *testing.T) {
	base := t.TempDir()
	repoAbs := filepath.Join(base, testRepoDirName)
	repoRel := filepath.FromSlash(testRepoDirName)

	repo := mustInitRepo(t, repoAbs, false)
	mustWriteFile(t, repoAbs, "search.txt", "one\nalpha\nBeta\nALPHA\nomega\n")
	mustWriteFile(t, repoAbs, "literal.txt", "a.*\n")
	mustWriteBinaryFile(t, repoAbs, testBinaryFileName, []byte(testBinaryContent))
	_ = mustCommitAll(t, repo, "grep fixtures")

	tool := newTestGitTool(t, base)
	ctx := t.Context()

	out, err := tool.Grep(ctx, GrepArgs{
		RepoPath:     repoRel,
		Pattern:      "alpha",
		IgnoreCase:   true,
		ContextLines: new(1),
	})
	if err != nil {
		t.Fatalf("Grep() error = %v", err)
	}
	if out.Count != 2 || len(out.Matches) != 2 {
		t.Fatalf("Grep().Matches = %#v, want 2 matches", out.Matches)
	}
	if out.FilesVisited != 3 {
		t.Fatalf("Grep().FilesVisited = %d, want 3", out.FilesVisited)
	}
	if out.OmittedBinaryFiles != 1 {
		t.Fatalf("Grep().OmittedBinaryFiles = %d, want 1", out.OmittedBinaryFiles)
	}
	if out.Matches[0].Path != "search.txt" || out.Matches[0].LineNumber != 2 {
		t.Fatalf("Grep().Matches[0] = %#v, want line 2 in search.txt", out.Matches[0])
	}
	if len(out.Matches[0].Before) != 1 || out.Matches[0].Before[0] != "one" {
		t.Fatalf("Grep().Matches[0].Before = %#v, want [one]", out.Matches[0].Before)
	}
	if len(out.Matches[0].After) != 1 || out.Matches[0].After[0] != "Beta" {
		t.Fatalf("Grep().Matches[0].After = %#v, want [Beta]", out.Matches[0].After)
	}
	if out.Matches[1].Path != "search.txt" || out.Matches[1].LineNumber != 4 {
		t.Fatalf("Grep().Matches[1] = %#v, want line 4 in search.txt", out.Matches[1])
	}

	literal, err := tool.Grep(ctx, GrepArgs{
		RepoPath: repoRel,
		Pattern:  "a.*",
		Literal:  true,
		Paths:    []string{"literal.txt"},
	})
	if err != nil {
		t.Fatalf("Grep(literal) error = %v", err)
	}
	if literal.Count != 1 || len(literal.Matches) != 1 {
		t.Fatalf("Grep(literal).Matches = %#v, want 1 match", literal.Matches)
	}
	if literal.Matches[0].Path != "literal.txt" || literal.Matches[0].Line != "a.*" {
		t.Fatalf("Grep(literal).Matches[0] = %#v, want literal a.* line", literal.Matches[0])
	}
	if len(literal.Paths) != 1 || literal.Paths[0] != "literal.txt" {
		t.Fatalf("Grep(literal).Paths = %#v, want [literal.txt]", literal.Paths)
	}

	truncated, err := tool.Grep(ctx, GrepArgs{
		RepoPath:   repoRel,
		Pattern:    "alpha",
		IgnoreCase: true,
		MaxMatches: 1,
	})
	if err != nil {
		t.Fatalf("Grep(maxMatches=1) error = %v", err)
	}
	if !truncated.Truncated || len(truncated.Matches) != 1 {
		t.Fatalf("Grep(maxMatches=1) = %#v, want single truncated match", truncated)
	}

	_, err = tool.Grep(ctx, GrepArgs{RepoPath: repoRel, Pattern: "["})
	if err == nil {
		t.Fatal("Grep(invalid pattern) error = nil, want error")
	}

	_, err = tool.Grep(ctx, GrepArgs{RepoPath: repoRel, Pattern: ""})
	if err == nil {
		t.Fatal("Grep(empty pattern) error = nil, want error")
	}

	defaulted, err := tool.Grep(ctx, GrepArgs{RepoPath: repoRel, Pattern: "a", MaxFiles: 0})
	if err != nil {
		t.Fatalf("Grep(maxFiles default) error = %v", err)
	}
	if defaulted.MaxFiles != defaultGrepMaxFiles {
		t.Fatalf("Grep(maxFiles default).MaxFiles = %d, want %d", defaulted.MaxFiles, defaultGrepMaxFiles)
	}
}

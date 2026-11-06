package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// commitTestConfig writes a config for commit tests; contents is always the
// trivial document (unparam-suppressed).
const trivialDocument = "config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n"

func commitTestConfig(t *testing.T, contents string) string { //nolint:unparam // kept symmetric for future variants
	t.Helper()
	path := filepath.Join(t.TempDir(), "house.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestCommitCandidate_SourceConflict(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)

	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// External edit after the snapshot.
	if err = os.WriteFile(path, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# hand edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := CommitCandidate(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# tool candidate\n"))
	if !errors.Is(err, ErrConfigSourceConflict) {
		t.Fatalf("expected source conflict, got %v (report %+v)", err, report)
	}
	if report.Committed {
		t.Fatal("conflicted commit must not report Committed")
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# hand edit") {
		t.Fatalf("the operator's external bytes must survive: %q", string(data))
	}
}

func TestCommitCandidate_ModeConflict(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err = CommitCandidate(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n")); !errors.Is(err, ErrConfigSourceConflict) {
		t.Fatalf("expected mode conflict, got %v", err)
	}
}

func TestCommitCandidate_SymlinkLeafRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(realPath, []byte(trivialDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "house.yaml")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadConfigSource(context.Background(), linkPath); !errors.Is(err, ErrNotRegularConfig) {
		t.Fatalf("symlink leaf must be refused, got %v", err)
	}
}

func TestCommitCandidate_HardLinkedSourceRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard-link semantics differ on windows")
	}
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "a.yaml")
	second := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(first, []byte(trivialDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadConfigSource(context.Background(), first); !errors.Is(err, ErrNotRegularConfig) {
		t.Fatalf("multiply hard-linked source must be refused, got %v", err)
	}
}

func TestCommitCandidate_CancelBeforeReplacement(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := CommitCandidate(ctx, state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n"))
	if err == nil || report.Committed {
		t.Fatalf("cancellation before replacement must not commit, got report=%+v err=%v", report, err)
	}

	data, _ := os.ReadFile(path)
	if string(data) != trivialDocument {
		t.Fatalf("original bytes must survive: %q", string(data))
	}
}

func TestCommitCandidate_CommittedFactSurvivesPostCommitFailure(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("durability failure")
	calls := 0
	fs := migrationIO{syncDir: func(context.Context, string) error {
		calls++
		if calls == 2 { // the post-replacement sync only
			return boom
		}

		return nil
	}}

	report, err := CommitCandidateWithFS(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n"), fs)
	if !errors.Is(err, ErrCommitPostCommit) {
		t.Fatalf("expected post-commit durability failure, got %v", err)
	}
	if !report.Committed {
		t.Fatal("the replacement fact must survive the later failure")
	}

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# candidate") {
		t.Fatalf("the candidate must be on disk despite the later failure: %q", string(data))
	}
}

func TestCommitCandidate_PruneFailureBecomesWarning(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("cannot remove backup")
	fs := migrationIO{prune: func(context.Context, string) error { return boom }}

	report, err := CommitCandidateWithFS(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n"), fs)
	if err != nil {
		t.Fatalf("a pruning failure must not fail the commit: %v", err)
	}
	if !report.Committed || len(report.Warnings) == 0 {
		t.Fatalf("expected committed-with-retention-warning, got %+v", report)
	}
}

func TestCommitCandidate_BackupFailureRetainsOriginal(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	boom := errors.New("backup failure")
	fs := migrationIO{write: func(context.Context, string, string, []byte, os.FileMode) (string, error) { return "", boom }}

	report, err := CommitCandidateWithFS(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n"), fs)
	if !errors.Is(err, ErrCommitTransient) {
		t.Fatalf("pre-commit backup failure must be retryable-transient, got %v", err)
	}
	if report.Committed {
		t.Fatal("not committed")
	}

	orig, _ := os.ReadFile(path)
	if string(orig) != trivialDocument {
		t.Fatalf("original bytes must survive: %q", string(orig))
	}
}

func TestCommitCandidate_SuccessSerializesAndPrunes(t *testing.T) {
	t.Parallel()

	path := commitTestConfig(t, trivialDocument)
	state, err := ReadConfigSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// Seed three old backups so the shared newest-three pruning is exercised.
	for i := 0; i < 5; i++ {
		name := filepath.Base(path) + ".bak.20200101T000000.000000000Z." + strings.Repeat("1", 6+i)
		if wErr := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte("old"), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
	}

	report, err := CommitCandidate(context.Background(), state, []byte("config_version: 2\nagent:\n  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001\n# candidate\n"))
	if err != nil || !report.Committed {
		t.Fatalf("commit failed: %v (%+v)", err, report)
	}
	if report.BackupPath == "" {
		t.Fatal("a successful commit must produce a protected backup")
	}

	backupData, readErr := os.ReadFile(report.BackupPath)
	if readErr != nil {
		t.Fatalf("read backup: %v (path %q)", readErr, report.BackupPath)
	}
	if string(backupData) != trivialDocument {
		t.Fatalf("the backup must hold the exact prior bytes: %q", string(backupData))
	}

	// Unrelated files are never pruned.
	if _, err = os.Stat(filepath.Join(filepath.Dir(path), "unrelated.txt")); err == nil {
		t.Log("unrelated survived (expected)")
	}

	entries, _ := os.ReadDir(filepath.Dir(path))
	bakCount := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			bakCount++
		}
	}
	if bakCount > 4 { // 1 new backup + 3 retained + unrelated naming scheme only
		t.Errorf("protected backups must be pruned to the newest three, found %d", bakCount)
	}
}

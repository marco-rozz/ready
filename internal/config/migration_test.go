package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const migrationFixture = `config_version: 1
llm:
  default: local::test
  providers:
    local:
      provider: ollama
      api: ollama
channels:
  cli:
    enabled: true
skills:
  path: authored-skills/
`

const (
	retentionFailure  = "retention"
	sourceEditFailure = "source_edit"
	syncCommitFailure = "sync_commit"
	windowsTestOS     = "windows"
)

type testMigration struct {
	version int
	up      func(*yaml.Node) error
}

var _ Migration = testMigration{}

func (m testMigration) Version() int        { return m.version }
func (m testMigration) Description() string { return fmt.Sprintf("synthetic migration %d", m.version) }

func (m testMigration) Up(doc *yaml.Node) error {
	if m.up != nil {
		return m.up(doc)
	}

	return nil
}

func migrationRegistry(t *testing.T, steps ...Migration) *MigrationRegistry {
	t.Helper()
	r, err := NewMigrationRegistry(1+len(steps), steps...)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

func migrationSource(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "house.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func sourceBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func TestConfigVersionRejectsInvalidDocuments(t *testing.T) {
	for _, input := range []string{
		"llm: {}", "config_version: null", "config_version: '1'", "config_version: 1.1",
		"config_version: true", "config_version: 0", "config_version: -1",
		"config_version: 9999999999999999999999999999999999999", "config_version: 1\nconfig_version: 1",
		"[1, 2]", "config_version: 1\n---\nconfig_version: 1", "config_version: [",
	} {
		t.Run(input, func(t *testing.T) {
			path := migrationSource(t, input)
			_, err := Load(context.Background(), path)
			if !errors.Is(err, ErrInvalidConfigDocument) {
				t.Fatalf("error = %v", err)
			}
			if string(sourceBytes(t, path)) != input {
				t.Fatal("invalid input modified")
			}
		})
	}
	// Check version before decoding incompatible current struct fields.
	for _, tc := range []struct {
		version, target int
		want            error
	}{
		{2, 1, ErrIncompatibleVersion}, {1, 2, ErrMigrationRequired},
	} {
		_, err := decodeConfig(context.Background(), fmt.Appendf(nil, "config_version: %d\nllm: []", tc.version), "a 'quoted $path.yaml", tc.target)
		var mismatch *VersionError
		if !errors.Is(err, tc.want) || !errors.As(err, &mismatch) {
			t.Fatalf("error = %v", err)
		}
		if mismatch.Current != tc.version || mismatch.Target != tc.target {
			t.Fatalf("mismatch = %+v", mismatch)
		}
	}
}

func TestConfigVersionNoOpIsReadOnly(t *testing.T) {
	path := migrationSource(t, migrationFixture)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != windowsTestOS {
		if err = os.Chmod(filepath.Dir(path), 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
	}
	r, err := ProductionMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		cfg, report, prepErr := r.Prepare(context.Background(), path, nil)
		if prepErr != nil || cfg.ConfigVersion != 1 || report.Committed {
			t.Fatalf("prepare = %v, %+v", prepErr, report)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("current-schema read changed filesystem")
	}
	if string(sourceBytes(t, path)) != migrationFixture {
		t.Fatal("rewritten current config")
	}
}

func TestMigrationRegistryRequiresCompleteChain(t *testing.T) {
	for _, tc := range []struct {
		target int
		steps  []Migration
	}{
		{0, nil},
		{2, nil},
		{2, []Migration{nil}},
		{2, []Migration{testMigration{version: 3}}},
		{3, []Migration{testMigration{version: 2}, testMigration{version: 2}}},
		{2, []Migration{testMigration{version: 2}, testMigration{version: 3}}},
	} {
		if _, err := NewMigrationRegistry(tc.target, tc.steps...); !errors.Is(err, ErrInvalidMigrationRegistry) {
			t.Fatalf("target %d error = %v", tc.target, err)
		}
	}
	if r, err := ProductionMigrations(); err != nil || len(r.steps) != 0 {
		t.Fatalf("production registry = %+v, %v", r, err)
	}
}

func TestConfigVersionShippedExample(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	data := sourceBytes(t, path)
	_, version, err := parseVersion(data, path)
	if err != nil || version != TargetConfigVersion || !bytes.HasPrefix(data, []byte("config_version: 1")) {
		t.Fatalf("shipped example version = %d, %v", version, err)
	}
}

func TestMigrationPreservesSourcePermissions(t *testing.T) {
	if runtime.GOOS == windowsTestOS {
		t.Skip("Unix mode bits; Windows DACL has a separate test")
	}
	path := migrationSource(t, migrationFixture)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	_, report, err := migrationRegistry(t, testMigration{version: 2}).Prepare(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]os.FileMode{path: 0o640, report.BackupPath: 0o600} {
		info, statErr := os.Stat(file)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s = %o; want %o", file, info.Mode().Perm(), want)
		}
	}
}

func TestMigrationChainPreservesDocumentAndOriginalBackup(t *testing.T) {
	input := migrationFixture + `# operator notes
old_name: 'quoted value' # attached comment
custom: &settings
  enabled: false
  count: 0
  empty: ""
copy:
  <<: *settings
`
	path := migrationSource(t, input)
	var order []int
	var steps []Migration
	for version := 2; version <= 4; version++ {
		steps = append(steps, testMigration{version: version, up: func(doc *yaml.Node) error {
			order = append(order, version)
			if version == 2 {
				return renameSyntheticKey(doc, "old_name", "new_name")
			}

			return nil
		}})
	}
	r := migrationRegistry(t, steps...)
	cfg, report, err := r.Prepare(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != "[2 3 4]" || !report.Committed || len(report.Steps) != 3 || cfg.ConfigVersion != 4 {
		t.Fatalf("order/report = %v %+v", order, report)
	}
	if string(sourceBytes(t, report.BackupPath)) != input {
		t.Fatal("backup is not exact original")
	}
	output := string(sourceBytes(t, path))
	for _, want := range []string{"config_version: 4", "# operator notes", "new_name: 'quoted value' # attached comment", "&settings", "*settings", "enabled: false", "count: 0", `empty: ""`, "path: authored-skills/"} {
		if !strings.Contains(output, want) {
			t.Errorf("output lost %q: %s", want, output)
		}
	}
	if !strings.HasPrefix(output, "config_version: 4\n") || strings.Contains(output, filepath.Dir(path)) || strings.Contains(output, "failover_retries") {
		t.Fatal("runtime values leaked into document")
	}
	if cfg.Skills.Path != filepath.Join(filepath.Dir(path), "authored-skills")+"/" {
		t.Fatalf("runtime path = %q", cfg.Skills.Path)
	}
	if runtime.GOOS != windowsTestOS {
		info, statErr := os.Stat(report.BackupPath)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("backup permissions: %v %v", info, statErr)
		}
	}
	// A second preparation is byte-identical and produces no backup.
	_, again, err := r.Prepare(context.Background(), path, nil)
	if err != nil || again.Committed || again.BackupPath != "" || output != string(sourceBytes(t, path)) {
		t.Fatalf("second prepare = %+v %v", again, err)
	}
}

func TestMigrationValidationFailureAndCancellationPreserveSource(t *testing.T) {
	for _, mode := range []string{"step", "validation", "cancel", "root"} {
		t.Run(mode, func(t *testing.T) {
			path := migrationSource(t, migrationFixture)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			step := testMigration{version: 2, up: func(doc *yaml.Node) error {
				switch mode {
				case "step":
					return errors.New("synthetic refusal: private-value")
				case "cancel":
					cancel()
				case "root":
					doc.Content[0].Kind = yaml.SequenceNode
				}

				return nil
			}}
			_, report, err := migrationRegistry(t, step).Prepare(ctx, path, func(*Config) error { return errors.New("local configuration rejected") })
			if err == nil || report.Committed || report.BackupPath != "" {
				t.Fatalf("report/error = %+v %v", report, err)
			}
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("step error leaked configuration value")
			}
			if string(sourceBytes(t, path)) != migrationFixture {
				t.Fatal("failed preparation modified config")
			}
		})
	}
}

// The synthetic step makes the author-defined rename conflict policy explicit.
func renameSyntheticKey(doc *yaml.Node, from, to string) error {
	root := doc.Content[0]
	var source *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == to {
			return errors.New("rename destination already exists")
		}
		if root.Content[i].Value == from {
			source = root.Content[i]
		}
	}
	if source != nil {
		source.Value = to
	}

	return nil
}

func TestMigrationRenameConflictPreservesBothValues(t *testing.T) {
	input := migrationFixture + "old_name: original\nnew_name: operator-choice\n"
	path := migrationSource(t, input)
	step := testMigration{version: 2, up: func(doc *yaml.Node) error { return renameSyntheticKey(doc, "old_name", "new_name") }}
	_, report, err := migrationRegistry(t, step).Prepare(context.Background(), path, nil)
	if err == nil || report.Committed || report.BackupPath != "" {
		t.Fatalf("rename conflict: %+v %v", report, err)
	}
	if string(sourceBytes(t, path)) != input {
		t.Fatal("conflicting values were overwritten")
	}
}

func TestMigrationFailureBoundaries(t *testing.T) {
	for _, mode := range []string{"backup", "temp", "replace", sourceEditFailure, "sync_backup", syncCommitFailure, retentionFailure} {
		t.Run(mode, func(t *testing.T) {
			path := migrationSource(t, migrationFixture)
			fs := migrationIO{replace: replaceConfig, syncDir: syncConfigDir}
			injected := errors.New("injected filesystem failure")
			writeCount, syncCount := 0, 0
			fs.write = func(ctx context.Context, src, pattern string, data []byte, perm os.FileMode) (string, error) {
				writeCount++
				if mode == "backup" && writeCount == 1 || mode == "temp" && writeCount == 2 {
					return "", injected
				}
				if mode == sourceEditFailure && writeCount == 2 {
					if err := os.WriteFile(path, []byte(migrationFixture+"# external edit\n"), 0o600); err != nil {
						return "", err
					}
				}

				return writeMigrationFile(ctx, src, pattern, data, perm)
			}
			fs.syncDir = func(ctx context.Context, dir string) error {
				syncCount++
				if mode == "sync_backup" && syncCount == 1 || mode == syncCommitFailure && syncCount == 2 {
					return injected
				}

				return syncConfigDir(ctx, dir)
			}
			if mode == "replace" {
				fs.replace = func(context.Context, string, string) error { return injected }
			}
			if mode == retentionFailure {
				fs.prune = func(context.Context, string) error { return injected }
			}
			_, report, err := migrationRegistry(t, testMigration{version: 2}).prepare(context.Background(), path, nil, fs)
			committed := mode == syncCommitFailure || mode == retentionFailure
			if report.Committed != committed {
				t.Fatalf("commit = %+v, %v", report, err)
			}
			if mode == retentionFailure {
				if err != nil || len(report.Warnings) != 1 {
					t.Fatalf("retention = %+v %v", report, err)
				}
			} else if err == nil {
				t.Fatal("failure not returned")
			}
			if mode == sourceEditFailure && !errors.Is(err, ErrConfigSourceConflict) {
				t.Fatalf("conflict error = %v", err)
			}
			assertMigrationOutcome(t, path, mode, committed)
			entries, readErr := os.ReadDir(filepath.Dir(path))
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".house.yaml.migration-") {
					t.Fatal("temporary file leaked")
				}
			}
		})
	}
}

func assertMigrationOutcome(t *testing.T, path, mode string, committed bool) {
	t.Helper()
	output := string(sourceBytes(t, path))
	switch {
	case committed:
		if !strings.Contains(output, "config_version: 2") {
			t.Fatal("commit was reverted")
		}
	case mode == sourceEditFailure:
		if !strings.HasSuffix(output, "# external edit\n") {
			t.Fatal("external edit lost")
		}
	default:
		if output != migrationFixture {
			t.Fatal("source modified before commit")
		}
	}
}

func TestMigrationConcurrentWritersAndLockCancellation(t *testing.T) {
	path := migrationSource(t, migrationFixture)
	var calls atomic.Int32
	r := migrationRegistry(t, testMigration{version: 2, up: func(*yaml.Node) error {
		calls.Add(1)

		return nil
	}})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, _, err := r.Prepare(context.Background(), path, nil); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("migration ran %d times", calls.Load())
	}
	lock, err := acquireMigrationLock(context.Background(), path+".migration.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err = acquireMigrationLock(ctx, path+".migration.lock"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait = %v", err)
	}
}

func TestMigrationRejectsLinkedSource(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path := migrationSource(t, migrationFixture)
			linked := filepath.Join(filepath.Dir(path), "linked.yaml")
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(path, linked); err != nil {
				t.Skipf("links unavailable: %v", err)
			}
			_, report, err := migrationRegistry(t, testMigration{version: 2}).Prepare(context.Background(), linked, nil)
			if err == nil || report.Committed {
				t.Fatalf("linked preparation: %+v %v", report, err)
			}
			if string(sourceBytes(t, path)) != migrationFixture {
				t.Fatal("linked source modified")
			}
			if _, err = Load(context.Background(), linked); err != nil {
				t.Fatalf("current linked read: %v", err)
			}
		})
	}
}

func TestMigrationBackupRetentionIsScoped(t *testing.T) {
	path := migrationSource(t, migrationFixture)
	dir := filepath.Dir(path)
	for _, base := range []string{"house.yaml", "office.yaml"} {
		for i := range 5 {
			name := fmt.Sprintf("%s.bak.2026010%dT120000.000000000Z.123456789", base, i+1)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("old backup"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	other := filepath.Join(dir, "house.yaml.bak.user-copy")
	if err := os.WriteFile(other, []byte("manual"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, report, err := migrationRegistry(t, testMigration{version: 2}).Prepare(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	houses, _ := filepath.Glob(filepath.Join(dir, "house.yaml.bak.*"))
	offices, _ := filepath.Glob(filepath.Join(dir, "office.yaml.bak.*"))
	if len(houses) != 4 || len(offices) != 5 || string(sourceBytes(t, other)) != "manual" {
		t.Fatalf("retention house=%v office=%v report=%+v", houses, offices, report)
	}
}

func TestMigrationDoesNotPersistSecretOrValidatorOverrides(t *testing.T) {
	secret := strings.Repeat("s", 40)
	path := migrationSource(t, strings.ReplaceAll(migrationFixture, "  cli:\n    enabled: true", "  zmqcli:\n    enabled: true\n    auth_token_file: token"))
	tokenPath := filepath.Join(filepath.Dir(path), "token")
	if err := os.WriteFile(tokenPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	input := strings.ReplaceAll(string(sourceBytes(t, path)), "auth_token_file: token", "auth_token_file: "+filepath.ToSlash(tokenPath))
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := migrationRegistry(t, testMigration{version: 2}).Prepare(context.Background(), path, func(cfg *Config) error {
		cfg.MCPProfile = "runtime-only"

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	output := sourceBytes(t, path)
	if bytes.Contains(output, []byte(secret)) || bytes.Contains(output, []byte("runtime-only")) || !bytes.Contains(output, []byte(filepath.ToSlash(tokenPath))) {
		t.Fatal("runtime values materialized")
	}
}

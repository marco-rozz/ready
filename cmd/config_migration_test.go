package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

type serveTestMigration struct{}

var _ config.Migration = serveTestMigration{}

func (serveTestMigration) Version() int          { return 2 }
func (serveTestMigration) Description() string   { return "synthetic future schema" }
func (serveTestMigration) Up(_ *yaml.Node) error { return nil }

func TestServeConfigPreparationValidatesBeforeCommit(t *testing.T) {
	registry, err := config.NewMigrationRegistry(2, serveTestMigration{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "headless", "bad-profile"} {
		t.Run(mode, func(t *testing.T) {
			input := minimalOAuthConfigYAML(filepath.Join(t.TempDir(), "credentials"))
			if mode != "headless" {
				input += "\nchannels:\n  cli:\n    enabled: true\n"
			}
			path := writeTempConfig(t, input)
			opts := ServeOptions{}
			if mode == "bad-profile" {
				opts.Profile = "nonexistent-profile"
			}
			_, report, prepErr := prepareServeConfig(context.Background(), path, opts, registry)
			if mode == "valid" {
				if prepErr != nil || !report.Committed {
					t.Fatalf("preparation = %+v, %v", report, prepErr)
				}
				var logs bytes.Buffer
				logConfigMigration(zerolog.New(&logs), report)
				if !strings.Contains(logs.String(), "config migration committed") || !strings.Contains(logs.String(), report.BackupPath) {
					t.Fatalf("logs = %s", logs.String())
				}
			} else {
				if prepErr == nil || report.Committed {
					t.Fatalf("preparation = %+v, %v", report, prepErr)
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil || string(data) != input {
					t.Fatalf("failed validation changed config: %v", readErr)
				}
			}
		})
	}
}

func TestLoginModesRejectMigrationBeforeAnyOtherAction(t *testing.T) {
	for _, flags := range [][]string{nil, {flagStatus}, {flagForget}, {flagDeviceCode}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a 'quoted $file.yaml")
			if err := os.WriteFile(path, []byte("config_version: 1\nllm: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			command := newTestLoginCommand(&stdout, &stderr)
			command.Action = func(ctx context.Context, c *cli.Command) error {
				return runLoginWithConfig(ctx, c, strings.NewReader(""), &stdout, &stderr,
					func(_ context.Context, elected string, _ io.Writer) (*config.Config, error) {
						// The read-only loader's real version error, for a future binary
						// targeting 2. The compiled production target stays at 1.
						return nil, &config.VersionError{Path: elected, Current: 1, Target: 2}
					})
			}
			args := append([]string{loginLabel, flagConfig, path}, flags...)
			args = append(args, "llm:missing-target")
			err := command.Run(context.Background(), args)
			var exit cli.ExitCoder
			if !errors.As(err, &exit) || exit.ExitCode() != ExitErr {
				t.Fatalf("exit = %v", err)
			}
			for _, want := range []string{"config_version 1", "target 2", config.ServeMigrationCommand(path), "start the daemon"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("missing %q in %s", want, stderr.String())
				}
			}
			entries, readErr := os.ReadDir(filepath.Dir(path))
			if readErr != nil || len(entries) != 1 {
				t.Fatalf("login wrote migration/credential files: %v %v", entries, readErr)
			}
		})
	}
}

func TestServeRejectsUnversionedElectedConfigBeforeRuntime(t *testing.T) {
	path := writeTempConfig(t, "llm: []\n")
	err := Serve(context.Background(), ServeOptions{ConfigPath: path})
	if !errors.Is(err, config.ErrInvalidConfigDocument) {
		t.Fatalf("serve error = %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("serve started runtime: %v %v", entries, readErr)
	}
}

func TestServeDoesNotFallBackAfterElectedConfigFailure(t *testing.T) {
	workdir, home := t.TempDir(), t.TempDir()
	t.Chdir(workdir)
	t.Setenv("HOME", home)
	local := filepath.Join(workdir, ".ready", "config.yaml")
	global := filepath.Join(home, ".ready", "config.yaml")
	input := minimalOAuthConfigYAML(filepath.Join(home, "credentials"))
	writeFile(t, local, "llm: []\n")
	writeFile(t, global, input)
	err := Serve(context.Background(), ServeOptions{})
	if !errors.Is(err, config.ErrInvalidConfigDocument) {
		t.Fatalf("serve = %v", err)
	}
	data, readErr := os.ReadFile(global)
	if readErr != nil || string(data) != input {
		t.Fatal("rejected global config was modified")
	}
	entries, readErr := os.ReadDir(filepath.Dir(global))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("global runtime or migration started: %v %v", entries, readErr)
	}
}

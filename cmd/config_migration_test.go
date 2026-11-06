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
	"github.com/marco-rozz/ready/internal/daemonlock"
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
			// A v1 source that the synthetic step migrates to 2.
			input := strings.Replace(withTestAgent(minimalOAuthConfigYAML()), "config_version: 2", "config_version: 1", 1)
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
				if readErr != nil || string(data) != withTestAgent(input) {
					t.Fatalf("failed validation changed config: %v", readErr)
				}
			}
		})
	}
}

func TestLoginModesRejectMigrationBeforeAnyOtherAction(t *testing.T) {
	for _, flags := range [][]string{nil, {flagStatus}, {flagForget}, {flagDeviceCode}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			path := filepath.Join(writeTempAgentDir(t, "config_version: 1\nllm: []\n"), "a 'quoted $file.yaml")
			if err := os.WriteFile(path, []byte("config_version: 1\nllm: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			command := newTestLoginCommand(&stdout, &stderr)
			command.Action = func(ctx context.Context, c *cli.Command) error {
				return runLoginWithConfig(ctx, c, strings.NewReader(""), &stdout, &stderr,
					func(_ context.Context, agentDir string, _ io.Writer) (*config.Config, error) {
						// The read-only loader's real version error, for a future binary
						// targeting 2. The compiled production target stays at 1.
						return nil, &config.VersionError{Path: filepath.Join(agentDir, config.ConfigFileName), Current: 1, Target: 2}
					})
			}
			args := append([]string{loginLabel, flagAgentDir, filepath.Dir(path)}, flags...)
			args = append(args, "llm:missing-target")
			err := command.Run(context.Background(), args)
			var exit cli.ExitCoder
			if !errors.As(err, &exit) || exit.ExitCode() != ExitErr {
				t.Fatalf("exit = %v", err)
			}
			for _, want := range []string{"config_version 1", "target 2", config.ServeMigrationCommand(filepath.Dir(path)), "start the daemon"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("missing %q in %s", want, stderr.String())
				}
			}
			entries, readErr := os.ReadDir(filepath.Dir(path))
			if readErr != nil {
				t.Fatalf("read the agent directory: %v", readErr)
			}
			for _, e := range entries {
				name := e.Name()
				if name == configFileName || name == filepath.Base(path) {
					continue
				}
				t.Errorf("login wrote a migration/credential artifact: %s", name)
			}
		})
	}
}

func TestServeRejectsUnversionedElectedConfigBeforeRuntime(t *testing.T) {
	path := writeTempConfig(t, "llm: []\n")
	err := Serve(context.Background(), ServeOptions{AgentDir: filepath.Dir(path)})
	if !errors.Is(err, config.ErrInvalidConfigDocument) {
		t.Fatalf("serve error = %v", err)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		name := e.Name()
		if name == configFileName || name == daemonlock.OwnershipFile || name == daemonlock.MaintenanceFile {
			continue
		}
		t.Errorf("serve started runtime: artifact %s", name)
	}
}

// Serve never falls back: an explicit agent directory failing to load neither
// reads the default editor nor creates any runtime artifact of its own.
func TestServeDoesNotFallBackAfterElectedConfigFailure(t *testing.T) {
	workdir, home := t.TempDir(), t.TempDir()
	t.Chdir(workdir)
	t.Setenv("HOME", home)
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())
	writeFile(t, filepath.Join(agentDir, "config.yaml"), "llm: []\n")
	err := Serve(context.Background(), ServeOptions{AgentDir: agentDir})
	if !errors.Is(err, config.ErrInvalidConfigDocument) {
		t.Fatalf("serve = %v", err)
	}
	// The default agent was never read, and no runtime artifact
	// (oauth store, sessions, logs) appeared in the failing directory; the
	// two stable lock files are owned by design and expected.
	entries, readErr := os.ReadDir(agentDir)
	if readErr != nil {
		t.Fatalf("read the failed agent directory: %v", readErr)
	}
	for _, e := range entries {
		name := e.Name()
		if name == configFileName || name == daemonlock.OwnershipFile || name == daemonlock.MaintenanceFile {
			continue
		}
		t.Errorf("failed agent directory grew a runtime artifact: %s", name)
	}
}

// markeredConfigYAML returns a valid current-schema config whose ZMQ shared
// secret holds an unresolved credential marker (a manual TODO variant).
func markeredConfigYAML() string {
	return `
config_version: 2
llm:
  default: "ollama::test"
  providers:
    ollama:
      provider: ollama
      api: ollama
channels:
  zmqcli:
    enabled: true
    bind: tcp://127.0.0.1:7600
    auth_token: literal-canary-value
`
}

// TestServePreparationRefusesLiteralCredentials verifies the
// cli-bootstrap delta: ordinary serve preparation rejects a current-schema
// config holding reserved REDACTED- markers BEFORE any migration, backup or
// config write; the file keeps its bytes and no backup appears.
func TestServePreparationRefusesLiteralCredentials(t *testing.T) {
	input := markeredConfigYAML()
	path := writeTempConfig(t, input)
	dir := filepath.Dir(path)

	_, report, err := prepareServeConfig(context.Background(), path, ServeOptions{}, nil)
	if err == nil || report.Committed {
		t.Fatalf("preparation must refuse unresolved markers, got report=%+v err=%v", report, err)
	}
	if !errors.Is(err, config.ErrInvalidSecretRef) {
		t.Fatalf("refusal must wrap the marker sentinel, got: %v", err)
	}
	if !strings.Contains(err.Error(), "channels.zmqcli.auth_token") {
		t.Fatalf("refusal must name the affected field, got: %v", err)
	}
	if strings.Contains(err.Error(), "literal-canary-value") {
		t.Fatalf("refusal must never echo the literal, got: %v", err)
	}

	// File unchanged, no backup/lock artifacts beyond the stable lock file.
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != withTestAgent(input) {
		t.Fatalf("refusal must not touch the config: %v", readErr)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			t.Errorf("no backup may be created on refusal, found %s", e.Name())
		}
	}
}

// TestServePreparationRefusesDisabledSubsystemLiterals verifies markers in a
// DISABLED integration are refused all the same.
func TestServePreparationRefusesDisabledSubsystemLiterals(t *testing.T) {
	input := `
config_version: 2
llm:
  default: "ollama::test"
  providers:
    ollama:
      provider: ollama
      api: ollama
channels:
  cli:
    enabled: true
home_assistant:
  enabled: false
  websocket:
    url: ws://homeassistant.local:8123
    token: literal-canary-value
`
	path := writeTempConfig(t, input)
	_, report, err := prepareServeConfig(context.Background(), path, ServeOptions{}, nil)
	if err == nil || !errors.Is(err, config.ErrInvalidSecretRef) {
		t.Fatalf("legacy token in a disabled subsystem must refuse, got report=%+v err=%v", report, err)
	}
	if !strings.Contains(err.Error(), "home_assistant.websocket.token") {
		t.Fatalf("refusal must name the field, got: %v", err)
	}
}

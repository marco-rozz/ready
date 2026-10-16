package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The chain: without a --config flag, login elects the config serve would
// (local ./.ready/config.yaml preferred over the global candidate) and
// announces it as its first output line — the login↔daemon config mismatch
// is visible at enumeration.
func TestLogin_ElectsChainWithoutConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(wd, ".ready", "config.yaml")
	writeFile(t, localPath, minimalOAuthConfigYAML(t.TempDir()))
	t.Cleanup(func() { osRemoveAll(t, filepath.Join(wd, ".ready")) })

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	_ = cmd.Run(context.Background(), []string{loginLabel, flagStatus, unknownTargetForChain})

	firstLine, _, _ := strings.Cut(stdout.String(), "\n")
	// Elect returns the local candidate in its literal cwd-relative form.
	localForm := "./" + configLocalRel
	if firstLine != "using config "+localForm {
		t.Errorf("first line = %q, want %q (the local candidate of the chain)", firstLine, "using config "+localForm)
	}
}

// A target missing from the elected config fails naming the elected config
// path — the wrong-chain election is diagnosable from the message alone.
func TestLogin_UnknownTargetNamesElectedConfig(t *testing.T) {
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(t.TempDir()))

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	_ = cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus, unknownTargetForChain})

	if !strings.Contains(stderr.String(), "elected config") {
		t.Errorf("stderr = %q, want the elected config path named", stderr.String())
	}
	if !strings.Contains(stderr.String(), configPath) {
		t.Errorf("stderr = %q, want %q in the failure", stderr.String(), configPath)
	}
}

// configLocalRel mirrors config.LocalConfigRel (dot-prefixed literal the
// chain returns for a local election).
const configLocalRel = ".ready/config.yaml"

// writeFile is shared with the chain tests; osRemoveAll keeps the t.Cleanup
// calls readable.
func osRemoveAll(t *testing.T, dir string) {
	t.Helper()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}

// writeFile creates the file (with parents) for the chain election tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const unknownTargetForChain = "not-in-config"

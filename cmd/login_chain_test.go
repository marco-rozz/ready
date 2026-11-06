package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The selection: without --agent-dir, login selects the same directory serve
// would (unconditionally ~/.ready, independent of cwd) and announces it as
// its first output line — the login↔daemon directory mismatch is visible at
// enumeration.
func TestLogin_SelectsDefaultAgentDirWithoutFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// A cwd-local candidate must be ignored entirely: selection is
	// cwd-independent, so a stray ./.ready stays unread.
	localPath := filepath.Join(wd, ".ready", "config.yaml")
	writeFile(t, localPath, minimalOAuthConfigYAML())
	t.Cleanup(func() { osRemoveAll(t, filepath.Join(wd, ".ready")) })

	defaultDir := filepath.Join(home, ".ready")
	defaultPath := filepath.Join(defaultDir, "config.yaml")
	writeFile(t, defaultPath, minimalOAuthConfigYAML())
	if physical, evalErr := filepath.EvalSymlinks(defaultDir); evalErr == nil {
		defaultDir = filepath.Clean(physical)
	}
	t.Cleanup(func() { osRemoveAll(t, defaultDir) })

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	_ = cmd.Run(context.Background(), []string{loginLabel, flagStatus, unknownTargetForChain})

	firstLine, _, _ := strings.Cut(stdout.String(), "\n")
	if firstLine != "using agent directory "+defaultDir {
		t.Errorf("first line = %q, want %q (the default agent directory)", firstLine, "using agent directory "+defaultDir)
	}
}

// A target missing from the selected config fails naming the selected agent
// directory — the wrong-directory selection is diagnosable from the message
// alone.
func TestLogin_UnknownTargetNamesSelectedAgentDir(t *testing.T) {
	agentDir := writeTempAgentDir(t, minimalOAuthConfigYAML())

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	_ = cmd.Run(context.Background(), []string{loginLabel, flagAgentDir, agentDir, flagStatus, unknownTargetForChain})

	if !strings.Contains(stderr.String(), "unknown") {
		t.Errorf("stderr = %q, want the unknown-target failure", stderr.String())
	}
}

// writeFile is shared with the chain tests; osRemoveAll keeps the t.Cleanup
// calls readable.
func osRemoveAll(t *testing.T, dir string) {
	t.Helper()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
}

// writeFile creates the file (with parents) for the exit tests.
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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitRefusalPrintsDiagnostic(t *testing.T) {
	// A regular file cannot be an agent directory; this fails before any
	// terminal input and exercises the real dispatcher error rendering.
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code, err := run(t.Context(), []string{homeAgent, initLabel, "--agent-dir", path}, &stderr)
	if code != ExitErr || err == nil || strings.TrimSpace(stderr.String()) == "" {
		t.Fatalf("init refusal must print its reason: code=%d err=%v stderr=%q", code, err, stderr.String())
	}
}

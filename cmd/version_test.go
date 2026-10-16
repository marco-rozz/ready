package main

import (
	"bytes"
	"strings"
	"testing"
)

// versionString renders the canonical line even on a dev build without
// ldflags: the placeholders are non-empty and the rendering is stable.
func TestVersionStringDevBuild(t *testing.T) {
	out := versionString()
	if !strings.HasPrefix(out, homeAgent+" ") {
		t.Errorf("versionString = %q, want prefix %q", out, homeAgent+" ")
	}
	if !strings.Contains(out, "(commit ") || !strings.Contains(out, "built ") {
		t.Errorf("versionString = %q, want the (commit <sha>, built <date>) tail", out)
	}
	if !strings.HasSuffix(out, ")") {
		t.Errorf("versionString = %q, want a trailing parenthesis", out)
	}
}

// --version prints the version to stdout and exits 0 without running any
// subcommand runtime (cli-bootstrap: the version request never starts a
// runtime).
func TestVersionFlagPrintsAndExitsOK(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	code, err := runCapture(t, []string{homeAgent, "--version"}, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != ExitOK {
		t.Errorf("exit code = %d, want %d", code, ExitOK)
	}

	if !strings.Contains(stdout.String(), homeAgent+" ") || strings.Contains(stdout.String(), "missing subcommand") {
		t.Errorf("stdout = %q, want the canonical version line", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// The bare-invocation hard error must stay intact next to the version flag
// (cli-bootstrap: no-subcommand invocation writes the exact message and
// exits 1).
func TestBareInvocationStillFailsNextToVersion(t *testing.T) {
	stderr := &bytes.Buffer{}

	code, _ := run(t.Context(), []string{homeAgent}, stderr)
	if code != ExitErr {
		t.Errorf("exit code = %d, want %d", code, ExitErr)
	}
	if !strings.Contains(stderr.String(), noSubcommandMessage) {
		t.Errorf("stderr = %q, want %q", stderr.String(), noSubcommandMessage)
	}

	// Flags without a subcommand fail before the root Action even runs
	// (urfave rejects the undefined root flag), but the exit code contract
	// stays 1 for a bare-invocation with global flags.
	stderr.Reset()
	code, _ = run(t.Context(), []string{"ready", "--config", "foo.yaml"}, stderr)
	if code != ExitErr {
		t.Errorf("flags without subcommand must still fail; code=%d stderr=%q", code, stderr.String())
	}
}

// runCapture runs the dispatcher with the command writer pointed at stdout
// so the version printer's output is capturable alongside the error stream.
func runCapture(t *testing.T, args []string, stdout, stderr *bytes.Buffer) (int, error) {
	t.Helper()

	root := newRootCommand(stderr)
	root.Writer = stdout

	err := root.Run(t.Context(), args)
	if err == nil {
		return ExitOK, nil
	}

	return exitCodeFor(err), err
}

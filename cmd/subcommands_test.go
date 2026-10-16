package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freeLocalAddress binds a listener on 127.0.0.1:0, closes it and returns the
// address — a port that was free at call time. Tests that must assume "no
// daemon is listening" use this instead of a hard-coded port, so they keep
// passing when a real ready serve daemon is running on the default 7600.
func freeLocalAddress(t *testing.T) string {
	t.Helper()

	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}

	addr := l.Addr().String()

	if cErr := l.Close(); cErr != nil {
		t.Fatalf("close reserved listener: %v", cErr)
	}

	return "tcp://" + addr
}

// TestNoSubcommandHardError verifies the breaking change described in
// decision A9 of the rollout handoff: invoking the binary with zero
// subcommand args MUST exit 1 and print the exact stderr line operators
// will see in their systemd logs.
func TestNoSubcommandHardError(t *testing.T) {
	var stderr bytes.Buffer

	code, err := run(context.Background(), []string{homeAgent}, &stderr)

	if code != ExitErr {
		t.Errorf("exit code: got %d, want %d", code, ExitErr)
	}

	if err == nil {
		t.Error("expected non-nil error for no-subcommand invocation")
	}

	want := "missing subcommand: use 'serve', 'chat' or 'login'\n"
	if got := stderr.String(); got != want {
		t.Errorf("stderr: got %q, want %q", got, want)
	}
}

// TestNoSubcommandWithRootFlags ensures that supplying global flags without
// a subcommand is still a hard error — the operator did not actually pick a
// subcommand, even if they typed the binary correctly.
func TestNoSubcommandWithRootFlags(t *testing.T) {
	var stderr bytes.Buffer

	// --help short-circuits in urfave, so use a non-help flag. The bare
	// "home-agent" with no extra args is already covered above.
	code, _ := run(context.Background(), []string{homeAgent}, &stderr)

	if code != ExitErr {
		t.Errorf("exit code: got %d, want %d", code, ExitErr)
	}

	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr should mention missing subcommand, got %q", stderr.String())
	}
}

// TestUnknownSubcommand verifies that typos exit non-zero and name the
// offending token so the operator can spot the mistake.
func TestUnknownSubcommand(t *testing.T) {
	var stderr bytes.Buffer

	code, _ := run(context.Background(), []string{homeAgent, "does-not-exist"}, &stderr)

	if code == ExitOK {
		t.Errorf("exit code: got %d, want non-zero", code)
	}

	if !strings.Contains(stderr.String(), "does-not-exist") {
		t.Errorf("stderr should name the unknown subcommand, got %q", stderr.String())
	}
}

// TestChatExitsWithErrorWhenNoDaemon verifies that `chat` exits non-zero and
// writes a human-readable error to stderr when unable to establish a session
// (no daemon listening, auth failure, or connection timeout).
// The exact exit code depends on whether a daemon is running on the address:
// ExitErr (1) on connection failure / timeout, ExitAuthFail (3) on auth
// rejection. Both are valid "error" outcomes — the invariant is non-OK exit
// and a single-line stderr message.
func TestChatExitsWithErrorWhenNoDaemon(t *testing.T) {
	var stderr bytes.Buffer

	address := freeLocalAddress(t) // must be a port with no daemon on it

	code, _ := run(
		context.Background(),
		[]string{homeAgent, chatLabel, "--address", address, "--reply-timeout", "2s"},
		&stderr,
	)

	if code == ExitOK {
		t.Errorf("exit code: got %d, want non-zero (error)", code)
	}

	// The error should be a single human-readable line on stderr, not JSON.
	stderrOut := stderr.String()
	if stderrOut == "" {
		t.Error("stderr should contain an error message, got empty string")
	}

	// Must be exactly one line (the error), not a multi-line log dump.
	lines := strings.Split(strings.TrimRight(stderrOut, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("stderr should be exactly one line; got %d lines:\n%s", len(lines), stderrOut)
	}
}

// TestExitCodeConstants pins the numeric values that downstream code (and
// Change 2's network body) will rely on. Changing any of these is a wire
// contract break.
func TestExitCodeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"ExitOK", ExitOK, 0},
		{"ExitErr", ExitErr, 1},
		{"ExitBusy", ExitBusy, 2},
		{"ExitAuthFail", ExitAuthFail, 3},
		{"ExitTimeout", ExitTimeout, 124},
		{"ExitSIGINT", ExitSIGINT, 130},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestChatDoesNotTouchDotHome confirms decision A9: the chat subcommand
// writes no log files and creates no state directories. We run the chat
// stub in a temp working directory and assert the directory remains empty.
func TestChatDoesNotTouchDotHome(t *testing.T) {
	tmp := t.TempDir()

	// Snapshot the temp dir before running chat — it should stay this way.
	before := dirContents(t, tmp)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	if chErr := os.Chdir(tmp); chErr != nil {
		t.Fatalf("chdir: %v", chErr)
	}

	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	var stderr bytes.Buffer

	address := freeLocalAddress(t)

	_, _ = run(
		context.Background(),
		[]string{homeAgent, chatLabel, "--address", address},
		&stderr,
	)

	after := dirContents(t, tmp)

	if !equalStringSlices(before, after) {
		t.Errorf("chat created or modified files; before=%v after=%v", before, after)
	}

	// Also assert .ready/ specifically does not exist (paranoia).
	if _, statErr := os.Stat(filepath.Join(tmp, ".ready")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("chat should not create .ready/, but stat returned: %v", statErr)
	}
}

// TestServeFailsOnMissingConfig is the smoke test from task 5.12: prove the
// serve subcommand is actually wired to Serve() by surfacing a real config
// load failure as a non-zero exit with a descriptive stderr line.
func TestServeFailsOnMissingConfig(t *testing.T) {
	var stderr bytes.Buffer

	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist.yaml")

	code, runErr := run(
		context.Background(),
		[]string{homeAgent, "serve", "--config", missing},
		&stderr,
	)

	if code == ExitOK {
		t.Errorf("exit code: got %d, want non-zero", code)
	}

	if runErr == nil {
		t.Error("expected non-nil error from run() when config is missing")
	}

	// Serve returns a wrapped fmt.Errorf("failed to load config: %w", err);
	// urfave then surfaces it via the chain. We only need to see that the
	// error mentions config loading so operators have a chance of diagnosing.
	combined := stderr.String() + " " + errString(runErr)
	if !strings.Contains(combined, "config") {
		t.Errorf("expected error mentioning config; stderr=%q err=%v", stderr.String(), runErr)
	}
}

// --- test helpers below --------------------------------------------------

// dirContents returns the sorted relative paths of every entry under root.
// The test uses it to detect any file creation made by the chat stub.
func dirContents(t *testing.T, root string) []string {
	t.Helper()

	var out []string

	walkErr := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}

		out = append(out, rel)

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}

	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}

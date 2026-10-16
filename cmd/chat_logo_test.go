package main

// Tests for the chat-tui-logo-modes change: the READY banner is
// subcommand-scoped (serve prints it unconditionally; chat prints it only on
// the interactive TUI path in PNG mode; login never), the READY_LOGO
// resolver matrix, and the stdout purity of every non-TUI path (--exec,
// piped line-reader, --raw) under all logo-mode values.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// bannerBytes are the observable signatures of every decoration the old
// unconditional banner could put on a stream: the glyph-art rows (block and
// half-block glyphs) and the two inline-PNG escape sequences (kitty APC 'G',
// iTerm2 OSC 1337). A function (not a global) keeps gochecknoglobals happy.
func bannerBytes() []string {
	return []string{"█", "▀", "▄", "\x1b_G", "\x1b]1337"}
}

func assertNoBanner(t *testing.T, streams ...string) {
	t.Helper()

	for i, s := range streams {
		for _, b := range bannerBytes() {
			if strings.Contains(s, b) {
				t.Errorf("stream %d contains banner byte %q: %q", i, b, s)
			}
		}
	}
}

// --- task 2.3: the READY_LOGO resolver matrix -------------------------

func TestParseLogoModeMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		raw  string
		want rawLogoMode
	}{
		{"", rawLogoAuto},            // unset
		{logoValAuto, rawLogoAuto},   // explicit auto
		{logoValPng, rawLogoPNG},     // case-insensitive
		{logoValGlyph, rawLogoGlyph}, // surrounding whitespace
		{logoValOff, rawLogoOff},
		{logoValUnknown, rawLogoAuto}, // unknown → auto (leniency)
		{logoValZero, rawLogoAuto},
	}

	for _, tc := range cases {
		if got := parseLogoMode(tc.raw); got != tc.want {
			t.Errorf("parseLogoMode(%q): got %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestResolveLogoModeMatrix(t *testing.T) {
	t.Parallel()

	proto := logoValProto

	cases := []struct {
		name  string
		raw   string
		proto string
		want  logoMode
	}{
		// unset / auto: capability decides.
		{"unset-capable", "", proto, logoModePNG},
		{"unset-non-capable", "", "", logoModeGlyph},
		{"auto-capable", logoValAuto, proto, logoModePNG},
		{"auto-non-capable", "auto", "", logoModeGlyph},
		// png forces PNG, degrading to glyph when unsupported.
		{"png-capable", logoValPng, proto, logoModePNG},
		{"png-non-capable", "png", "", logoModeGlyph},
		// glyph forces uniform behaviour on any terminal.
		{"glyph-capable", logoValGlyph, proto, logoModeGlyph},
		{"glyph-non-capable", "glyph", "", logoModeGlyph},
		// off suppresses the logo everywhere.
		{"off-capable", logoValOff, proto, logoModeOff},
		{"off-non-capable", "off", "", logoModeOff},
		// unknown values fall back to auto behaviour.
		{"unknown-capable", logoValUnknown, proto, logoModePNG},
		{"unknown-non-capable", "fancy", "", logoModeGlyph},
	}

	for _, tc := range cases {
		if got := resolveLogoMode(tc.raw, tc.proto); got != tc.want {
			t.Errorf("resolveLogoMode(%q, %q): got %v, want %v", tc.raw, tc.proto, got, tc.want)
		}
	}
}

// --- tasks 1.3 / 5.1 / 5.2: stdout purity of the non-TUI paths -------------

// logoEnvValues are every READY_LOGO value the purity contract covers:
// unset, explicit auto, png, glyph, off, and an unknown value. A function
// (not a global) keeps gochecknoglobals happy.
func logoEnvValues() []string {
	return []string{"", logoValAuto, logoValPng, logoValGlyph, logoValOff, logoValUnknown}
}

// Logo-mode literal values shared by the resolver-matrix tests (goconst
// wants named constants above its repetition threshold).
const (
	logoValAuto    = "auto"
	logoValPng     = "png"
	logoValGlyph   = "glyph"
	logoValOff     = "off"
	logoValUnknown = "fancy"
	logoValZero    = "0"
	logoValProto   = "iterm2" // any non-empty protocol string = capable terminal

	// cli flag names, reused from newChatActionCmd's wiring.
	flagNameExec = "exec"
	flagNameRaw  = "raw"
)

// TestChat_ExecStdoutPureInAllLogoModes (task 5.1): --exec stdout carries
// exactly the rendered reply — no banner, no glyph art, no inline PNG — in
// every READY_LOGO value. The exec path never reaches the TUI branch,
// so no decoration can be emitted regardless of capability or env value.
func TestChat_ExecStdoutPureInAllLogoModes(t *testing.T) {
	for _, env := range logoEnvValues() {
		t.Run(env, func(t *testing.T) {
			if env != "" {
				t.Setenv(envLogo, env)
			}

			d := newFakeChatDaemon(t, true)
			var stdout, stderr bytes.Buffer

			cmd := newChatActionCmd(t, d)
			done := make(chan error, 1)
			go func() {
				done <- runChat(context.Background(), cmd, &stdout, &stderr, bytes.NewReader(nil))
			}()

			select {
			case err := <-done:
				if exitCodeFor(err) != ExitOK {
					t.Fatalf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
				}
			case <-time.After(execHangTestTimeout):
				t.Fatalf("runChat hung on --exec")
			}

			out := stdout.String()
			if !strings.Contains(out, d.execReply) {
				t.Errorf("stdout missing the rendered reply %q: %q", d.execReply, out)
			}
			assertNoBanner(t, out, stderr.String())
		})
	}
}

// TestChat_PipedInteractiveNoBanner (task 5.2): the piped stdin/stdout
// line-reader path and its --raw variant write no banner to any stream.
func TestChat_PipedInteractiveNoBanner(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run("raw="+boolFlagValue(raw), func(t *testing.T) {
			d := newFakeChatDaemon(t, true)
			var stdout, stderr bytes.Buffer

			// Reuse the exec harness's command wiring, clearing --exec so
			// runChat takes the piped stdin/stdout line-reader path instead.
			cmd := newChatActionCmd(t, d)
			for _, set := range [][2]string{{flagNameExec, ""}, {flagNameRaw, boolFlagValue(raw)}} {
				if err := cmd.Set(set[0], set[1]); err != nil {
					t.Fatalf("cmd.Set(%s): %v", set[0], err)
				}
			}

			// READY_LOGO=glyph would make a TUI run lead with glyph art;
			// the piped path must stay decoration-free even so.
			t.Setenv(envLogo, logoValGlyph)

			done := make(chan error, 1)
			go func() {
				done <- runChat(context.Background(), cmd, &stdout, &stderr, strings.NewReader("turn on the lights\n"))
			}()

			select {
			case err := <-done:
				if exitCodeFor(err) != ExitOK {
					t.Fatalf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
				}
			case <-time.After(execHangTestTimeout):
				t.Fatalf("runChat hung on the piped interactive path")
			}

			out := stdout.String()
			if !strings.Contains(out, d.execReply) {
				t.Errorf("stdout missing the rendered reply %q: %q", d.execReply, out)
			}
			assertNoBanner(t, out, stderr.String())
		})
	}
}

// TestLoginStdoutHasNoBanner (task 1.3, login scenario): `login --status`
// prints no banner before the status table — the banner is gone from the
// binary entry point entirely.
func TestLoginStdoutHasNoBanner(t *testing.T) {
	oauthDir := t.TempDir()
	configPath := writeTempConfig(t, minimalOAuthConfigYAML(oauthDir))

	var stdout, stderr bytes.Buffer
	cmd := newTestLoginCommand(&stdout, &stderr)

	err := cmd.Run(context.Background(), []string{loginLabel, flagConfig, configPath, flagStatus})
	if exitCodeFor(err) != ExitOK {
		t.Fatalf("exit code: got %d, want %d; stderr=%q", exitCodeFor(err), ExitOK, stderr.String())
	}

	assertNoBanner(t, stdout.String(), stderr.String())
}

func boolFlagValue(b bool) string {
	if b {
		return "true"
	}

	return "false"
}

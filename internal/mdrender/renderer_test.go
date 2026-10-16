package mdrender

import (
	"os"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/privacy"
)

// homePath is used to build a redacting sanitizer in tests; it is resolved
// lazily so the test does not hardcode a user path.
func testSanitizer(t *testing.T) *privacy.Sanitizer {
	t.Helper()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	return privacy.New(map[string]string{home: "~"})
}

func TestSanitizeClosesOddFences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		odd   bool
	}{
		{"balanced", "```go\nfmt.Println(\"hi\")\n```\n", false},
		{"open one", "```go\nfmt.Println(\"hi\")\n", true},
		{"no fences", "hello world\n", false},
		{"indented open", "  ```\ncode\n", true},
		{"info string open", "```python\nprint(1)\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sanitize(tc.input)
			count := len(fenceRE.FindAllString(got, -1))
			if count%2 != 0 {
				t.Errorf("sanitize left an odd number of fences (%d) for %q", count, tc.input)
			}
			if tc.odd && !strings.Contains(got, "```") {
				t.Errorf("sanitize did not append a closing fence for %q", tc.input)
			}
		})
	}
}

func TestRedactAppliedBeforeRender(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	san := privacy.New(map[string]string{home: "~"})
	r := New(80, StyleNotty, san)

	md := "my config lives at " + home + "/.ready/config.yaml\n"
	out := r.Render(md)

	if strings.Contains(out, home) {
		t.Errorf("home prefix reached the rendered output (un-redacted): %q", out)
	}
	if !strings.Contains(out, "~") {
		t.Errorf("expected redacted placeholder ~ in output: %q", out)
	}
}

func TestRenderErrorFallbackIsRedacted(t *testing.T) {
	t.Parallel()

	san := testSanitizer(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}

	// Force a glamour error by constructing a renderer whose underlying
	// TermRenderer is nil but style is not Raw — simulate via a renderer that
	// fails. glamour itself rarely errors, so directly exercise the fallback
	// path by setting tr to nil while keeping a non-Raw style.
	r := New(80, StyleDark, san)
	r.tr = nil // simulate render-error degradation

	md := "path: " + home + "/secret\n"
	out := r.Render(md)

	if strings.Contains(out, home) {
		t.Errorf("fallback leaked the un-redacted home prefix: %q", out)
	}
	if !strings.Contains(out, "~") {
		t.Errorf("fallback lost the redacted placeholder: %q", out)
	}
}

func TestRawPassthroughNoGlamour(t *testing.T) {
	t.Parallel()

	san := testSanitizer(t)
	r := New(80, StyleRaw, san)

	md := "# Title\n\nsome **bold** text\n"
	out := r.Render(md)

	// Raw must not add ANSI styling; the markdown markers survive verbatim.
	if !strings.Contains(out, "**bold**") {
		t.Errorf("raw path altered the markdown text: %q", out)
	}
}

func TestNottyVsDarkLightOutput(t *testing.T) {
	t.Parallel()

	san := testSanitizer(t)
	md := "# Hello\n\nworld\n"

	notty := New(80, StyleNotty, san).Render(md)
	dark := New(80, StyleDark, san).Render(md)
	light := New(80, StyleLight, san).Render(md)

	// notty contains no ANSI colour escapes.
	if strings.Contains(notty, "\x1b[3") || strings.Contains(notty, "\x1b[9") {
		t.Errorf("notty style emitted colour escapes: %q", notty)
	}
	// dark and light are distinct renderings and both contain ANSI styling.
	if dark == light {
		t.Errorf("dark and light styles produced identical output")
	}
	if dark == notty {
		t.Errorf("dark style produced notty-like (unstyled) output")
	}
}

func TestRecreateOnWidthChange(t *testing.T) {
	t.Parallel()

	san := testSanitizer(t)
	r := New(80, StyleDark, san)

	if r.Width() != 80 {
		t.Fatalf("initial width = %d, want 80", r.Width())
	}

	r2 := r.Recreate(120)
	if r2.Width() != 120 {
		t.Errorf("recreated width = %d, want 120", r2.Width())
	}
	if r2.Style() != StyleDark {
		t.Errorf("recreated style = %v, want StyleDark", r2.Style())
	}
	// Original is unchanged.
	if r.Width() != 80 {
		t.Errorf("original width changed after recreate: %d", r.Width())
	}
}

func TestStyleForBackground(t *testing.T) {
	t.Parallel()

	if got := StyleForBackground(true); got != StyleDark {
		t.Errorf("StyleForBackground(true) = %v, want StyleDark", got)
	}

	if got := StyleForBackground(false); got != StyleLight {
		t.Errorf("StyleForBackground(false) = %v, want StyleLight", got)
	}
}

func TestOneShotWidthFallback(t *testing.T) {
	t.Parallel()

	// nil stdout → fallback 80.
	if got := OneShotWidth(nil); got != defaultWidth {
		t.Errorf("OneShotWidth(nil) = %d, want %d", got, defaultWidth)
	}

	// Non-TTY file (os.Stdin is not a TTY under the test runner) → fallback.
	if got := OneShotWidth(os.Stdin); got != defaultWidth && got <= 0 {
		t.Errorf("OneShotWidth(non-tty) = %d, want positive fallback", got)
	}
}

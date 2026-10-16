package mdrender

import (
	"os"
	"regexp"
	"strings"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	glamour "charm.land/glamour/v2"
)

// Style is the glamour style the caller has already resolved. glamour v2 has
// no WithAutoStyle, so mdrender performs no terminal queries itself; the
// caller picks Dark/Light/Notty/Raw and passes it to New.
type Style int

// Style values. Raw is a no-op passthrough (no glamour rendering at all).
const (
	StyleDark Style = iota
	StyleLight
	StyleNotty
	StyleRaw
)

// styleName maps a Style to the glamour standard-style string. Raw has no
// glamour style (the renderer is not constructed).
func (s Style) styleName() string {
	switch s {
	case StyleDark:
		return "dark"
	case StyleLight:
		return "light"
	case StyleNotty:
		return "notty"
	default:
		return ""
	}
}

// Renderer is a reusable glamour v2 TermRenderer wrapper. It is safe for use
// from a single goroutine (the TUI's Update/View path); callers re-create it
// on resize via Recreate rather than mutating it concurrently.
type Renderer struct {
	style     Style
	width     int
	sanitizer *privacy.Sanitizer
	tr        *glamour.TermRenderer // nil for StyleRaw
}

// defaultWidth is the fallback word-wrap width when the caller has no terminal
// size yet (e.g. before the first tea.WindowSizeMsg) or an invalid one.
const defaultWidth = 80

// New constructs a Renderer with the given word-wrap width, client-resolved
// style, and redaction sanitizer. The sanitizer may be nil (no-op redaction).
// For StyleRaw no glamour TermRenderer is built — Render returns the redacted,
// sanitised raw text.
func New(width int, style Style, sanitizer *privacy.Sanitizer) *Renderer {
	r := &Renderer{style: style, width: clampWidth(width), sanitizer: sanitizer}
	if style == StyleRaw {
		return r
	}

	tr, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style.styleName()),
		glamour.WithWordWrap(r.width),
	)
	if err != nil {
		// glamour.NewTermRenderer only fails on an unparseable style, and the
		// three standard styles are builtin — so this is unreachable in
		// practice. Keep the renderer usable by degrading to raw passthrough.
		r.style = StyleRaw

		return r
	}

	r.tr = tr

	return r
}

// Recreate returns a new Renderer with the same style and sanitizer but a new
// word-wrap width, for terminal resize. The old renderer is not closed (it has
// no open resources); callers simply replace their reference.
func (r *Renderer) Recreate(width int) *Renderer {
	return New(width, r.style, r.sanitizer)
}

// Style returns the renderer's resolved style.
func (r *Renderer) Style() Style { return r.style }

// Width returns the renderer's word-wrap width.
func (r *Renderer) Width() int { return r.width }

// Render applies redaction, markdown sanitisation, and glamour rendering to
// md, returning the rendered string. On a glamour render error it falls back
// to the already-redacted and sanitised raw text — never the un-redacted
// input, never a blank. For StyleRaw the redacted, sanitised text is returned
// with no glamour rendering.
func (r *Renderer) Render(md string) string {
	redacted := r.sanitizer.Redact(md)
	sanitised := sanitize(redacted)

	if r.style == StyleRaw || r.tr == nil {
		return sanitised
	}

	out, err := r.tr.Render(sanitised)
	if err != nil {
		// Render failure: show the redacted, sanitised raw text (D18).
		return sanitised
	}

	return out
}

// fenceRE matches a CommonMark fenced-code-block opening/closing delimiter
// line: optional leading whitespace, three or more backticks, optional info
// string. Counting matches across the buffer tells us whether the fences
// balance; an odd count means a block is still open.
var fenceRE = regexp.MustCompile(`(?m)^[ \t]*` + "```")

// sanitize closes an open (odd-numbered) fenced code block by appending a
// closing delimiter, so a partial-stream buffer remains renderable. It is
// applied to the raw markdown text before glamour.Render, never to ANSI.
func sanitize(md string) string {
	matches := fenceRE.FindAllString(md, -1)
	if len(matches)%2 == 0 {
		return md
	}

	// Odd count: append a closing fence so the remainder is not rendered as
	// one giant code block.
	if !strings.HasSuffix(md, "\n") {
		return md + "\n```\n"
	}

	return md + "```\n"
}

// clampWidth returns w if positive, else defaultWidth.
func clampWidth(w int) int {
	if w <= 0 {
		return defaultWidth
	}

	return w
}

// StyleForBackground resolves Dark/Light from a background-colour darkness
// report (Bubble Tea's BackgroundColorMsg.IsDark in the TUI). Dark on a false
// /unknown result is the caller's responsibility; this helper only maps the
// boolean.
func StyleForBackground(isDark bool) Style {
	if isDark {
		return StyleDark
	}

	return StyleLight
}

// ResolveStyleOneShot resolves the glamour style for a one-shot stdout path
// (`--exec`, non-TTY interactive fallback). Notty when stdout is not a TTY;
// otherwise Dark/Light from the terminal environment (dark on detection
// failure). Raw is the caller's decision (`--raw`); this helper never returns
// it.
func ResolveStyleOneShot(stdout *os.File) Style {
	if stdout == nil || !term.IsTerminal(int(stdout.Fd())) {
		return StyleNotty
	}

	if termenv.HasDarkBackground() {
		return StyleDark
	}

	return StyleLight
}

// OneShotWidth returns the word-wrap width for a one-shot stdout path, read
// from the terminal size with an 80-column fallback.
func OneShotWidth(stdout *os.File) int {
	if stdout == nil {
		return defaultWidth
	}

	if w, _, err := term.GetSize(int(stdout.Fd())); err == nil && w > 0 {
		return w
	}

	return defaultWidth
}

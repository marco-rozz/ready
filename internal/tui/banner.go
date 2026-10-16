package tui

import (
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// LogoMode selects what leads the transcript viewport when the TUI opens:
// the inline PNG banner — when the resolved mode is png and the terminal is
// image-capable — is printed by cmd/chat.go on the main screen before the
// alt-screen program starts (so it reappears on exit with the restored main
// screen), and the TUI itself always leads with the glyph banner above the
// info block (banner-in-transcript, this change): the logo sits at the head
// of the scrollable text history in every capable/glyph run.
type LogoMode int

const (
	// LogoNone leads the viewport with the info block alone: off mode (no
	// logo anywhere). PNG mode ALSO resolves here for the frame — its banner
	// is printed on the main screen before the alt-screen program starts,
	// never inside the frame (inline images must not enter the frame, D1).
	LogoNone LogoMode = iota
	// LogoGlyph renders the READY glyph art inside the viewport, above the
	// info block. Glyph mode is the canonical in-TUI presentation; PNG mode
	// renders the same in-TUI banner so the logo is always at the head of the
	// scrollable history, while the PNG itself lives on the main screen.
	LogoGlyph
)

// BannerGlyphs is the READY banner's detailed glyph art: one string per text
// row per letter, each cell a half-height pixel (█ = both, ▀ = top, ▄ =
// bottom). It lives here (chat-logo-modes D4) so both the stdout renderer
// (cmd/ready.go, which keeps terminal detection + PNG emission) and the
// in-TUI renderer below consume the same shared data table without any
// internal → cmd import.
const BannerLetters = "READY."

// BannerGlyphs is the glyph data table shared with cmd/ready.go's stdout
// renderer; the in-TUI renderer (renderGlyphBanner) styles it with the TUI's
// own colour handling instead of writing per-row ANSI to stdout.
var BannerGlyphs = map[rune][]string{ //nolint:gochecknoglobals // pixel-art data table
	'R': {"██▀▀▀█▄", "██  ▄██", "██▀██▄ ", "▀▀  ▀▀▀"},
	'E': {"██▀▀▀▀▀", "██▄▄▄▄ ", "██     ", "▀▀▀▀▀▀▀"},
	'A': {" ▄█▀█▄ ", "██   ██", "██▀▀▀██", "▀▀   ▀▀"},
	'D': {"██▀▀█▄ ", "██   ██", "██  ▄█▀", "▀▀▀▀▀  "},
	'Y': {"██  ██", "▀█▄▄█▀", "  ██  ", "  ▀▀  "},
	'.': {"  ", "  ", "▄▄", "▀▀"},
}

// bannerSep is the horizontal gap between letters in every renderer.
const bannerSep = "  "

// bannerViolet is the violet of the '.' dot — RGB(130,132,210), the same
// colour the stdout renderers emit as a raw SGR sequence. In the viewport it
// is a lipgloss colour so the TUI's colour handling adapts it to the active
// terminal profile; raw escape sequences never enter the frame (D4).
var bannerViolet = lipgloss.Color("#8284D2") //nolint:gochecknoglobals // brand colour constant

// renderGlyphBanner renders the glyph banner as styled text for the viewport
// (glyph mode): the same rows the stdout renderer writes, but colour-handled
// by the TUI (no per-row stdout writes, no hard-coded FgViolet escape). The
// letters keep the terminal's default foreground; only the dot is tinted.
func renderGlyphBanner() string {
	dot := lipgloss.NewStyle().Foreground(bannerViolet)

	rows := make([]string, 0, 4)
	for row := range 4 {
		parts := make([]string, 0, len(BannerLetters))
		for _, ch := range BannerLetters {
			line := BannerGlyphs[ch][row]
			if ch == '.' {
				line = dot.Render(line)
			}
			parts = append(parts, line)
		}
		rows = append(rows, strings.Join(parts, bannerSep))
	}

	return strings.Join(rows, "\n")
}

// infoLabelStyle styles the info block's labels. Faint (like the status
// line) keeps the facts legible but subordinate to the conversation, and
// reads correctly on both dark and light backgrounds.
func infoLabelStyle() lipgloss.Style {
	return lipgloss.NewStyle().Faint(true)
}

// buildInfoBlock renders the static session-facts block shown as the first
// viewport content in png/off modes and directly below the glyph banner in
// glyph mode (chat-logo-modes D3): the daemon address, the session ID and
// the client version — all known at TUI construction time from the
// authenticated handshake. Plain styled text, NOT markdown-rendered; the
// caller stores it separately from the completed-turn render cache, so it
// is never persisted to history or checkpoints and never counted as a turn.
// The model reference is deliberately NOT here — it lives only in the
// status line (operator preference: one place, always visible, never
// duplicated in the banner). Missing facts render as "—", never as empty
// labels.
func buildInfoBlock(daemonAddr, sessionID, clientVersion string) string {
	line := func(label, value string) string {
		if value == "" {
			value = "—"
		}

		return infoLabelStyle().Render(label) + " " + value
	}

	rows := []string{
		line("daemon", daemonAddr),
		line("session", sessionID),
		line("client", clientVersion),
	}

	return strings.Join(rows, "\n")
}

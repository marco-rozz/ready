package main

import (
	_ "embed" // embedded logo asset
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/marco-rozz/ready/internal/tui"
)

const (
	Reset    = "\033[0m"
	FgViolet = "\033[38;2;130;132;210m"
)

// Image protocol identifiers (imageProtocol / printReadyImage).

// Image protocol identifiers (imageProtocol / printReadyImage).
const (
	protoKitty  = "kitty"
	protoIterm2 = "iterm2"
)

// image-capable terminal detection. Only env-based detection is used: it is
// synchronous and cannot stall a terminal that does not answer in-band
// queries. In-band capability probing (kitty CSI ? Gi, DA1-based device
// queries) is deliberately NOT used at banner time — printReady runs before
// any TUI setup and must never wait on stdin.
func imageProtocol() string {
	switch os.Getenv("TERM_PROGRAM") {
	case "WezTerm":
		// WezTerm speaks BOTH: prefer iTerm2 OSC 1337 (single escape, no chunking).
		return protoIterm2
	case "iTerm.app":
		return protoIterm2
	case "ghostty":
		return protoKitty
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return protoKitty
	}
	if os.Getenv("GHOSTTY_RESOURCES_DIR") != "" {
		return protoKitty
	}
	if os.Getenv("WEZTERM_PANE") != "" {
		return protoIterm2
	}

	return ""
}

// printReady prints the banner, layered like Claude Code's mascot:
//   - level 1 — real PNG inline image on image-capable terminals
//     (kitty/Ghostty/WezTerm/iTerm2), exact aspect, 2 colors;
//   - level 2 — detailed glyph art everywhere else (Terminal.app included:
//     the bg-fill experiment was reverted — on a real Terminal.app the fill
//     letterforms render distorted; the half-block glyphs are the design
//     the user sees and expects).
func printReady() {
	if proto := imageProtocol(); proto != "" {
		if printReadyImage(proto) {
			return
		}
		// emit failed → fall through to the glyph art
	}
	printReadyGlyphs()
}

func printReadyPNG() bool {
	proto := imageProtocol()
	if proto == "" {
		return false
	}

	return printReadyImage(proto)
}

// printReadyPNG emits ONLY the inline PNG (no glyph-on-stdout fallback):
// the chat TUI path calls it in every non-off logo mode and falls back to
// printReadyGlyphs() on failure (amendment banner-in-transcript: the
// banner is stamped on the main screen before the alt-screen session
// starts and reappears on exit). Returns false when the terminal is not
// image-capable or the emit failed.

// readyLogoPNG is the banner as a 2-color PNG (184×32, transparent
// background, white letters 235;235;235 + violet dot 130;132;210 — the same
// two colors as the text renderers). Emitted via the kitty graphics
// protocol or iTerm2 inline images on terminals that support them;
// embedded at build time, no runtime asset lookup.
//
//go:embed assets/ready_logo.png
var readyLogoPNG []byte

// printReadyImage emits the embedded PNG inline. Returns false when
// something goes wrong so the caller can fall back to text art.
func printReadyImage(proto string) bool {
	switch proto {
	case protoKitty:
		// Kitty graphics protocol: APC 'G' sequence. f=100 (PNG), a=T
		// (transmit+display), C=1 (keep cursor), q=1 (quiet — no response
		// frame), m=1/m=0 chunked base64 payload (4096-byte chunks per kitty
		// spec; the logo fits one chunk but chunking is implemented for
		// robustness). Only the first chunk carries the control key-values.
		const chunk = 4096
		b64 := base64.StdEncoding.EncodeToString(readyLogoPNG)
		for off, first := 0, true; off < len(b64) || first; {
			end := min(off+chunk, len(b64))
			last := end == len(b64)
			hdr := fmt.Sprintf("m=%d", 0)
			if !last {
				hdr = "m=1"
			}
			if first {
				hdr = "f=100,a=T,C=1,q=1," + hdr
			}
			if _, err := fmt.Fprintf(os.Stdout, "\x1b_G%s;%s\x1b\\", hdr, b64[off:end]); err != nil {
				return false
			}
			first, off = false, end
		}
		_, _ = fmt.Fprint(os.Stdout, "\n")
	case protoIterm2:
		// iTerm2 inline image escape: OSC 1337 ;File=[args]:base64, terminated
		// by BEL. iTerm2 also parses ST, BEL is the traditional terminator.
		if _, err := fmt.Fprintf(os.Stdout, "\x1b]1337;File=inline=1;size=%d;name=ready_logo.png:%s\a",
			len(readyLogoPNG), base64.StdEncoding.EncodeToString(readyLogoPNG)); err != nil {
			return false
		}
		_, _ = fmt.Fprint(os.Stdout, "\n")
	default:
		return false
	}

	return true
}

func printReadyGlyphs() {
	sep := "  "
	_, _ = fmt.Fprintln(os.Stdout)
	for row := range 4 {
		parts := make([]string, 0, len(tui.BannerLetters))
		for _, ch := range tui.BannerLetters {
			g := tui.BannerGlyphs[ch]
			line := g[row]
			if ch == '.' {
				line = FgViolet + line + Reset
			}
			parts = append(parts, line)
		}
		_, _ = fmt.Fprintln(os.Stdout, strings.Join(parts, sep))
	}
}

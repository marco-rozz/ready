package tui

// Tests for the static head block introduced by chat-tui-logo-modes: the
// info block (png/off modes) and the in-TUI glyph banner (glyph mode).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/privacy"
)

// logo test facts: all four info-block values known at construction, as they
// are in production after the authenticated handshake.
const (
	testModelRef    = "ollama-gemma::gemma4:31b"
	testDaemonAddr  = "tcp://127.0.0.1:7600"
	testClientVer   = "v1.2.3"
	testGlyphRowTop = "██▀▀▀█▄" // row 0 of the 'R' glyph — the banner's signature
)

func newLogoTestModel(t *testing.T, logo LogoMode) *Model {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID:  testSessionID,
		SessionID:     testSessionID, // the info block shows the conversation Session
		ReplyTimeout:  0,             // zero → NewModel clamps to 120s
		Sanitizer:     privacy.New(nil),
		ModelRef:      testModelRef,
		DaemonAddress: testDaemonAddr,
		ClientVersion: testClientVer,
		LogoMode:      logo,
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.BackgroundColorMsg{})

	return m
}

// assertInfoBlock verifies the three static facts are present verbatim in
// the content, and that the model reference is NOT in the banner (the
// status line is its only home).
func assertInfoBlock(t *testing.T, content string) {
	t.Helper()

	for _, want := range []string{
		testDaemonAddr,
		testSessionID,
		testClientVer,
		"daemon", "session", "client",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("info block missing %q in:\n%q", want, content)
		}
	}

	if strings.Contains(content, testModelRef) {
		t.Errorf("info block must not carry the model ref %q (status line only):\n%q", testModelRef, content)
	}
}

// TestInfoBlockPresentAtFirstFrameAllModes (task 4.3): in every logo mode the
// first rendered frame shows the info block. PNG/off modes lead with it
// alone; glyph mode shows the glyph banner above it.
func TestInfoBlockPresentAtFirstFrameAllModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []struct {
		name       string
		mode       LogoMode
		wantBanner bool
	}{
		{"png-logo", LogoNone, false},
		{"off-logo", LogoNone, false},
		{"glyph-logo", LogoGlyph, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			m := newLogoTestModel(t, mode.mode)
			content := m.buildContent()

			assertInfoBlock(t, content)

			hasBanner := strings.Contains(content, testGlyphRowTop)
			if hasBanner != mode.wantBanner {
				t.Errorf("glyph banner presence: got %v, want %v in content:\n%q", hasBanner, mode.wantBanner, content)
			}

			if mode.wantBanner {
				// glyph mode: banner strictly above the info block, info
				// block strictly above any (empty) conversation content.
				bannerIdx := strings.Index(content, testGlyphRowTop)
				daemonIdx := strings.Index(content, "daemon")
				if bannerIdx >= daemonIdx {
					t.Errorf("glyph banner must precede the info block; banner=%d info=%d", bannerIdx, daemonIdx)
				}
			} else if content != m.infoBlock {
				// png/off: the info block is the ONLY content — the whole
				// first-frame content is the static block itself.
				t.Errorf("png/off first frame is not the bare info block:\n%q", content)
			}

			// The first frame's viewport shows it too (content fits the
			// default viewport height).
			if !strings.Contains(m.View().Content, testClientVer) {
				t.Errorf("first frame does not show the info block:\n%q", m.View().Content)
			}
		})
	}
}

// TestGlyphBannerFirstFrameAboveConversation (task 3.3): with a completed
// turn in the transcript, the glyph banner and the info block sit above the
// conversation content, and the banner is rendered only through the model —
// never written to stdout outside the managed area (there is no stdout
// writer on Model at all; the cmd-side tests pin the no-decoration contract
// for the non-TUI paths).
func TestGlyphBannerFirstFrameAboveConversation(t *testing.T) {
	t.Parallel()

	m := newLogoTestModel(t, LogoGlyph)
	m.completeTurn("risposta123")

	content := m.buildContent()

	bannerIdx := strings.Index(content, testGlyphRowTop)
	infoIdx := strings.Index(content, testDaemonAddr)
	if bannerIdx == -1 || infoIdx == -1 {
		t.Fatalf("missing content: banner=%d info=%d\n%q", bannerIdx, infoIdx, content)
	}
	if bannerIdx >= infoIdx {
		t.Errorf("wrong ordering: want banner < info block; got %d < %d", bannerIdx, infoIdx)
	}

	// The banner stays viewport content (scrolls with the transcript); it is
	// NOT conversation history — the completed turns hold only the turn.
	if !strings.Contains(content, "risposta123") {
		t.Fatalf("reply not rendered in content:\n%q", content)
	}
	if len(m.completed) != 1 || m.completed[0].reply != "risposta123" {
		t.Errorf("completed turns polluted: %+v", m.completed)
	}
}

// TestInfoBlockIdenticalAcrossModes (task 4.3): same session facts, same
// info-block content in glyph and png modes — only the banner position
// differs.
func TestInfoBlockIdenticalAcrossModes(t *testing.T) {
	t.Parallel()

	glyph := newLogoTestModel(t, LogoGlyph)
	plain := newLogoTestModel(t, LogoNone)

	if glyph.infoBlock != plain.infoBlock {
		t.Errorf("info block differs across modes:\nglyph=%q\nplain=%q", glyph.infoBlock, plain.infoBlock)
	}
}

// TestInfoBlockUnchangedOnResizeReRender (task 4.3): the head block is a
// fixed static prefix — a resize re-render must not re-process or alter it.
func TestInfoBlockUnchangedOnResizeReRender(t *testing.T) {
	t.Parallel()

	m := newLogoTestModel(t, LogoNone)
	before := m.headBlock()

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m.Update(tea.BackgroundColorMsg{})

	if m.headBlock() != before {
		t.Errorf("info block changed on resize:\nbefore=%q\nafter=%q", before, m.headBlock())
	}

	// The completed-turn render cache never contains the info block: it is
	// not conversation history and would never be persisted to checkpoints.
	if strings.Contains(m.completedRenderd, testClientVer) {
		t.Errorf("info block leaked into the completed-turn render:\n%q", m.completedRenderd)
	}
}

// TestInfoBlockNotMarkdown (task 4.2): the info block is static styled text,
// not markdown — re-emitting it must reproduce the construction-time string
// byte-identically, without any width-dependent re-processing.
func TestInfoBlockNotMarkdown(t *testing.T) {
	t.Parallel()

	m := newLogoTestModel(t, LogoNone)

	want := buildInfoBlock(testDaemonAddr, testSessionID, testClientVer)
	if m.infoBlock != want {
		t.Errorf("info block not the static construction output:\nwant=%q\n got=%q", want, m.infoBlock)
	}
}

// TestBannerGlyphsSharedWithStdoutRenderer (task 3.1): the glyph data table
// moved to internal/tui and every letter has four rows of equal width so the
// stdout renderer in cmd/ready.go and the in-TUI renderer compose the same
// rows.
func TestBannerGlyphsSharedWithStdoutRenderer(t *testing.T) {
	t.Parallel()

	if len(BannerLetters) != 6 {
		t.Fatalf("BannerLetters: got %q, want READY.", BannerLetters)
	}

	for _, ch := range BannerLetters {
		g, ok := BannerGlyphs[ch]
		if !ok {
			t.Fatalf("BannerGlyphs missing letter %q", ch)
		}
		if len(g) != 4 {
			t.Errorf("letter %q has %d rows, want 4", ch, len(g))
		}
	}
}

// TestViewAltScreenPermanentMouseCapture (Amendment 3, mouse ownership): the
// frame runs in the alternate screen for the whole session and the mouse is
// captured in EVERY turn state. The client never hands the mouse back to the
// terminal: the wheel always scrolls the internal viewport, and selection
// and copy are TUI-managed (selection.go) — so neither the Terminal.app
// quirk (no selection override while reporting is active) nor the dead
// at-rest wheel of the conditional-capture amendment can ever reappear.
func TestViewAltScreenPermanentMouseCapture(t *testing.T) {
	t.Parallel()

	m := newLogoTestModel(t, LogoGlyph)

	v := m.View()
	if !v.AltScreen {
		t.Error("View is not alt-screen; the frame must own the terminal for the whole session")
	}
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("mouse mode at rest: got %v, want MouseModeCellMotion (permanent capture)", v.MouseMode)
	}

	// The capture is unconditional: working states report the same mode.
	m.state = stateThinking
	if v := m.View(); v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("mouse mode while working: got %v, want MouseModeCellMotion", v.MouseMode)
	}
}

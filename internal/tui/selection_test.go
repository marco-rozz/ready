package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/marco-rozz/ready/internal/privacy"
)

// selectionTestModel builds a Model sized 80x24 with a three-line content
// already laid out in the viewport (plain text, no styling — widths are
// exact display cells).
func selectionTestModel(t *testing.T, content string) *Model {
	t.Helper()

	m := NewModel(&fakeDealer{}, Options{Sanitizer: privacy.New(nil)})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.content = content
	m.vp.SetContent(content)
	m.vp.GotoBottom()

	return m
}

// TestSelectionDragHighlightsAndReleases drives the mouse path through the
// real Update handler: click → motion → the viewport shows the reverse-video
// span; release → the selection clears, the plain content is restored and
// the status line confirms the copy.
func TestSelectionDragHighlightsAndReleases(t *testing.T) {
	t.Parallel()

	m := selectionTestModel(t, "abc\nhello world\nxyz")

	// Click at line 1, cell 0 (viewport row 1 = content row 1, not scrolled).
	m.Update(tea.MouseClickMsg(tea.Mouse{X: 0, Y: 1, Button: 0}))
	if !m.selActive {
		t.Fatal("click did not start a selection")
	}
	if m.selAnchor != (selCell{row: 1, col: 0}) {
		t.Fatalf("anchor = %+v, want {1 0}", m.selAnchor)
	}

	// Drag to cell 5 of the same line: the viewport content must carry the
	// reverse-video highlight.
	m.Update(tea.MouseMotionMsg(tea.Mouse{X: 5, Y: 1, Button: 0}))
	rendered := m.vp.View()
	if !strings.Contains(rendered, "\x1b[7m") {
		t.Error("drag did not render the reverse-video highlight")
	}

	// Release: selection cleared, plain content restored, copy confirmed.
	tm, _ := m.Update(tea.MouseReleaseMsg(tea.Mouse{X: 5, Y: 1, Button: 0}))
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	m = mm
	if m.selActive {
		t.Error("selection still active after release")
	}
	if m.copiedMsg == "" {
		t.Error("release did not set the copy confirmation")
	}
	if got := m.vp.View(); strings.Contains(got, "\x1b[7m") {
		t.Error("plain content not restored after release")
	}
}

// TestSelectionCoordinatesAreViewportRelative asserts the row mapping
// content row = vp.YOffset + mouse Y: with the viewport scrolled two lines
// down, a click on the first visible row selects the content row two lines
// into the content.
func TestSelectionCoordinatesAreViewportRelative(t *testing.T) {
	t.Parallel()

	// 40 lines — taller than the 20-row viewport, so YOffset can move.
	lines := make([]string, 0, 40)
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("riga %02d", i))
	}
	m := selectionTestModel(t, strings.Join(lines, "\n"))
	m.vp.SetYOffset(2)

	m.Update(tea.MouseClickMsg(tea.Mouse{X: 1, Y: 0, Button: 0}))
	if m.selAnchor.row != 2 {
		t.Fatalf("anchor row = %d, want 2 (YOffset 2 + screen row 0)", m.selAnchor.row)
	}
}

// TestExtractSelectionCoversEdges exercises the what-you-see extraction on
// multi-row spans: edge rows clipped to their selection columns, inner rows
// whole, trailing padding stripped, ANSI removed.
func TestExtractSelectionCoversEdges(t *testing.T) {
	t.Parallel()

	// Styled line: the ANSI must not leak into the copied text.
	styled := "\x1b[1mhello world\x1b[0m"
	m := selectionTestModel(t, "abc\n"+styled+"\nxyz\nplain tail")

	tests := []struct {
		name    string
		anchor  selCell
		head    selCell
		want    string
		wantRev bool // reverse-ordered input must normalise
	}{
		{"single row partial", selCell{1, 0}, selCell{1, 5}, "hello", false},
		{"single row mid", selCell{1, 6}, selCell{1, 11}, "world", false},
		{"multi row span", selCell{1, 6}, selCell{3, 5}, "world\nxyz\nplain", false},
		{"full row", selCell{0, 0}, selCell{0, 3}, "abc", false},
		{"reversed order", selCell{3, 5}, selCell{1, 6}, "world\nxyz\nplain", true},
		{"selection past line end pads to extent", selCell{2, 0}, selCell{2, 10}, "xyz", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.selActive = true
			m.selAnchor = tc.anchor
			m.selHead = tc.head

			if got := m.extractSelection(); got != tc.want {
				t.Errorf("extractSelection() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSelectionCancelledOnContentChange: a click-drag in flight is cancelled
// by any transcript content change (progress chunk → tick rebuild), while
// the wheel keeps scrolling.
func TestSelectionCancelledOnContentChange(t *testing.T) {
	t.Parallel()

	m := selectionTestModel(t, "abc\nhello world\nxyz")
	m.Update(tea.MouseClickMsg(tea.Mouse{X: 0, Y: 1, Button: 0}))
	m.Update(tea.MouseMotionMsg(tea.Mouse{X: 5, Y: 1, Button: 0}))
	if !m.selActive {
		t.Fatal("precondition: selection active")
	}

	// Content change: the dirty-buffer path a progress chunk exercises.
	m.buffer.WriteString("nuovo contenuto")
	m.dirty = true
	m.Update(tickMsg{})
	if m.selActive {
		t.Error("selection survived a transcript content change")
	}

	// The wheel still scrolls after the cancellation: taller content than
	// the viewport, so the offset can actually move.
	lines := make([]string, 0, 40)
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("riga %02d", i))
	}
	m.content = strings.Join(lines, "\n")
	m.vp.SetContent(m.content)
	m.vp.GotoBottom()
	before := m.vp.YOffset()
	m.Update(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelUp}))
	if m.vp.YOffset() == before {
		t.Error("wheel did not scroll after selection cancellation")
	}
}

// TestSelectionClickWithoutDragIsInert: press+release without motion copies
// nothing and shows no confirmation.
func TestSelectionClickWithoutDragIsInert(t *testing.T) {
	t.Parallel()

	m := selectionTestModel(t, "abc\nhello world\nxyz")
	_, _ = m.Update(tea.MouseClickMsg(tea.Mouse{X: 0, Y: 1, Button: 0}))
	tm, _ := m.Update(tea.MouseReleaseMsg(tea.Mouse{X: 0, Y: 1, Button: 0}))
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	m = mm

	if m.copiedMsg != "" {
		t.Errorf("click without drag set a copy confirmation: %q", m.copiedMsg)
	}
}

// TestSelectionMouseOutsideTranscriptIsInert: clicks in the input-editor
// region never start a selection.
func TestSelectionMouseOutsideTranscriptIsInert(t *testing.T) {
	t.Parallel()

	m := selectionTestModel(t, "abc\nhello world\nxyz")
	m.Update(tea.MouseClickMsg(tea.Mouse{X: 5, Y: 22, Button: 0})) // editor region
	if m.selActive {
		t.Error("click below the transcript started a selection")
	}
}

// TestApplySelectionPadsPastLineEnd asserts the highlight covers the
// trailing empty cells when the selection extends past the text, and that
// the row keeps its full display width.
func TestApplySelectionPadsPastLineEnd(t *testing.T) {
	t.Parallel()

	m := selectionTestModel(t, "abc\nhello\nxyz")
	m.selActive = true
	m.selAnchor = selCell{row: 1, col: 0}
	m.selHead = selCell{row: 1, col: 10} // past "hello" (5 cells)

	out := m.applySelection(m.content)
	lines := strings.Split(out, "\n")
	if w := ansi.StringWidth(ansi.Strip(lines[1])); w != 5 {
		t.Errorf("selected row width = %d, want 5 (no runaway padding)", w)
	}
	if !strings.Contains(lines[1], "\x1b[7m") {
		t.Errorf("row not highlighted: %q", lines[1])
	}
	if strings.Contains(lines[1], "abc") || strings.Contains(lines[1], "xyz") {
		t.Errorf("neighbouring rows must be untouched, got %q", lines[1])
	}
}

// TestCellSliceNeverSplitsWideRunes: slicing around a double-width rune
// never breaks the glyph — the overlap rule includes any rune whose cells
// touch the span (a wide rune is 2 cells wide, so both edges over-include
// it rather than cutting it in half).
func TestCellSliceNeverSplitsWideRunes(t *testing.T) {
	t.Parallel()

	// "世" is double-width: cells 0-1, then "a" on cell 2.
	const s = "世a"

	if got := cellSlice(s, 0, 1); got != "世" {
		t.Errorf("cellSlice(0,1) = %q, want %q (overlap rule keeps the glyph whole)", got, "世")
	}
	if got := cellSlice(s, 0, 2); got != "世" {
		t.Errorf("cellSlice(0,2) = %q, want the full wide rune", got)
	}
	if got := cellSlice(s, 1, 3); got != "世a" {
		t.Errorf("cellSlice(1,3) = %q, want %q (the wide rune overlaps cell 1)", got, "世a")
	}
	if got := cellSlice(s, 2, 3); got != "a" {
		t.Errorf("cellSlice(2,3) = %q, want %q", got, "a")
	}
	if got := ansi.StringWidth(s); got != 3 {
		t.Errorf("precondition: display width of %q = %d, want 3", s, got)
	}
}

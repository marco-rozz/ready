package tui

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// selCell identifies a cell of the viewport content: row is the index into
// the content's lines (split on "\n"), col is the display-cell index of the
// ANSI-stripped line (wide runes count as their full width).
type selCell struct {
	row int
	col int
}

// selection state lives on the Model (selActive/selAnchor/selHead):
//
//   - selAnchor is where the drag started, selHead where it currently is —
//     the order along each axis is resolved at render/extraction time.
//   - Coordinates come straight from the SGR mouse protocol: row =
//     vp.YOffset() + mouse Y (the transcript starts at frame row 0 — the
//     viewport owns the top region of the frame), col = mouse X clamped to
//     the line's display width.
//
// Lifecycle (Amendment 3, mouse ownership): a click starts the selection,
// motion extends it, release extracts and copies the rendered text
// (what-you-see, ANSI stripped). Any transcript content change (progress
// chunk, final reply, resize re-render) cancels the selection — no mobile
// anchors, no drift bugs; scrolling stays available throughout.

// copyConfirmDuration is how long the status line keeps the copy
// confirmation before clearing it.
const copyConfirmDuration = 2 * time.Second

// pbcopyTimeout bounds the background pbcopy round-trip: the text is already
// delivered via OSC52 on capable terminals, so a stuck pbcopy must never
// outlive this window.
const pbcopyTimeout = 2 * time.Second

// handleMouseEvent routes the mouse-ownership messages out of Update
// (Amendment 3): wheel scroll, in-app selection start/extend/release, and
// the copy-confirmation expiry. The bool reports whether msg was consumed.
func (m *Model) handleMouseEvent(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		var vcmd tea.Cmd
		m.vp, vcmd = m.vp.Update(msg)
		_ = vcmd
		m.syncAutoFollow()

		return m, nil, true

	case tea.MouseClickMsg:
		m.startSelection(msg.Mouse())

		return m, nil, true

	case tea.MouseMotionMsg:
		btn := msg.Mouse()
		if btn.Button == 0 && !m.selActive {
			return m, nil, true
		}
		m.extendSelection(btn)
		m.refreshSelection()

		return m, nil, true

	case tea.MouseReleaseMsg:
		model, cmd := m.finishSelection()

		return model, cmd, true

	case copyStatusExpireMsg:
		m.copiedMsg = ""

		return m, nil, true
	}

	return m, nil, false
}

// selHighlightStyle is built per call (no globals): the reverse-video style
// applied to the selected span while the drag continues.
func selHighlightStyle() lipgloss.Style {
	return lipgloss.NewStyle().Reverse(true)
}

// startSelection anchors a new selection at the clicked cell. Rows outside
// the current content are clamped; clicks outside the transcript region
// (editor, bars, status line) never start one.
func (m *Model) startSelection(btn tea.Mouse) {
	if btn.Y >= m.vp.Height() {
		return
	}

	lines := strings.Split(m.content, "\n")
	row := m.vp.YOffset() + btn.Y
	if row < 0 {
		row = 0
	}
	if row >= len(lines) {
		row = len(lines) - 1
	}
	if row < 0 {
		return
	}

	col := btn.X
	if w := ansi.StringWidth(ansi.Strip(lines[row])); col > w {
		col = w
	}

	m.selActive = true
	m.selAnchor = selCell{row: row, col: col}
	m.selHead = m.selAnchor
}

// extendSelection moves the selection head to the dragged cell.
func (m *Model) extendSelection(btn tea.Mouse) {
	if !m.selActive || btn.Y >= m.vp.Height() {
		return
	}

	lines := strings.Split(m.content, "\n")
	row := m.vp.YOffset() + btn.Y
	if row < 0 {
		row = 0
	}
	if row >= len(lines) {
		row = len(lines) - 1
	}

	col := btn.X
	if w := ansi.StringWidth(ansi.Strip(lines[row])); col > w {
		col = w
	}

	m.selHead = selCell{row: row, col: col}
}

// cancelSelection drops any in-progress selection (called whenever the
// transcript content changes under it).
func (m *Model) cancelSelection() {
	m.selActive = false
	m.selAnchor = selCell{}
	m.selHead = selCell{}
}

// applySelection re-renders the viewport content with the selected rows
// highlighted: each affected row is drawn as its plain (ANSI-stripped) text
// with the selected span in reverse video, padded to the selection extent.
// Rows outside the selection keep their original styling. The styled content
// itself is never mutated — the highlight is a pure overlay.
func (m *Model) applySelection(content string) string {
	lines := strings.Split(content, "\n")
	lo, hi := m.selAnchor.row, m.selHead.row
	loCol, hiCol := m.selAnchor.col, m.selHead.col
	if lo > hi || (lo == hi && loCol > hiCol) {
		lo, hi = hi, lo
		loCol, hiCol = hiCol, loCol
	}

	if hi >= len(lines) {
		hi = len(lines) - 1
		if hi < lo {
			return content
		}
	}

	for r := lo; r <= hi; r++ {
		plain := ansi.Strip(lines[r])
		from := 0
		if r == lo {
			from = min(loCol, ansi.StringWidth(plain))
		}
		to := ansi.StringWidth(plain)
		if r == hi {
			to = min(hiCol, ansi.StringWidth(plain))
		}
		if from > to {
			from = to
		}

		lines[r] = renderSelLine(plain, from, to)
	}

	return strings.Join(lines, "\n")
}

// renderSelLine re-renders one content row with [from,to) highlighted:
// plain text with the selected span in reverse video, the span padded with
// spaces when the selection extends past the line's text (covering the
// trailing empty cells up to the release point).
func renderSelLine(plain string, from, to int) string {
	w := ansi.StringWidth(plain)
	var b strings.Builder
	b.WriteString(cellSlice(plain, 0, from))
	sel := cellSlice(plain, from, to)
	if pad := to - from - ansi.StringWidth(sel); pad > 0 {
		sel += strings.Repeat(" ", pad)
	}
	b.WriteString(selHighlightStyle().Render(sel))
	b.WriteString(cellSlice(plain, to, w))

	return b.String()
}

// extractSelection returns the selected text as rendered (ANSI stripped):
// edge rows are clipped to their selection columns, inner rows are taken
// whole. An empty selection (click without drag) yields "".
func (m *Model) extractSelection() string {
	if !m.selActive {
		return ""
	}

	lines := strings.Split(m.content, "\n")
	lo, hi := m.selAnchor.row, m.selHead.row
	loCol, hiCol := m.selAnchor.col, m.selHead.col
	if lo > hi || (lo == hi && loCol > hiCol) {
		lo, hi = hi, lo
		loCol, hiCol = hiCol, loCol
	}
	if lo < 0 || hi >= len(lines) || lo > hi {
		return ""
	}

	out := make([]string, 0, hi-lo+1)
	for r := lo; r <= hi; r++ {
		plain := ansi.Strip(lines[r])
		w := ansi.StringWidth(plain)
		from, to := 0, w
		if r == lo {
			from = min(loCol, w)
		}
		if r == hi {
			to = min(hiCol, w)
		}
		if from > to {
			from = to
		}
		out = append(out, strings.TrimRight(cellSlice(plain, from, to), " "))
	}

	return strings.Join(out, "\n")
}

// cellSlice returns the substring of a plain (ANSI-free) string covering the
// display cells [from,to): wide runes (cells 2..n) are included whole when
// they overlap the span, so slicing never splits a glyph in half.
func cellSlice(plain string, from, to int) string {
	if from >= to {
		return ""
	}

	var b strings.Builder
	cell := 0
	for _, r := range plain {
		rw := ansi.StringWidth(string(r))
		end := cell + rw
		if end > from && cell < to {
			b.WriteRune(r)
		}
		cell = end
		if cell >= to {
			break
		}
	}

	return b.String()
}

// copyToClipboard delivers the selected text to the system clipboard: OSC52
// (tea.SetClipboard — Ghostty/kitty/iTerm2/WezTerm) plus, on darwin, a
// background `pbcopy` with its own stdin pipe. pbcopy is deliberately NOT
// run through tea.ExecProcess: ExecProcess suspends the program (leaving the
// alt screen) and would flicker the whole frame on every copy.
func (m *Model) copyToClipboard(text string) tea.Cmd {
	pbcopy := m.pbcopyCmd(text)

	return tea.Batch(tea.SetClipboard(text), pbcopy)
}

// pbcopyCmd pipes the text to `pbcopy` in a background process, reaping it
// before returning. nil on non-darwin platforms or when pbcopy is missing —
// OSC52 already covered the capable terminals.
func (m *Model) pbcopyCmd(text string) tea.Cmd {
	if runtime.GOOS != "darwin" {
		return nil
	}

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pbcopyTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, "pbcopy")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil
		}
		if err := cmd.Start(); err != nil {
			return nil
		}
		_, _ = stdin.Write([]byte(text))
		_ = stdin.Close()
		_ = cmd.Wait()

		return copiedMsg{}
	}
}

// clearCopyStatusCmd expires the status line's copy confirmation.
func clearCopyStatusCmd() tea.Cmd {
	return tea.Tick(copyConfirmDuration, func(time.Time) tea.Msg {
		return copyStatusExpireMsg{}
	})
}

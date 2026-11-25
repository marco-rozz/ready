package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"

	"github.com/marco-rozz/ready/internal/mdrender"
	"github.com/marco-rozz/ready/internal/protocol"
)

// Mid-turn Notices (protocol 8, say-mid-turn-notice). A Notice is text the
// agent chose to show mid-Turn through the `say` tool: an act performed, so it
// renders as a labelled markdown block at its chronological position in the
// Turn's body — never as a preview, never as turn state.

// glyphNotice labels a Notice block: single-cell and not East-Asian-ambiguous
// (checked by TestLabelGlyphsAreSingleCellEvenWithEastAsianWidth), distinct
// from the steering glyphs.
const glyphNotice = "✦"

// labelNotice is the English label line text of a Notice block.
const labelNotice = glyphNotice + " notice"

// noticeIndent is the left indent of a Notice's markdown body: together with
// the label it sets the block apart from user echoes (band) and the answer.
const noticeIndent = 2

// bodyKind classifies one entry of a Turn's body.
type bodyKind int

const (
	// bodySteer is a user message injected into the Turn (steer_injected or
	// pre-loaded by turn_started).
	bodySteer bodyKind = iota
	// bodyNotice is a Mid-turn Notice (markdown text).
	bodyNotice
	// bodyInfo is a one-line informational entry (a Subagent handback trace).
	bodyInfo
)

// bodyItem is one entry of a Turn's body, kept in the order the matching
// daemon events arrived ("Chronological turn body").
type bodyItem struct {
	kind  bodyKind
	block echoBlock // bodySteer
	text  string    // bodyNotice (markdown) / bodyInfo (plain)
}

// ensureNoticeRenderer lazily builds the Notice body renderer at the viewport
// width minus the indent.
func (m *Model) ensureNoticeRenderer() *mdrender.Renderer {
	if m.noticeRenderer == nil {
		w := m.width - noticeIndent
		if w < 1 {
			w = 1
		}

		m.noticeRenderer = mdrender.New(w, m.style, m.sanitizer)
	}

	return m.noticeRenderer
}

// renderNoticeBlock renders one Notice: the dim label line (terminal styling,
// not markdown) above the indented, redacted markdown text.
func (m *Model) renderNoticeBlock(text string) string {
	body := strings.Trim(m.ensureNoticeRenderer().Render(text), "\n")
	body = lipgloss.NewStyle().PaddingLeft(noticeIndent).Render(body)

	return infoLabelStyle().Render(labelNotice) + "\n" + body
}

// renderBody renders a Turn's body items in order. Consecutive user echoes
// stay one line apart (as before); every other boundary has a blank row.
func (m *Model) renderBody(items []bodyItem) []string {
	out := make([]string, 0, len(items))

	for _, it := range items {
		switch it.kind {
		case bodySteer:
			out = append(out, m.renderEchoBlock(it.block))
		case bodyNotice:
			out = append(out, m.renderNoticeBlock(it.text))
		case bodyInfo:
			out = append(out, infoLabelStyle().Render(it.text))
		}
	}

	return out
}

// handleNotice applies a `notice` control event. A Notice of the live Turn
// joins its body and re-arms the reply timeout; any other (no live Turn, or
// another turn_id — for example after a reconnect during a running Turn)
// renders as a standalone block at the transcript tail. It never changes the
// turn state, the spinner or the usage segment.
func (m *Model) handleNotice(resp protocol.Response) (tea.Model, tea.Cmd) {
	if resp.Notice == nil || resp.Notice.Text == "" {
		return m, m.recvCmd()
	}

	text := resp.Notice.Text

	live := m.waiting && m.turnCounted && m.turnID != "" && resp.Turn != nil && resp.Turn.TurnID == m.turnID
	if live {
		m.body = append(m.body, bodyItem{kind: bodyNotice, text: text})
		m.timeoutSeq++
		m.rebuildViewport()

		return m, tea.Batch(m.recvCmd(), m.timeoutCmd())
	}

	if m.waiting {
		// Another turn is live: keep the Notice at the tail until it ends.
		m.standalone = append(m.standalone, text)
	} else {
		m.completed = append(m.completed, completedTurn{notice: text})
		m.completedDirty = true
	}

	m.rebuildViewport()

	return m, m.recvCmd()
}

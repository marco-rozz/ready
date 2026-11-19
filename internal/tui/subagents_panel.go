// subagents_panel.go — the TUI Subagent panel (openspec change
// subagent-runtime, task 6.4, design D11; amended by
// subagent-panel-observability D1/D3/D4/D6): a bounded, scrollable, pure
// view of the latest `subagents` snapshot — a header (running/idle counts,
// the ◆ pending-results badge, scroll indicators) and at most 2 fleet rows,
// anchored at the TAIL (the most recent agents). Only ACTIVE Subagents
// keep the box visible and take its space from the transcript viewport's
// share (never the input's). The wheel over the panel scrolls it (the
// transcript keeps the wheel elsewhere); Alt+↑/Alt+↓ scroll from the
// keyboard. The panel is a view only: it never touches the turn state or
// the transcript, and it is cleared on `session_changed`.
package tui

import (
	"fmt"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/marco-rozz/ready/internal/protocol"
)

// subagentsPanelMaxRows bounds the fleet rows shown at once
// (subagent-panel-observability D4: at most two — the two most recent).
const subagentsPanelMaxRows = 2

// Subagent statuses that keep the panel visible (the fleet's ACTIVE side).
const (
	subagentStatusRunning = "running"
	subagentStatusIdle    = "idle"
)

// subagentsPanelHeight returns the panel's rendered line count (0 when the
// panel is hidden). Only ACTIVE Subagents keep the panel visible
// (subagent-panel-observability D3): a one-shot that finished
// (done/failed/interrupted) is no longer active and drops out of the view,
// and a non-zero pending_handbacks alone NEVER keeps the box alive — the
// pending signal chain is the `subagent_handback` transcript line plus the
// automatic delivery at the head of the next Turn. A standing Subagent
// idles (wake-on-mail) and stays visible.
func (m *Model) subagentsPanelHeight() int {
	if m.subagents == nil || len(m.subagentsActiveAgents()) == 0 {
		return 0
	}

	return m.subagentsVisibleRows() + 2 // + top border (title + counts) + bottom border
}

// subagentsActiveAgents returns the fleet's active entries: running and
// idle (standing, wake-on-mail) Subagents only.
func (m *Model) subagentsActiveAgents() []protocol.SubagentsAgentView {
	if m.subagents == nil {
		return nil
	}

	active := make([]protocol.SubagentsAgentView, 0, len(m.subagents.Agents))
	for _, a := range m.subagents.Agents {
		if a.Status == subagentStatusRunning || a.Status == subagentStatusIdle {
			active = append(active, a)
		}
	}

	return active
}

// subagentsVisibleRows returns the rows the panel shows at the current
// scroll offset (at most subagentsPanelMaxRows), over the ACTIVE agents.
// The offset is clamped against the active count; the TAIL anchor is kept
// by subagentsApplySnapshot, not here.
func (m *Model) subagentsVisibleRows() int {
	active := m.subagentsActiveAgents()
	n := len(active)
	if n == 0 {
		return 0
	}

	rows := min(subagentsPanelMaxRows, n)
	m.subagentsScroll = min(max(0, m.subagentsScroll), max(0, n-rows))

	return rows
}

// subagentsApplySnapshot installs a new fleet snapshot (D4): a
// tail-anchored window stays tail-anchored (re-derived against the new
// active list); a user scrolled into history keeps their offset, clamped.
// Called from the Update path where the snapshot is applied — never inside
// the height arithmetic (R1).
func (m *Model) subagentsApplySnapshot(info *protocol.SubagentsInfo) {
	atTail := m.subagentsAtTail
	m.subagents = info
	if info == nil {
		m.subagentsScroll = 0
		m.subagentsAtTail = true

		return
	}

	n := len(m.subagentsActiveAgents())
	maxScroll := max(0, n-subagentsPanelMaxRows)
	if atTail || m.subagentsScroll > maxScroll {
		m.subagentsScroll = maxScroll
		m.subagentsAtTail = true
	}
	if m.subagentsScroll < 0 {
		m.subagentsScroll = 0
	}
}

// subagentsPanel renders the panel as a rounded-corner box in the ported
// Pi Agent panel style: the title `Subagents` is embedded in the top border
// (left) and the active counts ride the same border (right); the ACTIVE
// agent rows sit inside, padded one cell from the borders. Pure: no state
// change beyond the scroll clamping.
func (m *Model) subagentsPanel() string {
	if m.subagentsPanelHeight() == 0 {
		return ""
	}

	info := m.subagents
	active := m.subagentsActiveAgents()
	running, idle := 0, 0
	for _, a := range active {
		switch a.Status {
		case subagentStatusRunning:
			running++
		case subagentStatusIdle:
			idle++
		}
	}

	title := " Subagents "
	counts := fmt.Sprintf("%d running", running)
	if idle > 0 {
		counts += fmt.Sprintf(" · %d idle", idle)
	}
	if info.PendingHandbacks > 0 {
		counts += fmt.Sprintf(" · ◆ %d pending", info.PendingHandbacks)
	}

	rows := m.subagentsVisibleRows()
	n := len(active)
	indicator := ""
	if n > subagentsPanelMaxRows {
		switch {
		case m.subagentsScroll > 0 && m.subagentsScroll+rows < n:
			indicator = "↑↓"
		case m.subagentsScroll > 0:
			indicator = "↑"
		default:
			indicator = "↓"
		}
	}

	// D6: pad the counts group with one space on each side BEFORE the width
	// arithmetic; the scroll indicator (if any) rides between the spaces.
	countsIndicator := " " + counts
	if indicator != "" {
		countsIndicator += " " + indicator + " "
	} else {
		countsIndicator += " "
	}

	// Row content: start time, name (type), status, activity, queued mail.
	lines := make([]string, 0, rows)
	contentWidth := 0
	for _, a := range active[m.subagentsScroll : m.subagentsScroll+rows] {
		line := subagentsRowStyle().Render(subagentsRowTime(a.StartedAt)) +
			"  " +
			fmt.Sprintf("%s ", a.Name) +
			subagentsRowStyle().Render(fmt.Sprintf("(%s)", a.Type)) +
			"  " + a.Status
		if a.Activity != "" {
			line += " " + subagentsRowStyle().Render("· "+a.Activity)
		}
		if a.QueuedMail > 0 {
			line += " " + subagentsRowStyle().Render(fmt.Sprintf("· %d queued", a.QueuedMail))
		}
		lines = append(lines, line)

		if w := lipgloss.Width(line); w > contentWidth {
			contentWidth = w
		}
	}

	// Inner width fits the widest row plus one cell of padding on each side
	// (the rows never touch the borders), and the border labels (plus room
	// for the border dashes around them — two after the counts, D6); capped
	// by the terminal width (the box hugs its content, it is not full-width).
	inner := max(contentWidth+2, lipgloss.Width(title)+lipgloss.Width(countsIndicator)+6)
	inner = min(inner, max(0, m.width-2))

	boxWidth := inner + 2 // the two border columns

	border := subagentsBorderStyle()
	var b strings.Builder

	// Top border: ╭─ Subagents ───── counts ──╮ (D6: one space on each side
	// of the counts group — the digits never touch the border dashes; the
	// padded labels are what the width arithmetic sees, so the dashes
	// shrink accordingly. At least two dashes close the run before ╮).
	if dashes := boxWidth - 5 - lipgloss.Width(title) - lipgloss.Width(countsIndicator); dashes >= 1 {
		b.WriteString(border.Render("╭─") + border.Bold(true).Render(title) +
			border.Render(strings.Repeat("─", dashes)) +
			border.Bold(true).Render(countsIndicator) +
			border.Render("──╮"))
	} else {
		// Too narrow for the labels: plain top border.
		b.WriteString(border.Render("╭" + strings.Repeat("─", boxWidth-2) + "╮"))
	}

	for _, line := range lines {
		pad := inner - lipgloss.Width(line) - 1 // −1: the fixed left pad
		b.WriteString("\n" + border.Render("│") + " " + line + strings.Repeat(" ", max(0, pad)) + border.Render("│"))
	}

	b.WriteString("\n" + border.Render("╰"+strings.Repeat("─", boxWidth-2)+"╯"))

	return b.String()
}

// handbackDuration renders a `subagent_handback` notice's elapsed_ms
// (protocol 6, subagent-panel-observability D5) as a compact human duration
// ("4m12s"; "12s" under a minute). 0 — the field absent, e.g. an old
// daemon — renders nothing.
func handbackDuration(elapsedMS int64) string {
	if elapsedMS <= 0 {
		return ""
	}

	d := (time.Duration(elapsedMS) * time.Millisecond).Truncate(time.Second)
	if d <= 0 {
		return "0s"
	}

	return d.String()
}

// subagentsRowTime renders a snapshot's RFC3339 start time as a local HH:MM
// (a dashed "--:--" when the timestamp cannot be parsed).
func subagentsRowTime(startedAt string) string {
	if t, err := time.Parse(time.RFC3339, startedAt); err == nil {
		return t.Local().Format("15:04")
	}

	return "--:--"
}

// subagentsPanelScroll moves the panel's scroll offset by dir rows
// (clamped). The tail-anchor flag follows the offset: scrolled up into
// history clears it, returning to the bottom re-arms it so the next
// snapshot re-anchors at the tail (D4).
func (m *Model) subagentsPanelScroll(dir int) {
	if m.subagents == nil {
		return
	}

	n := len(m.subagentsActiveAgents())
	maxScroll := max(0, n-subagentsPanelMaxRows)

	m.subagentsScroll = min(max(0, m.subagentsScroll+dir), maxScroll)
	m.subagentsAtTail = m.subagentsScroll == maxScroll
}

// subagentsBorderStyle / row styles (built per call, no globals): the box
// uses a fixed soft teal — the operator's pick — instead of the theme's
// dark ANSI blue.
func subagentsBorderStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#4EC9B0"))
}

func subagentsRowStyle() lipgloss.Style {
	return lipgloss.NewStyle().Faint(true)
}

// wheelInSubagentsPanel reports whether a wheel event's Y lands inside the
// panel's screen rows (the panel renders directly below the transcript
// viewport).
func (m *Model) wheelInSubagentsPanel(y int) bool {
	h := m.subagentsPanelHeight()
	if h == 0 {
		return false
	}

	return y >= m.vp.Height() && y < m.vp.Height()+h
}

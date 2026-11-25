package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/marco-rozz/ready/internal/protocol"
)

const (
	pTypeExplore = "explore"
	pTypeGP      = "general-purpose"
)

// Test literals (goconst).
const (
	tNameDailyBrief    = "daily-brief"
	tStatusCompleted   = "completed"
	tID1               = "id-1"
	tStatusRunning     = "running"
	tStatusDone        = "done"
	tStatusFailed      = "failed"
	tStatusInterrupted = "interrupted"
)

func subagentsFixture() *protocol.SubagentsInfo {
	return &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: "explore-1", Type: pTypeExplore, Status: tStatusRunning, QueuedMail: 2},
			{SubagentID: "id-2", Name: "runner-2", Type: pTypeGP, Status: tStatusRunning},
			{SubagentID: "id-3", Name: "runner-3", Type: pTypeExplore, Status: tStatusRunning},
			{SubagentID: "id-4", Name: "watch-1", Type: pTypeGP, Status: "idle"},
			{SubagentID: "id-5", Name: "audit-1", Type: pTypeGP, Status: tStatusDone},
			{SubagentID: "id-6", Name: "explore-2", Type: pTypeExplore, Status: tStatusFailed},
			{SubagentID: "id-7", Name: "explore-3", Type: pTypeExplore, Status: tStatusInterrupted},
		},
		PendingHandbacks: 2,
	}
}

func newPanelModel(t *testing.T) *Model {
	t.Helper()

	m := NewModel(nil, Options{})
	m.width, m.height = 80, 30
	m.applyLayout()
	// Install the snapshot through the real Update-path apply (D4 tail
	// anchoring lives there, not in direct field assignment).
	m.subagentsApplySnapshot(subagentsFixture())

	return m
}

func TestSubagentsPanelHeader(t *testing.T) {
	m := newPanelModel(t)
	panel := m.subagentsPanel()

	if !strings.Contains(panel, "3 running") || !strings.Contains(panel, "1 idle") {
		t.Fatalf("header counts missing: %q", panel)
	}

	if strings.Contains(panel, "audit-1") || strings.Contains(panel, "done") {
		t.Fatalf("a finished one-shot must not appear in the panel: %q", panel)
	}

	if !strings.Contains(panel, "◆ 2 pending") {
		t.Fatalf("pending badge missing: %q", panel)
	}

	// The default window anchors at the TAIL (D4): with 4 active agents and
	// a 2-row cap the offset starts at the bottom, so only the up arrow
	// shows.
	if !strings.Contains(panel, " ↑ ") {
		t.Fatalf("up indicator missing at the tail anchor: %q", panel)
	}

	if !strings.Contains(panel, "watch-1") || strings.Contains(panel, "explore-1") {
		t.Fatalf("default window must show the most recent rows: %q", panel)
	}
}

func TestSubagentsPanelBoundedRowsAndScroll(t *testing.T) {
	m := newPanelModel(t)

	// Only the ACTIVE agents (running + idle) produce rows: 4, above the
	// visible cap of 2 — the finished one-shots are filtered out. The box
	// adds its two border lines.
	if h := m.subagentsPanelHeight(); h != subagentsPanelMaxRows+2 {
		t.Fatalf("height = %d, want %d", h, subagentsPanelMaxRows+2)
	}

	// Rows: box newlines = rows + top + bottom borders.
	if n := strings.Count(m.subagentsPanel(), "\n"); n != subagentsPanelMaxRows+1 {
		t.Fatalf("rows = %d, want %d (rows + the two border lines)", n, subagentsPanelMaxRows+1)
	}

	// The default window anchors at the tail: the two most recent agents.
	panel := m.subagentsPanel()
	if !strings.Contains(panel, "runner-3") || !strings.Contains(panel, "watch-1") {
		t.Fatalf("tail-anchored default rows missing: %q", panel)
	}
	if strings.Contains(panel, "explore-1") || strings.Contains(panel, "runner-2") {
		t.Fatalf("older agents must not render in the default window: %q", panel)
	}

	// Scroll down to the last row set (already the tail) and back up to the
	// oldest one; the indicator flips.
	m.subagentsPanelScroll(10)
	panel = m.subagentsPanel()
	if !strings.Contains(panel, "watch-1") {
		t.Fatalf("scroll did not reach the last active row: %q", panel)
	}

	if !strings.Contains(panel, " ↑ ") {
		t.Fatalf("up indicator missing at the bottom: %q", panel)
	}

	// Clamped at both ends.
	if m.subagentsScroll != 4-subagentsPanelMaxRows {
		t.Fatalf("scroll not clamped: %d", m.subagentsScroll)
	}

	m.subagentsPanelScroll(-10)
	if m.subagentsScroll != 0 {
		t.Fatalf("scroll not clamped at 0: %d", m.subagentsScroll)
	}

	if !strings.Contains(m.subagentsPanel(), " ↓ ") {
		t.Fatalf("down indicator missing at the top: %q", m.subagentsPanel())
	}
}

func TestSubagentsPanelActivityAndQueuedMail(t *testing.T) {
	m := newPanelModel(t)
	// The default window shows the LAST two active agents: runner-3 (index
	// 2) and watch-1 (index 3). Style the visible row.
	m.subagents.Agents[2].Activity = "using_tool ha_state_query"
	m.subagents.Agents[3].QueuedMail = 2

	panel := m.subagentsPanel()
	if !strings.Contains(panel, "using_tool ha_state_query") {
		t.Fatalf("activity label missing: %q", panel)
	}

	if !strings.Contains(panel, "2 queued") {
		t.Fatalf("queued-mail count missing: %q", panel)
	}
}

func TestSubagentsPanelHiddenWhenEmpty(t *testing.T) {
	m := &Model{}
	if h := m.subagentsPanelHeight(); h != 0 {
		t.Fatalf("nil snapshot height = %d", h)
	}

	m.subagents = &protocol.SubagentsInfo{}
	if h := m.subagentsPanelHeight(); h != 0 {
		t.Fatalf("empty snapshot height = %d", h)
	}
}

// Pending-only state renders NO panel (D3): a non-zero pending_handbacks
// alone never keeps the box alive.
func TestSubagentsPanelPendingOnlyNoPanel(t *testing.T) {
	m := newPanelModel(t)
	m.subagents = &protocol.SubagentsInfo{PendingHandbacks: 1}
	if h := m.subagentsPanelHeight(); h != 0 {
		t.Fatalf("pending-only snapshot height = %d, want 0", h)
	}
}

// A finished one-shot drops out of the panel (D3): the last active agent
// completing with nothing pending removes the box entirely.
func TestSubagentsPanelFinishedOneShotDropsOut(t *testing.T) {
	m := newPanelModel(t)
	m.subagents = &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: tNameDailyBrief, Type: pTypeGP, Status: tStatusRunning},
		},
	}
	if h := m.subagentsPanelHeight(); h != 3 { // 1 row + 2 borders
		t.Fatalf("one running one-shot: height = %d, want 3", h)
	}

	// The dispatch completes; no result pending, nothing active.
	m.subagents = &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: tNameDailyBrief, Type: pTypeGP, Status: tStatusDone},
		},
		PendingHandbacks: 0,
	}
	if h := m.subagentsPanelHeight(); h != 0 {
		t.Fatalf("finished one-shot: height = %d, want 0", h)
	}
}

// D4: a tail-anchored window stays tail-anchored on new snapshots; a user
// scrolled into history keeps their offset (clamped).
func TestSubagentsPanelTailAnchor(t *testing.T) {
	m := newPanelModel(t)

	// Scroll up into history.
	m.subagentsPanelScroll(-10)
	if m.subagentsScroll != 0 || m.subagentsAtTail {
		t.Fatalf("scrolled state wrong: offset=%d atTail=%v", m.subagentsScroll, m.subagentsAtTail)
	}

	// A new snapshot with the same fleet: the scrolled offset is kept.
	m.subagentsApplySnapshot(subagentsFixture())
	if m.subagentsScroll != 0 || m.subagentsAtTail {
		t.Fatalf("user offset not kept: offset=%d atTail=%v", m.subagentsScroll, m.subagentsAtTail)
	}

	// Scroll back to the bottom: the tail anchor re-arms.
	m.subagentsPanelScroll(10)
	if !m.subagentsAtTail {
		t.Fatal("tail anchor not re-armed at the bottom")
	}

	// A new snapshot with a growing fleet re-anchors at the tail.
	grown := subagentsFixture()
	grown.Agents = append(grown.Agents,
		protocol.SubagentsAgentView{SubagentID: "id-8", Name: "watch-2", Type: pTypeGP, Status: tStatusRunning})
	m.subagentsApplySnapshot(grown)
	if m.subagentsScroll != 5-subagentsPanelMaxRows {
		t.Fatalf("tail not re-derived: offset=%d, want %d", m.subagentsScroll, 5-subagentsPanelMaxRows)
	}

	// A shrinking fleet clamps a scrolled-out offset.
	m.subagentsPanelScroll(-10)
	shrunk := &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: "solo", Type: pTypeGP, Status: tStatusRunning},
		},
	}
	m.subagentsApplySnapshot(shrunk)
	if m.subagentsScroll != 0 {
		t.Fatalf("offset not clamped: %d", m.subagentsScroll)
	}
}

// D6: the counts never touch the border dashes — one space on each side.
func TestSubagentsPanelCountsNeverTouchBorder(t *testing.T) {
	m := newPanelModel(t)
	m.subagents = &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: "idle-1", Type: pTypeGP, Status: "idle"},
		},
		PendingHandbacks: 1,
	}

	top := ansi.Strip(strings.SplitN(m.subagentsPanel(), "\n", 2)[0])
	want := "── 0 running · 1 idle · ◆ 1 pending ──╮"
	if !strings.Contains(top, want) {
		t.Fatalf("top border %q does not contain %q", top, want)
	}

	// Running variant.
	m.subagents = &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: "run-1", Type: pTypeGP, Status: tStatusRunning},
		},
		PendingHandbacks: 1,
	}
	top = ansi.Strip(strings.SplitN(m.subagentsPanel(), "\n", 2)[0])
	want = "── 1 running · ◆ 1 pending ──╮"
	if !strings.Contains(top, want) {
		t.Fatalf("top border %q does not contain %q", top, want)
	}
}

func TestSubagentsPanelRowPadding(t *testing.T) {
	// A single-row fleet whose row is the box's widest content: the spec
	// (chat-tui-rendering "Subagent panel") requires the rows to sit
	// "padded one cell from the borders", including the widest one.
	info := &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{
				SubagentID: tID1,
				Name:       "sub-a1",
				Type:       pTypeExplore,
				Status:     tStatusRunning,
				StartedAt:  "2026-01-02T10:30:00Z",
				Activity:   "scanning",
			},
		},
	}

	// Row content width: HH:MM(5) + 2 + name(6) + 1 + (explore)(9) + 2 +
	// running(7) + 1 + "· scanning"(10) = 43. Width 47 makes the width-2
	// box cap bind exactly (43+2 = 47-2) and must still keep the padding;
	// 48 is a plain narrow fit, 120 a wide terminal.
	for _, width := range []int{47, 48, 120} {
		m := NewModel(nil, Options{})
		m.width, m.height = width, 30
		m.applyLayout()
		m.subagentsApplySnapshot(info)

		panel := m.subagentsPanel()
		if panel == "" {
			t.Fatalf("width %d: panel missing", width)
		}

		lines := strings.Split(strings.TrimPrefix(panel, "\n"), "\n")
		row := ""
		widths := make([]int, 0, len(lines))
		for _, line := range lines {
			plain := ansi.Strip(line)
			if strings.HasPrefix(plain, "│") {
				row = plain
			}
			widths = append(widths, ansi.StringWidth(plain))
		}

		// Nothing overflows the terminal, and every box line is the same
		// width (the box hugs its content: 43 + 2 padding + 2 borders).
		for i, w := range widths {
			if w > width {
				t.Fatalf("width %d: line %d is %d cells wide, exceeds the terminal:\n%s", width, i, w, panel)
			}
			if w != widths[0] {
				t.Fatalf("width %d: line %d is %d cells wide, box lines misaligned (%d):\n%s", width, i, w, widths[0], panel)
			}
		}

		if row == "" {
			t.Fatalf("width %d: no fleet row rendered:\n%s", width, panel)
		}

		// Exactly one space between the borders and the widest row's text.
		if !strings.HasPrefix(row, "│ ") || strings.HasPrefix(row, "│  ") {
			t.Fatalf("width %d: row not padded one cell from the left border: %q", width, row)
		}
		if !strings.HasSuffix(row, " │") || strings.HasSuffix(row, "  │") {
			t.Fatalf("width %d: row not padded one cell from the right border: %q", width, row)
		}
	}
}

func TestSubagentsPanelClearedOnSessionChanged(t *testing.T) {
	m := newPanelModel(t)
	if m.subagentsPanelHeight() == 0 {
		t.Fatal("panel should be visible")
	}

	// The session_changed branch clears the view (task 6.4).
	m.subagents = nil
	m.subagentsScroll = 0

	if m.subagentsPanelHeight() != 0 {
		t.Fatal("panel not cleared")
	}
}

func TestSubagentsWheelRouting(t *testing.T) {
	m := newPanelModel(t)

	// A wheel event over the panel rows scrolls the panel, not the
	// transcript. The panel renders directly below the viewport, and after
	// the D1 fix the layout arithmetic matches the render (R4). Start
	// scrolled into history (the default window anchors at the tail, D4)
	// so a down-wheel actually moves.
	m.subagentsPanelScroll(-10)
	top := m.vp.Height()
	if top == 0 {
		t.Skip("an empty transcript gives a zero-height viewport")
	}

	before := m.subagentsScroll
	m.handleMouseEvent(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelDown, Y: top}))
	if m.subagentsScroll != before+1 {
		t.Fatalf("panel wheel scroll = %d, want %d", m.subagentsScroll, before+1)
	}

	// A wheel event over the transcript does not touch the panel.
	before = m.subagentsScroll
	m.handleMouseEvent(tea.MouseWheelMsg(tea.Mouse{Button: tea.MouseWheelUp, Y: 0}))
	if m.subagentsScroll != before {
		t.Fatalf("transcript wheel touched the panel")
	}
}

func TestHandbackDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, ""},
		{-5, ""},
		{500, "0s"},
		{12000, "12s"},
		{252000, "4m12s"},
		{252900, "4m12s"},
		{3720000, "1h2m0s"},
	}
	for _, c := range cases {
		if got := handbackDuration(c.ms); got != c.want {
			t.Fatalf("handbackDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// The handback notice line gains the duration segment when elapsed_ms is
// present (protocol 6) and degrades to the plain two-segment line when the
// field is absent (an old daemon).
func TestSubagentHandbackLineDuration(t *testing.T) {
	m := NewModel(nil, Options{})
	m.handleControl(protocol.Response{
		ConnectionID: "c1",
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlSubagentHandback,
		Handback:     &protocol.HandbackNotice{Name: tNameDailyBrief, Status: tStatusCompleted, ElapsedMS: 252000},
	})

	if len(m.completed) == 0 {
		t.Fatal("no transcript entry recorded")
	}
	last := m.completed[len(m.completed)-1]
	if !strings.Contains(last.info, "── subagent daily-brief · completed · 4m12s ──") {
		t.Fatalf("duration segment missing: %q", last.info)
	}

	// Field absent: the plain two-segment line.
	m.handleControl(protocol.Response{
		ConnectionID: "c1",
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlSubagentHandback,
		Handback:     &protocol.HandbackNotice{Name: tNameDailyBrief, Status: tStatusCompleted},
	})
	last = m.completed[len(m.completed)-1]
	if strings.Contains(last.info, "· 4m12s") || !strings.Contains(last.info, "── subagent daily-brief · completed ──") {
		t.Fatalf("field-absent line wrong: %q", last.info)
	}
}

// Regression (2026-11-19): a handback trace arriving while a model turn is
// in flight renders IMMEDIATELY inside the live turn — between the prompt
// echo and the streaming/final reply — and is archived WITH the turn.
func TestSubagentHandbackLineInsideLiveTurn(t *testing.T) {
	m := NewModel(nil, Options{})
	// An in-flight turn: the prompt echo is live, no completed entry yet.
	m.curEcho = "spawn a subagent"
	m.state = stateThinking
	m.waiting = true

	// The fast one-shot finishes BEFORE the owner's reply: the trace must
	// be visible at once, inside the live region.
	m.handleControl(protocol.Response{
		ConnectionID: "c1",
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlSubagentHandback,
		Handback:     &protocol.HandbackNotice{Name: tNameDailyBrief, Status: tStatusCompleted, ElapsedMS: 252000},
	})
	if len(m.completed) != 0 {
		t.Fatalf("the trace must not touch completed turns mid-flight: %+v", m.completed)
	}
	if len(m.body) != 1 || m.body[0].kind != bodyInfo || !strings.Contains(m.body[0].text, "4m12s") {
		t.Fatalf("trace not in the live turn: %v", m.body)
	}

	live := m.buildContent()
	echoAt := strings.Index(live, "spawn a")
	traceAt := strings.Index(live, "── subagent daily-brief · completed · 4m12s ──")
	if echoAt < 0 || traceAt < 0 || traceAt < echoAt {
		t.Fatalf("trace not rendered between the echo and the preview:\n%s", live)
	}

	// The owner's reply completes: the trace is archived WITH the turn,
	// between the echo and the reply.
	m.completeTurn("avviato")
	if len(m.body) != 0 {
		t.Fatalf("live body not cleared: %v", m.body)
	}
	if len(m.completed) != 1 || len(m.completed[0].body) != 1 {
		t.Fatalf("trace not archived with the turn: %+v", m.completed)
	}

	done := m.buildContent()
	echoAt = strings.Index(done, "spawn a")
	traceAt = strings.Index(done, "── subagent daily-brief · completed · 4m12s ──")
	replyAt := strings.Index(done, "avviato")
	if echoAt < 0 || traceAt < 0 || replyAt < 0 || echoAt >= traceAt || traceAt >= replyAt {
		t.Fatalf("archived order wrong (echo < trace < reply):\n%s", done)
	}
}

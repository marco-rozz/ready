package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

const testSession = "sess"

// newUsageTestModel builds a model with fixed dimensions and a fake dealer.
func newUsageTestModel(t *testing.T, opts Options) *Model {
	t.Helper()

	m := NewModel(&fakeDealer{}, opts)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.ta.Width() // touch to keep the linter honest about the field's use

	return m
}

// TestUsageSegmentHiddenBeforeData pins the "Footer stays minimal before any
// usage" scenario: no usage data → no usage segment.
func TestUsageSegmentHiddenBeforeData(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})

	if seg := m.usageSegment(); seg != "" {
		t.Fatalf("usage segment must be hidden before data, got %q", seg)
	}
}

// TestUsageSegmentAfterCompletedTurn pins the "Footer after a completed turn"
// scenario: Read/Cache%/Written, occupancy and the turn count all render.
func TestUsageSegmentAfterCompletedTurn(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	m.usage = &protocol.UsageInfo{
		ReadTokens:          68000,
		WrittenTokens:       5400,
		CachedTokens:        38000,
		ContextOccupancyPct: 17.5,
		TurnCount:           12,
		LLMCalls:            61,
	}

	seg := m.usageSegment()
	for _, want := range []string{"R68.0k", "C55.9%", "O5.4k", "ctx 18%", "[12»61]"} {
		if !strings.Contains(seg, want) {
			t.Errorf("usage segment %q does not contain %q", seg, want)
		}
	}
}

// TestUsageSegmentOmitsAbsentSubsegments pins the "Ollama session shows no
// cache segment" scenario: absent cached tokens and occupancy are omitted,
// never rendered as zero.
func TestUsageSegmentOmitsAbsentSubsegments(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	m.usage = &protocol.UsageInfo{ReadTokens: 1000, WrittenTokens: 200, TurnCount: 3, LLMCalls: 5}

	// Also pins the internal-React-iterations visibility scenario: the
	// bracketed count is "user turns · LLM calls", so one user request that
	// ran three ReAct iterations renders as [3 · 5] — the two figures
	// diverge exactly when the agent used tools (LLMCalls > TurnCount).
	m.usage = &protocol.UsageInfo{ReadTokens: 1000, WrittenTokens: 200, TurnCount: 3, LLMCalls: 5}

	seg := m.usageSegment()
	if strings.Contains(seg, "C") && strings.Contains(seg, "%") {
		// The only C that may appear is none: no cache sub-segment at all.
		t.Errorf("cache sub-segment must be absent when the provider reports no cached tokens, got %q", seg)
	}
	if strings.Contains(seg, "ctx") {
		t.Errorf("occupancy must be absent without a window, got %q", seg)
	}
	if !strings.Contains(seg, "R1.0k") || !strings.Contains(seg, "O200") {
		t.Errorf("usage segment = %q, want R/O present", seg)
	}
	if !strings.Contains(seg, "[3»5]") {
		t.Errorf("usage segment = %q, want [user turns»LLM calls] present", seg)
	}
}

// TestProgressChunkUpdatesUsageLive pins the "live footer counters" scenario:
// a progress frame carrying usage totals updates the segment mid-turn (LLM
// calls tick per ReAct iteration), while a progress frame without usage
// leaves the existing totals untouched.
func TestProgressChunkUpdatesUsageLive(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	seed := &protocol.UsageInfo{ReadTokens: 100, WrittenTokens: 10, TurnCount: 2, LLMCalls: 3}
	m.usage = seed

	// Mid-turn tick: a new ReAct iteration completed on the daemon.
	m = update(t, m, progressChunk{
		Content: "partial",
		Usage:   &protocol.UsageInfo{ReadTokens: 900, WrittenTokens: 90, TurnCount: 2, LLMCalls: 4},
	})
	if got := m.usageSegment(); !strings.Contains(got, "[2»4]") {
		t.Errorf("usage segment after live progress = %q, want LLM counter advanced", got)
	}

	// A progress frame without usage (legacy daemon / fresh session) must not
	// wipe the last known totals.
	m = update(t, m, progressChunk{Content: "more"})
	if got := m.usageSegment(); !strings.Contains(got, "[2»4]") {
		t.Errorf("usage segment after usage-less progress = %q, want totals retained", got)
	}
}

// TestUsageSegmentContextTint pins the colour-threshold scenario: 72% →
// yellow, 92% → red, below → no tint.
func TestUsageSegmentContextTint(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{17.5, ""},
		{69.9, ""},
		{72, "3"},
		{89.9, "3"},
		{92, "1"},
	}
	for _, tc := range cases {
		if got := ctxOccupancyColor(tc.pct); got != tc.want {
			t.Errorf("ctxOccupancyColor(%.1f) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

// TestStatusLineDropsSessionAndCwd pins the footer amendment: the session ID
// and CWD no longer appear in the status line.
func TestStatusLineDropsSessionAndCwd(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSessionID, Sanitizer: privacy.New(nil)})
	m.modelRef = "ollama-gemma::gemma4:31b"
	m.modelState = protocol.ModelStateDefault

	line := m.statusLine()
	if strings.Contains(line, "abcdefgh") {
		t.Errorf("status line must not contain the session id: %q", line)
	}
	if strings.Contains(line, "/tmp") || strings.Contains(line, "cwd") {
		t.Errorf("status line must not contain the cwd: %q", line)
	}
}

// TestStatusLineAlwaysShowsLabel pins that the footer always carries the
// turn-state label in the bottom-left corner, including while a turn is in
// flight and the working spinner is embedded in the border.
func TestStatusLineAlwaysShowsLabel(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})

	if got := m.statusLine(); !strings.Contains(got, "idle") {
		t.Errorf("idle footer must show the state label: %q", got)
	}

	m.state = stateStreaming
	if got := m.statusLine(); !strings.Contains(got, "streaming") {
		t.Errorf("working footer must keep the state label: %q", got)
	}
}

// TestInputBarsNoIndicatorsWhenInputFits pins "Bars are empty of indicators
// when input fits".
func TestInputBarsWhenInputFits(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	m.applyLayout()

	top, bottom := m.inputBars()
	if strings.Contains(top, "more") || strings.Contains(bottom, "more") {
		t.Errorf("bars must be plain rules when input fits: top=%q bottom=%q", top, bottom)
	}
}

// TestInputBarsScrollIndicators pins the "Input scrolls instead of growing
// past the cap" scenario: with content beyond the cap, the bars carry the
// centered ↑/↓ N more indicators following the cursor.
func TestInputBarsScrollIndicators(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})

	// 12 logical lines: cap at 24 rows = max(5, 7) = 7 visible lines.
	content := "l0\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11"
	m.ta.InsertString(content)
	m.applyLayout()
	m.ta.View() // first content render: lets the internal viewport track
	m.ta.MoveToEnd()

	// Cursor at the end: offset follows the cursor — hidden above, none below.
	above, below := m.hiddenInputLines()
	if above != 5 || below != 0 {
		t.Fatalf("cursor-at-end hidden = (%d, %d), want (5, 0)", above, below)
	}

	top, bottom := m.inputBars()
	if !strings.Contains(top, "↑ 5 more") {
		t.Errorf("top bar must carry '↑ 5 more', got %q", top)
	}
	if strings.Contains(bottom, "more") {
		t.Errorf("bottom bar must be a plain rule, got %q", bottom)
	}

	// Cursor at the top: everything above visible, hidden below.
	m.ta.MoveToBegin()
	m.applyLayout()
	m.ta.View()
	above, below = m.hiddenInputLines()
	if above != 0 || below != 5 {
		t.Fatalf("cursor-at-start hidden = (%d, %d), want (0, 5)", above, below)
	}

	top, bottom = m.inputBars()
	if strings.Contains(top, "more") {
		t.Errorf("top bar must be a plain rule with the cursor at the top, got %q", top)
	}
	if !strings.Contains(bottom, "↓ 5 more") {
		t.Errorf("bottom bar must carry '↓ 5 more', got %q", bottom)
	}
}

// TestInputBarsSpinnerWithIndicator pins the "Spinner yields to scroll
// indicators" scenario, ported verbatim from Pi Agent's
// CustomEditor.renderTopBorder: the indicator is centered in the space left
// by the left-aligned spinner+label block when it fits; otherwise the label
// degrades to the lone frame and the indicator survives only if it then fits
// beside it; on very narrow terminals the indicator is dropped entirely.
func TestInputBarsSpinnerWithIndicator(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	m.state = stateStreaming
	m.ta.InsertString("l0\nl1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\nl11")
	m.applyLayout()
	m.ta.View()
	m.ta.MoveToEnd()

	top, _ := m.inputBars()
	if !strings.Contains(top, "↑ 5 more") {
		t.Fatalf("top bar must carry the scroll indicator during a turn, got %q", top)
	}
	// Both fit on an 80-col bar: full spinner + indicator.
	if !strings.Contains(top, "READY.") {
		t.Errorf("top bar must embed the working spinner when both fit, got %q", top)
	}
	// Pi layout: the spinner block is left-aligned, not centered.
	if !strings.HasPrefix(top, "── ") {
		t.Errorf("spinner block must sit left-aligned after '── ', got %q", top)
	}

	// Intermediate width: the full label no longer fits beside the centered
	// indicator — compact frame + indicator (Pi's degrade path).
	m.Update(tea.WindowSizeMsg{Width: 24, Height: 24})
	m.applyLayout()
	top, _ = m.inputBars()
	if strings.Contains(top, "READY.") {
		t.Errorf("spinner must drop to compact form when both do not fit, got %q", top)
	}
	if !strings.Contains(top, "↑") {
		t.Errorf("scroll indicator must survive the compact fallback, got %q", top)
	}

	// Very narrow terminal: even the compact frame cannot host the indicator
	// — Pi drops it entirely and keeps the lone frame after a short prefix.
	m.Update(tea.WindowSizeMsg{Width: 16, Height: 24})
	m.applyLayout()
	top, _ = m.inputBars()
	if strings.Contains(top, "READY.") || strings.Contains(top, "↑") {
		t.Errorf("very narrow bar must keep the lone spinner only, got %q", top)
	}
}

// TestSendInputInterceptsReservedCommands pins the "Reserved slash commands"
// scenarios: /session and /context are sent as their control request types
// with no user echo and no thinking state; any other slash input is forwarded
// to the model byte-identically.
func TestSendInputInterceptsReservedCommands(t *testing.T) {
	for _, cmd := range []string{"/session", "/context"} {
		m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
		d, ok := m.dealer.(*fakeDealer)
		if !ok {
			t.Fatalf("dealer is %T", m.dealer)
		}

		m.ta.InsertString(cmd)
		tm, send := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		mm, ok := tm.(*Model)
		if !ok {
			t.Fatalf("handleKey returned %T", tm)
		}
		_ = send() // execute the queued send command
		if mm.curEcho != "" {
			t.Errorf("%s must not be echoed as a user turn", cmd)
		}
		if mm.waiting {
			t.Errorf("%s must not arm the turn state", cmd)
		}
		if len(d.sends) != 1 {
			t.Fatalf("expected exactly one sent frame for %s, got %d", cmd, len(d.sends))
		}

		// The sent frame must be the control request type.
		req, err := protocol.UnmarshalRequest(d.sends[0])
		if err != nil {
			t.Fatalf("unmarshal sent request for %s: %v", cmd, err)
		}
		wantType, okCmd := reservedCommand(cmd)
		if !okCmd {
			t.Fatalf("%s is not a reserved command", cmd)
		}
		if req.Type != wantType {
			t.Errorf("%s sent as %q, want %q", cmd, req.Type, wantType)
		}
	}
}

// TestSendInputNonReservedSlashReachesModel pins "Non-reserved slash input
// reaches the model".
func TestSendInputNonReservedSlashReachesModel(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	d, ok := m.dealer.(*fakeDealer)
	if !ok {
		t.Fatalf("dealer is %T", m.dealer)
	}

	text := "/help me please" // starts with "/" but is not reserved
	m.ta.InsertString(text)
	tm, batch := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	// The turn send is a tea.Batch (message + timeout + tick): only the first
	// sub-command sends the frame (the others sleep).
	bm, batchOK := batch().(tea.BatchMsg)
	if !batchOK {
		t.Fatalf("turn send returned %T, want tea.BatchMsg", batch())
	}
	if len(bm) > 0 {
		_ = bm[0]()
	}
	if mm.curEcho != text {
		t.Errorf("ordinary slash input must be echoed like any turn, got %q", mm.curEcho)
	}
	if len(d.sends) != 1 {
		t.Fatalf("expected one sent message, got %d", len(d.sends))
	}
	req, err := protocol.UnmarshalRequest(d.sends[0])
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Type != protocol.RequestTypeMessage || req.Content != text {
		t.Errorf("sent %q/%q, want message with byte-identical content", req.Type, req.Content)
	}
}

// TestCommandReplyRendersBlockWithoutDisturbingTurn pins that a /session
// reply arriving mid-turn renders as a transcript block and leaves the
// in-flight turn untouched.
func TestCommandReplyRendersBlockWithoutDisturbingTurn(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})

	// Start a turn (echo + waiting + live buffer).
	m.ta.InsertString("real question")
	tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m2, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	m = m2
	if !m.waiting {
		t.Fatal("expected a waiting turn")
	}

	cmdID := "cmd-req-1"
	m.pendingCtrl[cmdID] = protocol.RequestTypeSessionInfo

	tm2, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // extra newline harmless
	_ = tm2
	tm, _ = m.handleReply(protocol.Response{
		ConnectionID: "sess",
		Status:       protocol.ResponseStatusOK,
		RequestID:    cmdID,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "## Session\n\n**Session key:** `cli`",
	})
	m3, ok3 := tm.(*Model)
	if !ok3 {
		t.Fatalf("handleReply returned %T", tm)
	}
	m = m3

	// The turn is still in flight, the buffer untouched.
	if !m.waiting || m.state != stateThinking {
		t.Fatalf("command reply must not finalise the in-flight turn (waiting=%v state=%v)", m.waiting, m.state)
	}
	if len(m.completed) != 1 || m.completed[0].echo != "" {
		t.Fatalf("command reply must append an echo-less block, got %+v", m.completed)
	}
}

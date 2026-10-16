package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// steeringTestModel is a Model with a recording dealer and the window
// bootstrapped, ready to drive turn flows.
func steeringTestModel(t *testing.T) (*Model, *fakeDealer) {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID: testSessionID,
		ReplyTimeout: 0, // zero → NewModel clamps to 120s
		Sanitizer:    privacy.New(nil),
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	return m, d
}

// sendTurn types text into the editor and pushes RETURN through the real
// sendInput path, executing only the frame-sending sub-command of the
// returned Batch (the others are sleeping ticks/timeouts — never run those
// in a test).
func sendTurn(m *Model, text string) {
	m.ta.InsertString(text)
	tm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := tm.(*Model)
	if !ok {
		return
	}
	*m = *mm
	if cmd == nil {
		return
	}
	if bm, ok := cmd().(tea.BatchMsg); ok && len(bm) > 0 {
		_ = bm[0]()
	}
}

// TestSteeringReturnWhileWorkingQueues: RETURN during a live turn queues the
// prompt — no request leaves the client, the live turn's echo and stream are
// untouched, and no reply timeout is armed (nil command).
func TestSteeringReturnWhileWorkingQueues(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)

	sendTurn(m, "prima") // "prima" in flight (waiting)
	if !m.waiting || m.curEcho != "prima" {
		t.Fatalf("precondition: live turn missing (waiting=%v echo=%q)", m.waiting, m.curEcho)
	}

	sentBefore := len(d.sends)

	m.ta.InsertString("secondo")
	tm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	_ = mm

	if len(m.pending) != 1 || m.pending[0] != "secondo" {
		t.Fatalf("queue = %v, want [secondo]", m.pending)
	}
	if cmd != nil {
		t.Error("queueing must not arm a reply timeout or send anything")
	}
	if m.curEcho != "prima" {
		t.Errorf("live turn's echo clobbered by the queue: %q", m.curEcho)
	}
	if len(d.sends) != sentBefore {
		t.Errorf("%d extra frames sent while queueing, want 0", len(d.sends)-sentBefore)
	}

	// The editor is cleared for the next prompt.
	if m.ta.Value() != "" {
		t.Errorf("editor not cleared on queue: %q", m.ta.Value())
	}
}

// TestSteeringDrainsOnCompletion: when the in-flight turn's final reply
// arrives, the first queued prompt is sent immediately as a new live turn
// with a fresh timeout.
func TestSteeringDrainsOnCompletion(t *testing.T) {
	t.Parallel()

	const promptSecondo = "secondo"

	m, d := steeringTestModel(t)
	sendTurn(m, "prima")       // "prima" in flight
	sendTurn(m, promptSecondo) // queued, nothing sent

	if len(d.sends) != 1 {
		t.Fatalf("precondition: sends = %d, want 1 (only 'prima')", len(d.sends))
	}

	// The 'prima' reply arrives: the drain must fire the queued prompt.
	completeReply(m, "risposta prima")

	if m.state != stateThinking || !m.waiting || m.curEcho != promptSecondo {
		t.Fatalf("queued prompt not promoted: state=%v waiting=%v echo=%q", m.state, m.waiting, m.curEcho)
	}

	req, err := protocol.UnmarshalRequest(d.sends[1])
	if err != nil {
		t.Fatalf("unmarshal drained request: %v", err)
	}
	if req.Type != protocol.RequestTypeMessage || req.Content != promptSecondo {
		t.Fatalf("drained request = %s/%q, want message/secondo", req.Type, req.Content)
	}
}

// TestSteeringFIFOOrder: multiple queued prompts drain one per completed
// turn, in submission order.
func TestSteeringFIFOOrder(t *testing.T) {
	t.Parallel()

	const promptDue = "due"
	const promptTre = "tre"

	m, d := steeringTestModel(t)
	sendTurn(m, "uno") // "uno" in flight
	sendTurn(m, promptDue)
	sendTurn(m, promptTre)

	if len(m.pending) != 2 || m.pending[0] != promptDue || m.pending[1] != promptTre {
		t.Fatalf("queue = %v, want [due tre]", m.pending)
	}

	// Turn 1 completes → "due" sent; "tre" still queued.
	completeReply(m, "risposta uno")
	if len(m.pending) != 1 || m.pending[0] != promptTre || m.curEcho != promptDue {
		t.Fatalf("after first drain: queue=%v echo=%q", m.pending, m.curEcho)
	}

	// Turn 2 completes → "tre" sent; queue empty, back to idle.
	completeReply(m, "risposta due")
	if len(m.pending) != 0 || m.curEcho != promptTre {
		t.Fatalf("after second drain: queue=%v echo=%q", m.pending, m.curEcho)
	}

	// Turn 3 completes → idle, nothing queued.
	completeReply(m, "risposta tre")
	if m.state != stateIdle || m.waiting {
		t.Fatal("queue empty but the client never returned to idle")
	}

	// 'tre' went out: the last message frame carries it.
	req, err := protocol.UnmarshalRequest(d.sends[len(d.sends)-1])
	if err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Content != promptTre {
		t.Fatalf("FIFO violated: last sent = %q, want tre", req.Content)
	}
}

// completeReply drives one replyFinal through the model and, when the turn
// was followed by a queued-prompt drain (m.waiting is the safe
// discriminator: queue empty ⇒ the returned command is the BARE recvCmd,
// which blocks forever in tests — it must never be invoked), executes only
// the frame-sending sub-command of the drain batch.
func completeReply(m *Model, content string) {
	tm, cmd := m.Update(replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      content,
	}})
	mm, ok := tm.(*Model)
	if !ok {
		return
	}
	*m = *mm
	if !m.waiting || cmd == nil {
		return
	}
	if bm, ok := cmd().(tea.BatchMsg); ok && len(bm) > 1 {
		// Sub-command 1 is the drain (0 is the blocking recvCmd): a Batch
		// containing the queued send + timeout + tick — run only the send.
		if dbm, ok := bm[1]().(tea.BatchMsg); ok && len(dbm) > 0 {
			_ = dbm[0]()
		}
	}
}

// TestRequestInFlightErrorIsInert: the daemon's structural rejection must
// never tear down the live turn — belt-and-braces for a state the client
// can no longer reach.
func TestRequestInFlightErrorIsInert(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	sendTurn(m, "prima") // live turn
	if !m.waiting {
		t.Fatal("precondition: live turn missing")
	}

	tm, cmd := m.Update(replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    "request_in_flight",
	}})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	*m = *mm

	if m.quitting || m.exitSet {
		t.Error("request_in_flight tore the session down")
	}
	if !m.waiting {
		t.Error("live turn abandoned on request_in_flight")
	}
	if cmd == nil {
		t.Fatal("session must keep listening (recv re-armed)")
	}
}

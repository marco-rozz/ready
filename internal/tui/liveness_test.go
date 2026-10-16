package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/protocol"
)

// testErrNoActiveSession keeps the goconst literal count down (the same
// string appears in other tui test files).
const testErrNoActiveSession = "no_active_session"

// newLivenessModel builds a model with an explicit liveness timeout.
func newLivenessModel(t *testing.T, liveness time.Duration) (*Model, *fakeDealer) {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID:    testSessionID,
		ReplyTimeout:    0,
		Sanitizer:       nil,
		LivenessTimeout: liveness,
	})

	return m, d
}

// TestPingInterval_DerivedFromHandshake (task 4.3): the ping cadence is a
// third of the negotiated liveness window; zero (old daemon) keeps the
// built-in default.
func TestPingInterval_DerivedFromHandshake(t *testing.T) {
	m, _ := newLivenessModel(t, 45*time.Second)
	if m.pingInterval != 15*time.Second {
		t.Fatalf("pingInterval = %s, want 15s (timeout/3)", m.pingInterval)
	}

	m, _ = newLivenessModel(t, 0)
	if m.pingInterval != defaultPingInterval {
		t.Fatalf("pingInterval = %s, want default %s when no handshake metadata", m.pingInterval, defaultPingInterval)
	}
}

// TestPingTick_SendsBestEffortPing (task 5.1): the tick fires one ping on the
// dealer and re-arms itself. Send failures are swallowed by the command.
func TestPingTick_SendsBestEffortPing(t *testing.T) {
	m, d := newLivenessModel(t, 45*time.Second)

	tm, cmd := m.Update(pingTickMsg{})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if cmd == nil {
		t.Fatal("expected a re-armed tick command")
	}

	bm, batchOK := cmd().(tea.BatchMsg)
	if !batchOK {
		t.Fatalf("ping tick returned %T, want tea.BatchMsg", cmd())
	}
	// Sub-command 0 is the ping send (the other is the 15s Tick — never run
	// it in a test). It must not fail the TUI even when the dealer errors.
	if len(bm) == 0 {
		t.Fatal("empty ping batch")
	}
	if msg := bm[0](); msg != nil {
		t.Fatalf("ping send must swallow its own errors, got %v", msg)
	}

	d.mu.Lock()
	sent := len(d.sends)
	d.mu.Unlock()
	if sent != 1 {
		t.Fatalf("expected 1 sent frame, got %d", sent)
	}

	d.mu.Lock()
	raw := d.sends[0]
	d.mu.Unlock()

	req, err := protocol.UnmarshalRequest(raw)
	if err != nil {
		t.Fatalf("unmarshal ping request: %v", err)
	}
	if req.Type != protocol.RequestTypePing {
		t.Fatalf("expected ping request, got %q", req.Type)
	}
	if req.ConnectionID != testSessionID {
		t.Fatalf("expected session id %q, got %q", testSessionID, req.ConnectionID)
	}
	if len(mm.pendingPings) != 1 {
		t.Fatalf("expected 1 tracked in-flight ping, got %d", len(mm.pendingPings))
	}
}

// TestPingTick_StoppedWhenQuitting (task 5.4): no ping during teardown.
func TestPingTick_StoppedWhenQuitting(t *testing.T) {
	m, d := newLivenessModel(t, 45*time.Second)
	m.quitting = true

	tm, cmd := m.Update(pingTickMsg{})
	if _, ok := tm.(*Model); !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if cmd != nil {
		t.Fatal("quitting model must not re-arm the ping tick")
	}

	d.mu.Lock()
	sent := len(d.sends)
	d.mu.Unlock()
	if sent != 0 {
		t.Fatalf("expected 0 sent frames while quitting, got %d", sent)
	}
}

// TestPingErrorReply_IsInert (task 5.2 + 6.1): a failed heartbeat never quits
// the TUI — neither an old daemon's malformed_request rejection (no request
// id echoed) nor a no_active_session from a released session.
func TestPingErrorReply_IsInert(t *testing.T) {
	m := newTestModelWithPings(t, "ping-1")

	tm, cmd := m.Update(controlAfter{Resp: protocol.Response{
		Status:    protocol.ResponseStatusError,
		RequestID: "ping-1",
		ErrorCode: testErrNoActiveSession,
	}})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if mm.exitSet {
		t.Fatal("a ping error must not quit the TUI")
	}
	if cmd == nil {
		t.Fatal("expected the recv loop to be re-issued")
	}
	if len(mm.pendingPings) != 0 {
		t.Fatalf("ping bookkeeping not consumed, %d left", len(mm.pendingPings))
	}

	// Old daemon shape: empty request id + malformed_request.
	tm, cmd = mm.Update(controlAfter{Resp: protocol.Response{
		Status:    protocol.ResponseStatusError,
		ErrorCode: "malformed_request",
	}})
	mm, ok = tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if mm.exitSet {
		t.Fatal("an old-daemon ping rejection must not quit the TUI")
	}
	if cmd == nil {
		t.Fatal("expected the recv loop to be re-issued")
	}

	// A non-ping error still terminates (existing semantics).
	tm, _ = mm.Update(controlAfter{Resp: protocol.Response{
		Status:    protocol.ResponseStatusError,
		RequestID: "not-a-ping",
		ErrorCode: "session_timeout",
	}})
	mm, ok = tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if !mm.exitSet {
		t.Fatal("a non-ping error control must still quit the TUI")
	}
}

// TestPingOKReply_IsInert (task 5.2): the daemon's ok ping control carries no
// model state and must leave the TUI untouched, consuming the ping bookkeeping.
func TestPingOKReply_IsInert(t *testing.T) {
	m := newTestModelWithPings(t, "ping-ok")

	tm, cmd := m.Update(controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		RequestID:    "ping-ok",
		ResponseType: protocol.ResponseTypeControl,
	}})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	if mm.exitSet {
		t.Fatal("a ping ok reply must not quit the TUI")
	}
	if mm.state != stateIdle {
		t.Fatalf("ping ok reply disturbed turn state: %v", mm.state)
	}
	if cmd == nil {
		t.Fatal("expected the recv loop to be re-issued")
	}
	if len(mm.pendingPings) != 0 {
		t.Fatalf("ping bookkeeping not consumed, %d left", len(mm.pendingPings))
	}
}

// newTestModelWithPings builds a base test model with one tracked in-flight
// ping request id.
func newTestModelWithPings(t *testing.T, pingID string) *Model {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID: testSessionID,
		ReplyTimeout: 0,
		Sanitizer:    nil,
	})
	m.pendingPings[pingID] = struct{}{}

	return m
}

package tui

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// ANSI escapes produced by the status line's model region (lipgloss renders
// the colour regardless of the output writer's profile — stripping happens
// at write time, not render time).
const (
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"

	testDefaultRef  = "ollama-gemma::gemma4:31b"
	testFallbackRef = "or-gemma::google/gemma-3-27b-it"
)

// --- status-line model region rendering (task 3.2) --------------------------

// TestStatusLineModelRegionRendering covers the chat-tui-rendering scenarios
// "Default model is green", "Fallback promotion is yellow", "No model is red",
// plus the omitted-when-unknown case.
func TestStatusLineModelRegionRendering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		ref        string
		state      protocol.ModelState
		wantText   string
		wantEscape string
	}{
		{"default is green", testDefaultRef, protocol.ModelStateDefault, testDefaultRef, ansiGreen},
		{"fallback is yellow", testFallbackRef, protocol.ModelStateFallback, testFallbackRef, ansiYellow},
		{"unavailable is red", "", protocol.ModelStateUnavailable, "LLM unavailable", ansiRed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := newTestModel(t)
			m.modelRef = tc.ref
			m.modelState = tc.state

			line := m.statusLine()
			if !strings.Contains(line, tc.wantText) {
				t.Errorf("status line %q does not show %q", line, tc.wantText)
			}

			if !strings.Contains(line, tc.wantEscape) {
				t.Errorf("status line %q does not carry the expected colour escape %q", line, tc.wantEscape)
			}
		})
	}

	t.Run("unknown state omits the region", func(t *testing.T) {
		t.Parallel()

		m := newTestModel(t)
		if line := m.statusLine(); strings.Contains(line, "LLM") {
			t.Errorf("status line %q should omit the model region when no status was reported", line)
		}
	})
}

// TestHandshakeSeedsModelRegion verifies the Options plumbing: the model
// fields reported by the authenticated handshake reach the status line.
func TestHandshakeSeedsModelRegion(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID: "seeded-session",
		Sanitizer:    privacy.New(nil),
		ModelRef:     testDefaultRef,
		ModelState:   protocol.ModelStateDefault,
	})

	if !strings.Contains(m.statusLine(), testDefaultRef) {
		t.Errorf("status line %q does not show the handshake-reported model", m.statusLine())
	}
}

// --- model-status control consumption (task 3.1) -----------------------------

// TestIdleModelStatusControlUpdatesStatusLine covers the chat-tui-rendering
// scenario "Status changes while idle": a control event with model_state
// updates the status line without requiring user input and without exiting.
func TestIdleModelStatusControlUpdatesStatusLine(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		ModelRef:     testFallbackRef,
		ModelState:   protocol.ModelStateFallback,
	}})

	if m.state != stateIdle {
		t.Errorf("turn state = %v, want idle (a model event must not touch turn state)", m.state)
	}

	if m.exitSet {
		t.Errorf("model event must not set an exit reason (got %+v)", m.exitReason)
	}

	line := m.statusLine()
	if !strings.Contains(line, testFallbackRef) {
		t.Errorf("status line %q does not show the promoted fallback", line)
	}

	if !strings.Contains(line, ansiYellow) {
		t.Errorf("status line %q does not render the fallback in yellow", line)
	}
}

// TestModelStatusControlDuringStreamKeepsTranscript covers the scenario
// "Status change during a turn does not disturb the transcript": a model
// event arriving while progress chunks stream updates only the status line.
func TestModelStatusControlDuringStreamKeepsTranscript(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("turn on the kitchen light")
	m = update(t, m, keyMsg("enter"))

	m = update(t, m, progressChunk{Content: "partial answer chunk"})
	if m.state != stateStreaming {
		t.Fatalf("turn state = %v, want streaming after a progress chunk", m.state)
	}

	transcriptBefore := m.buildContent()

	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		ModelRef:     "",
		ModelState:   protocol.ModelStateUnavailable,
	}})

	if m.state != stateStreaming {
		t.Errorf("turn state = %v, want streaming (model event must not disturb the turn)", m.state)
	}

	if got := m.buildContent(); got != transcriptBefore {
		t.Errorf("transcript changed on a model event: before %q after %q", transcriptBefore, got)
	}

	line := m.statusLine()
	if !strings.Contains(line, "LLM unavailable") || !strings.Contains(line, ansiRed) {
		t.Errorf("status line %q does not show the red LLM-unavailable region", line)
	}
}

// --- llm_unavailable reply handling (tasks 3.3 / 3.5) ------------------------

const (
	testPartialChunk  = "partial answer chunk "
	testPartialChunk2 = "second chunk"
	testIncompleteMsg = "The response could not be completed. Some actions may already have run; check the current state before repeating the request."
)

// TestLLMUnavailableReplyRetainsPartialStream covers the chat-tui-rendering
// scenario "Partial stream is marked incomplete": the transcript shows the
// partial chunks followed by the warning, the TUI returns to idle, and the
// session stays open (no exit reason set).
func TestLLMUnavailableReplyRetainsPartialStream(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("do something risky")
	m = update(t, m, keyMsg("enter"))

	m = update(t, m, progressChunk{Content: testPartialChunk})
	m = update(t, m, progressChunk{Content: testPartialChunk2})

	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    protocol.ErrorCodeLLMUnavailable,
		Content:      testIncompleteMsg,
	}})

	if m.state != stateIdle {
		t.Errorf("turn state = %v, want idle after llm_unavailable", m.state)
	}

	if m.exitSet {
		t.Errorf("llm_unavailable must keep the session open, got exit reason %+v", m.exitReason)
	}

	if len(m.completed) != 1 {
		t.Fatalf("completed turns = %d, want 1", len(m.completed))
	}

	reply := m.completed[0].reply
	if !strings.Contains(reply, testPartialChunk+testPartialChunk2) {
		t.Errorf("completed reply %q does not retain the partial stream content", reply)
	}

	if !strings.Contains(reply, testIncompleteMsg) {
		t.Errorf("completed reply %q does not carry the incomplete-response warning", reply)
	}

	if strings.Index(reply, testPartialChunk) > strings.Index(reply, testIncompleteMsg) {
		t.Errorf("completed reply %q must show the partial chunks BEFORE the warning", reply)
	}

	if m.waiting || m.tickOn {
		t.Errorf("turn bookkeeping not reset: waiting=%v tickOn=%v", m.waiting, m.tickOn)
	}
}

// TestLLMUnavailableReplyWithoutPartialStream verifies the no-stream case:
// the terminal message alone is the turn's completion.
func TestLLMUnavailableReplyWithoutPartialStream(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hello")
	m = update(t, m, keyMsg("enter"))

	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    protocol.ErrorCodeLLMUnavailable,
		Content:      "The AI service is temporarily unavailable. Please try again in a few minutes.",
	}})

	if m.state != stateIdle || m.exitSet {
		t.Fatalf("llm_unavailable must complete the turn and keep the session open: state=%v exit=%v", m.state, m.exitSet)
	}

	if len(m.completed) != 1 || m.completed[0].reply != "The AI service is temporarily unavailable. Please try again in a few minutes." {
		t.Fatalf("completed reply = %+v, want the temporary-unavailability message alone", m.completed)
	}
}

// TestSessionExpiredReplyStillExits guards the boundary: session-lifecycle
// errors (no_active_session / session_timeout) on a reply remain terminal
// and exit 1 — only llm_unavailable is a completed turn.
func TestSessionExpiredReplyStillExits(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hello")
	m = update(t, m, keyMsg("enter"))

	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    "no_active_session",
	}})

	if m.exitReason.Code != exitErr {
		t.Errorf("no_active_session reply exit = %d, want %d", m.exitReason.Code, exitErr)
	}
}

// TestModelStatusControlStillListensAfterEvents feeds two consecutive model
// events and then a normal reply, proving the TUI keeps its receive loop
// alive across model events.
func TestModelStatusControlStillListensAfterEvents(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		ModelRef:     "a::1",
		ModelState:   protocol.ModelStateDefault,
	}})
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		ModelRef:     "b::2",
		ModelState:   protocol.ModelStateFallback,
	}})
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "done",
	}})

	if m.state != stateIdle || m.exitSet {
		t.Fatalf("model events must not break the receive loop: state=%v exit=%v", m.state, m.exitSet)
	}

	if !strings.Contains(m.statusLine(), "b::2") {
		t.Errorf("status line %q does not show the latest model status", m.statusLine())
	}
}

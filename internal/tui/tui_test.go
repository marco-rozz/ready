package tui

import (
	"errors"
	"image/color"
	"regexp"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// fakeDealer is a test dealer: Recv returns pre-seeded frames in order, then
// blocks (selectable via close). Send captures payloads. Close sets closed.
type fakeDealer struct {
	mu     sync.Mutex
	frames [][]byte // raw response frames to return from Recv
	sends  [][]byte
	closed bool
}

func (f *fakeDealer) Recv() (zmq4.Msg, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()

		return zmq4.Msg{}, errors.New("dealer closed")
	}
	if len(f.frames) == 0 {
		f.mu.Unlock()
		// Block forever (mimics a real waiting Recv) until Close is called.
		select {}
	}
	raw := f.frames[0]
	f.frames = f.frames[1:]
	f.mu.Unlock()

	return zmq4.Msg{Frames: [][]byte{raw}}, nil
}

func (f *fakeDealer) Send(msg zmq4.Msg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(msg.Frames) > 0 {
		f.sends = append(f.sends, msg.Frames[0])
	}

	return nil
}

func (f *fakeDealer) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()

	return nil
}

func respFrame(resp protocol.Response) []byte {
	b, _ := protocol.Marshal(resp)

	return b
}

// testSessionID is the session identity used in every model test.
const testSessionID = "abcdefgh-1234"

// testEchoMessage is the user text used in every echo-block test.
const testEchoMessage = "hello"

// newTestModel builds a Model with a fake dealer and a no-op sanitizer.
func newTestModel(t *testing.T) *Model {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID: testSessionID,
		ReplyTimeout: 0, // zero → NewModel clamps to 120s
		Sanitizer:    privacy.New(nil),
	})
	// Simulate the initial window + background-colour messages Bubble Tea
	// delivers at startup so the renderer is constructed.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(tea.BackgroundColorMsg{ /* IsDark via color.Color */ })

	return m
}

func keyMsg(keystroke string) tea.KeyPressMsg {
	switch keystroke {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "shift+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}
	case "alt+enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "ctrl+j":
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	case "ctrl+g":
		return tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	default:
		return tea.KeyPressMsg{Code: 'x'}
	}
}

func update(t *testing.T, m *Model, msg tea.Msg) *Model {
	t.Helper()
	got, _ := m.Update(msg)
	mm, ok := got.(*Model)
	if !ok {
		t.Fatalf("Update did not return *Model: %T", got)
	}

	return mm
}

// --- progressive render + reply flush (no duplication) ----------------------

func TestProgressiveRenderAndReplyFlushNoDup(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)

	// User sends a message.
	m.ta.SetValue(testEchoMessage)
	m = update(t, m, keyMsg("enter"))
	if m.ta.Value() != "" {
		t.Errorf("input not cleared after RETURN; got %q", m.ta.Value())
	}
	if m.state != stateThinking {
		t.Errorf("state = %v, want thinking after send", m.state)
	}

	// Stream two chunks; first chunk transitions thinking→streaming.
	m = update(t, m, progressChunk{Content: "Hello"})
	if m.state != stateStreaming {
		t.Errorf("state = %v, want streaming after first chunk", m.state)
	}
	m = update(t, m, progressChunk{Content: " world"})

	// Tick → livePreview rendered from the accumulated buffer.
	m = update(t, m, tickMsg{})
	if m.livePreview == "" {
		t.Fatal("expected a live preview after tick")
	}
	if !strings.Contains(m.livePreview, "Hello") {
		t.Errorf("live preview missing streamed text: %q", m.livePreview)
	}

	// Final reply arrives → authoritative flush, turn finalised.
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "Hello world!",
	}})
	if m.state != stateIdle {
		t.Errorf("state = %v, want idle after reply", m.state)
	}
	if len(m.completed) != 1 {
		t.Fatalf("completed turns = %d, want 1", len(m.completed))
	}
	if m.completed[0].reply != "Hello world!" {
		t.Errorf("completed reply = %q, want %q", m.completed[0].reply, "Hello world!")
	}
	if m.completed[0].echo != testEchoMessage {
		t.Errorf("completed echo = %q, want %q", m.completed[0].echo, testEchoMessage)
	}
	// No double print: livePreview cleared, curEcho cleared.
	if m.livePreview != "" || m.curEcho != "" {
		t.Errorf("turn not finalised: livePreview=%q curEcho=%q", m.livePreview, m.curEcho)
	}
}

func TestEarlierTurnsSurviveFlush(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)

	sendAndReply := func(text, reply string) {
		m.ta.SetValue(text)
		m = update(t, m, keyMsg("enter"))
		m = update(t, m, replyFinal{Resp: protocol.Response{
			Status:       protocol.ResponseStatusOK,
			ResponseType: protocol.ResponseTypeReply,
			Content:      reply,
		}})
	}

	sendAndReply("q1", "a1")
	sendAndReply("q2", "a2")

	// Two completed turns recorded.
	if len(m.completed) != 2 {
		t.Fatalf("completed turns = %d, want 2", len(m.completed))
	}

	// Re-render the completed transcript and confirm both survive.
	body := m.renderCompleted()
	if !strings.Contains(body, "a1") || !strings.Contains(body, "a2") {
		t.Errorf("completed transcript lost a turn: %q", body)
	}
}

func TestReplyRenderedWhenNoChunksStreamed(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("ping")
	m = update(t, m, keyMsg("enter"))
	// No progress chunks; reply directly.
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "# Full answer\n\nbody",
	}})
	if len(m.completed) != 1 || m.completed[0].reply != "# Full answer\n\nbody" {
		t.Fatalf("expected one completed turn with the full reply; got %+v", m.completed)
	}
	body := m.renderCompleted()
	if !strings.Contains(body, "Full") || !strings.Contains(body, "answer") {
		t.Errorf("full reply not rendered: %q", body)
	}
}

// --- keybindings ------------------------------------------------------------

func TestShiftReturnAndCtrlJInsertNewline(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	before := m.ta.Value()

	m = update(t, m, keyMsg("shift+enter"))
	if !strings.Contains(m.ta.Value(), "\n") {
		t.Errorf("shift+enter did not insert a newline; value=%q", m.ta.Value())
	}

	// Ctrl+J fallback.
	m = update(t, m, keyMsg("ctrl+j"))
	if strings.Count(m.ta.Value(), "\n") < 2 {
		t.Errorf("ctrl+j did not insert a second newline; value=%q", m.ta.Value())
	}

	_ = before
}

func TestCtrlDEmptyQuitsZero(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	if m.ta.Value() != "" {
		t.Fatalf("precondition: input not empty")
	}
	m = update(t, m, keyMsg("ctrl+d"))
	if !m.exitSet || m.exitReason.Code != exitOK {
		t.Errorf("ctrl+d on empty input: exit=%v set=%v, want code %d", m.exitReason, m.exitSet, exitOK)
	}
	if !m.quitting {
		t.Errorf("ctrl+d on empty input did not set quitting")
	}
}

func TestCtrlDNonEmptyKeepsDelete(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("abc")
	m = update(t, m, keyMsg("ctrl+d"))
	if m.exitSet {
		t.Errorf("ctrl+d on non-empty input quit unexpectedly (exit=%v)", m.exitReason)
	}
}

// --- exit-reason mapping ----------------------------------------------------

func TestCtrlCExits130(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, keyMsg("ctrl+c"))
	if m.exitReason.Code != exitSIGINT {
		t.Errorf("ctrl+c exit = %d, want %d", m.exitReason.Code, exitSIGINT)
	}
}

func TestInterruptMsgExits130(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, tea.InterruptMsg{})
	if m.exitReason.Code != exitSIGINT {
		t.Errorf("InterruptMsg exit = %d, want %d", m.exitReason.Code, exitSIGINT)
	}
}

func TestRecvErrorExits1(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, errMsg{err: errors.New("connection reset")})
	if m.exitReason.Code != exitErr {
		t.Errorf("recv error exit = %d, want %d", m.exitReason.Code, exitErr)
	}
	if !strings.Contains(m.exitReason.Message, "connection reset") {
		t.Errorf("recv error message = %q", m.exitReason.Message)
	}
}

func TestTimeoutExits124(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hi")
	m = update(t, m, keyMsg("enter"))
	if !m.waiting {
		t.Fatal("expected waiting=true after send")
	}
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: m.timeoutSeq})
	if m.exitReason.Code != exitTimeout {
		t.Errorf("timeout exit = %d, want %d", m.exitReason.Code, exitTimeout)
	}
	if m.exitReason.Message != "timeout" {
		t.Errorf("timeout message = %q, want %q", m.exitReason.Message, "timeout")
	}
}

func TestLateTimeoutIgnoredAfterReply(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hi")
	m = update(t, m, keyMsg("enter"))
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "ok",
	}})
	if m.waiting {
		t.Fatal("waiting should be false after reply")
	}
	m = update(t, m, timeoutMsg{})
	if m.exitSet {
		t.Errorf("late timeout triggered an exit: %v", m.exitReason)
	}
}

// TestStaleTimeoutIgnoredByNewerTurn reproduces the client-exit bug observed
// live on 2026-10-02: the 900s timer armed by turn N fired while turn N+1 was
// still waiting, and the global waiting flag attributed the stale timeout to
// the live turn — the TUI quit with exit 124 mid-turn while the daemon kept
// running it ("no pending entry for notification" flood, dropped final reply).
// A timeout message carrying an older turn's tag must never kill a newer turn.
func TestStaleTimeoutIgnoredByNewerTurn(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)

	// Turn 1: sent and completed.
	m.ta.SetValue("prima")
	m = update(t, m, keyMsg("enter"))
	staleSeq := m.turnSeq
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "ok",
	}})
	if m.waiting {
		t.Fatal("waiting should be false after turn 1")
	}

	// Turn 2: sent and in flight (waiting=true) — the state the stale timer
	// of turn 1 finds when it fires 900s later.
	m.ta.SetValue("seconda")
	m = update(t, m, keyMsg("enter"))
	if !m.waiting {
		t.Fatal("waiting should be true while turn 2 is in flight")
	}
	if m.turnSeq == staleSeq {
		t.Fatal("turn 2 must carry a different turn sequence than turn 1")
	}

	// The stale turn-1 timer fires while turn 2 is waiting: inert.
	m = update(t, m, timeoutMsg{turnSeq: staleSeq})
	if m.exitSet {
		t.Fatalf("stale turn-1 timer killed live turn 2: %v", m.exitReason)
	}
	if !m.waiting {
		t.Fatal("stale timeout must not disturb the live turn")
	}

	// The turn-2 timer (correctly doubly-tagged) still fires the documented exit.
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: m.timeoutSeq})
	if m.exitReason.Code != exitTimeout {
		t.Errorf("current-turn timeout exit = %d, want %d", m.exitReason.Code, exitTimeout)
	}
}

// TestProgressRearmKeepsStreamingTurnAlive covers the sliding-deadline core
// of the change: a turn that keeps receiving progress chunks must survive
// past the nominal reply timeout measured from send. Re-arming adds timers
// (commands cannot be cancelled) all sharing the current turnSeq, so WITHOUT
// the arm-sequence tag the earliest send-armed timer would fire first and
// end a live turn — each superseded arm must be inert and only the latest
// arm may end the turn.
func TestProgressRearmKeepsStreamingTurnAlive(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("lunga")
	m = update(t, m, keyMsg("enter"))
	sendArm := m.timeoutSeq

	// The turn streams progress beyond the nominal deadline. Each chunk
	// re-arms: the send-armed timer's arm (sendArm) is superseded at the
	// first chunk.
	for i := 0; i < 3; i++ {
		m = update(t, m, progressChunk{Content: "x"})
	}
	if m.timeoutSeq == sendArm {
		t.Fatal("progress chunks must bump the arm sequence")
	}

	// Every timer armed for this turn fires one by one, oldest first —
	// exactly what happens in production as the deadline keeps sliding.
	for arm := sendArm; arm < m.timeoutSeq; arm++ {
		m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: arm})
		if m.exitSet {
			t.Fatalf("superseded arm %d ended a turn still producing progress: %v", arm, m.exitReason)
		}
		if !m.waiting {
			t.Fatal("superseded timeout must not disturb the live turn")
		}
	}

	// Silent from here on: the latest armed timer still ends the turn
	// (documented exit 124, deadline measured from the last progress).
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: m.timeoutSeq})
	if m.exitReason.Code != exitTimeout {
		t.Errorf("current-arm timeout exit = %d, want %d", m.exitReason.Code, exitTimeout)
	}
}

// TestArmTaggedTimeoutInertAfterReply extends TestLateTimeoutIgnoredAfterReply
// to the re-arm semantics: once the reply completes the turn, a properly
// tagged CURRENT-turn timeout (the shape the re-arm path produces) must be
// inert whatever the arm number it carries.
func TestArmTaggedTimeoutInertAfterReply(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hi")
	m = update(t, m, keyMsg("enter"))

	// One re-arm happens, then the reply completes the turn.
	m = update(t, m, progressChunk{Content: "partial"})
	lastArm := m.timeoutSeq
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeReply,
		Content:      "ok",
	}})
	if m.waiting {
		t.Fatal("waiting should be false after reply")
	}

	// The last armed timer fires after completion: inert (waiting gate).
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: lastArm})
	if m.exitSet {
		t.Errorf("tagged timeout after reply triggered an exit: %v", m.exitReason)
	}
	if m.waiting {
		t.Fatal("timeout after reply must leave the TUI idle")
	}
}

// --- post-handshake control messages ---------------------------------------

func TestShutdownControlIdleExits0(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusShutdown,
		ResponseType: protocol.ResponseTypeControl,
	}})
	if m.exitReason.Code != exitOK {
		t.Errorf("idle shutdown exit = %d, want %d", m.exitReason.Code, exitOK)
	}
}

func TestShutdownControlMidTurnExits0(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hi")
	m = update(t, m, keyMsg("enter"))
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusShutdown,
		ResponseType: protocol.ResponseTypeControl,
	}})
	if m.exitReason.Code != exitOK {
		t.Errorf("mid-turn shutdown exit = %d, want %d", m.exitReason.Code, exitOK)
	}
}

func TestSessionExpiredControlExits1(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m = update(t, m, controlAfter{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeControl,
		ErrorCode:    "no_active_session",
	}})
	if m.exitReason.Code != exitErr {
		t.Errorf("session-expired exit = %d, want %d", m.exitReason.Code, exitErr)
	}
	if m.exitReason.Message != "session expired" {
		t.Errorf("session-expired message = %q, want %q", m.exitReason.Message, "session expired")
	}
	if m.state != stateError {
		t.Errorf("state = %v, want error", m.state)
	}
}

func TestErrorReplyHandledDefensively(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue("hi")
	m = update(t, m, keyMsg("enter"))
	m = update(t, m, replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    "session_timeout",
		Content:      "should not render",
	}})
	if m.exitReason.Code != exitErr {
		t.Errorf("error reply exit = %d, want %d", m.exitReason.Code, exitErr)
	}
	if m.exitReason.Message != "session expired" {
		t.Errorf("error reply message = %q, want %q", m.exitReason.Message, "session expired")
	}
}

// --- DEALER wiring: single recv, malformed-frame skip, send payload --------

func TestRecvCmdSkipsMalformedFrame(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{frames: [][]byte{
		[]byte("not json"),
		respFrame(protocol.Response{
			Status:       protocol.ResponseStatusOK,
			ResponseType: protocol.ResponseTypeReply,
			Content:      "after-skip",
		}),
	}}
	m := NewModel(d, Options{ConnectionID: "s1", Sanitizer: privacy.New(nil)})

	msg := m.recvCmd()()
	rf, ok := msg.(replyFinal)
	if !ok {
		t.Fatalf("recvCmd returned %T, want replyFinal (after skipping malformed frame)", msg)
	}
	if rf.Resp.Content != "after-skip" {
		t.Errorf("recvCmd content = %q, want %q", rf.Resp.Content, "after-skip")
	}
}

func TestSendCmdEmitsMessageRequest(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "sess-1", Sanitizer: privacy.New(nil)})

	cmd := m.sendCmd(protocol.RequestTypeMessage, "turn off the lights")
	msg := cmd()
	if _, ok := msg.(sentMsg); !ok {
		t.Fatalf("sendCmd returned %T, want sentMsg", msg)
	}
	if len(d.sends) != 1 {
		t.Fatalf("sends = %d, want 1", len(d.sends))
	}
	req, err := protocol.UnmarshalRequest(d.sends[0])
	if err != nil {
		t.Fatalf("UnmarshalRequest: %v", err)
	}
	if req.Type != protocol.RequestTypeMessage {
		t.Errorf("request type = %v, want message", req.Type)
	}
	if req.Content != "turn off the lights" {
		t.Errorf("request content = %q", req.Content)
	}
	if req.ConnectionID != "sess-1" {
		t.Errorf("request session = %q", req.ConnectionID)
	}
}

// --- redaction before echo render ------------------------------------------

func TestEchoRedactedBeforeRender(t *testing.T) {
	t.Parallel()

	home, err := homeDir()
	if err != nil {
		t.Fatalf("homeDirFunc: %v", err)
	}

	san := privacy.New(map[string]string{home: "~"})
	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s", Sanitizer: san})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	echo := m.renderEcho("my path is " + home + "/secret")
	if strings.Contains(echo, home) {
		t.Errorf("echo rendered an un-redacted home prefix: %q", echo)
	}
	if !strings.Contains(echo, "❯❯") {
		t.Errorf("echo missing ❯❯ prompt prefix: %q", echo)
	}
}

// --- Pi Agent user-message echo shape ---------------------------------------

// The user echo must render as Pi Agent's UserMessageComponent: no prompt
// prefix, one-cell text inset on both edges, a background highlight that
// spans the full line width (including padding lines above and below), and a
// colour that follows the terminal background darkness.
func TestEchoUserMessageBlockShape(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s"})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})

	echo := m.renderEcho(testEchoMessage)
	lines := strings.Split(echo, "\n")
	if len(lines) < 3 {
		t.Fatalf("echo block = %d lines, want at least 3 (padding + content): %q", len(lines), echo)
	}
	if !strings.Contains(echo, "48;2;52;53;65") { // #343541 as truecolor bg SGR
		t.Errorf("echo missing dark background colour %s: %q", userMessageBgDark, echo)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != 40 {
			t.Errorf("echo line visible width = %d, want 40 (full-width band): %q", got, line)
		}
	}
}

// On a light terminal the echo highlight must switch to the light theme
// background colour.
func TestEchoUserMessageBlockLightTheme(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s"})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})
	m.Update(tea.BackgroundColorMsg{Color: color.RGBA{R: 255, G: 255, B: 255, A: 255}})

	echo := m.renderEcho(testEchoMessage)
	if !strings.Contains(echo, "48;2;232;232;232") { // #e8e8e8 as truecolor bg SGR
		t.Errorf("echo missing light background colour %s: %q", userMessageBgLight, echo)
	}
}

// --- regression: echo block spacing + background coherence ------------------

// The echo block must carry exactly one padding band above and below the
// text. glamour prepends a leading newline to its output; trimming only the
// tail used to turn it into a second blank band at the top of the block.
func TestEchoBlockSinglePaddingBand(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s", Sanitizer: privacy.New(nil)})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})

	lines := strings.Split(m.renderEcho(testEchoMessage), "\n")
	if len(lines) != 3 {
		t.Fatalf("echo block = %d lines, want exactly 3 (top pad + text + bottom pad): %q",
			len(lines), lines)
	}
}

// Inline SGR resets inside the echoed markdown must not clear the block
// background: every reset is immediately followed by a re-paint of the band
// colour, so the highlight spans the whole row.
func TestEchoBackgroundSurvivesInlineResets(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s", Sanitizer: privacy.New(nil)})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})

	echo := m.renderEcho(testEchoMessage)
	const band = "\x1b[48;2;52;53;65"
	for _, reset := range []string{"\x1b[m", "\x1b[0m", "\x1b[49m"} {
		rest := echo
		for {
			i := strings.Index(rest, reset)
			if i < 0 {
				break
			}
			after := rest[i+len(reset):]
			// A reset at end of line is harmless: bg.Render re-establishes
			// the band at the start of every row, so only a reset followed
			// by more text on the SAME row must re-paint the band.
			if !strings.HasPrefix(after, band) && !strings.HasPrefix(after, "\n") && after != "" {
				t.Fatalf("reset %q not followed by the band colour: %q", reset, rest[i:])
			}
			rest = after
		}
	}
}

// The reply must not sit flush against the echo block: one blank row
// separates the highlighted band from the response, in both the completed
// transcript and the live current-turn view.
func TestEchoReplySpacing(t *testing.T) {
	t.Parallel()

	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: "s"})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 24})

	m.completed = append(m.completed, completedTurn{echo: testEchoMessage, reply: "reply text"})
	m.completedDirty = true
	completed := m.renderCompleted()
	if !blankRowBefore(completed, "reply text") {
		t.Errorf("completed transcript lacks blank row between echo and reply:\n%q", completed)
	}

	m.curEcho = testEchoMessage
	m.livePreview = "live reply"
	content := m.buildContent()
	if !blankRowBefore(content, "live reply") {
		t.Errorf("live view lacks blank row between echo and reply:\n%q", content)
	}
}

// blankRowBefore reports whether s contains needle preceded by a blank row
// (two consecutive newlines, ignoring ANSI escapes and the reply's own
// leading indentation).
func blankRowBefore(s, needle string) bool {
	clean := ansiRe.ReplaceAllString(s, "")
	idx := strings.Index(clean, needle)
	if idx < 0 {
		return false
	}

	return strings.HasSuffix(strings.TrimRight(clean[:idx], " \t"), "\n\n")
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Pressing RETURN must push the user echo into the viewport immediately —
// before any progress chunk or final reply arrives.
func TestSendRendersEchoImmediately(t *testing.T) {
	t.Parallel()

	m := newTestModel(t)
	m.ta.SetValue(testEchoMessage)
	m = update(t, m, keyMsg("enter"))

	if !strings.Contains(m.vp.View(), testEchoMessage) {
		t.Fatalf("user echo not rendered in the viewport at send time:\n%q", m.vp.View())
	}
}

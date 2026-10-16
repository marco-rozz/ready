package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/protocol"
)

// Exit code constants mirror the `chat` exit-code contract (see cmd/chat.go).
// They are duplicated here so the TUI can set an exit reason without importing
// package main; the dispatcher maps the numeric value to cli.Exit.
const (
	exitOK      = 0
	exitErr     = 1
	exitTimeout = 124
	exitSIGINT  = 130
)

// ExitReason is the TUI's final state, surfaced to the dispatcher after
// Program.Run returns. The TUI never calls os.Exit.
type ExitReason struct {
	Code    int
	Message string // optional human-readable line, printed to stderr by the dispatcher
}

// ErrInterrupted is re-exported here so the dispatcher does not need to depend
// on bubbletea internals to detect an out-of-band SIGINT. It wraps
// tea.ErrInterrupted.
var ErrInterrupted = tea.ErrInterrupted

// dealer is the subset of zmq4.Socket the TUI needs: send, receive, close.
// zmq4.Socket satisfies it; tests inject a fake.
//
// Guarantees at compile time that zmq4.Socket implements dealer.
var _ dealer = zmq4.Socket(nil)

type dealer interface {
	Recv() (zmq4.Msg, error)
	Send(zmq4.Msg) error
	Close() error
}

// progressChunk carries one streamed progress notification, plus the live
// session usage totals when the daemon attached them (footer counter advances
// mid-turn). Usage is nil when absent (fresh session, legacy daemon).
type progressChunk struct {
	Content string
	Usage   *protocol.UsageInfo
}

// replyFinal carries the authoritative final reply of a turn.
type replyFinal struct {
	Resp protocol.Response
}

// controlAfter carries a post-handshake control message (shutdown / session
// expiry / other server-side control).
type controlAfter struct {
	Resp protocol.Response
}

// tickMsg is the coalesced render-tick message.
type tickMsg struct{}

// timeoutMsg fires when the per-turn reply deadline elapses, triggering
// timeout handling. turnSeq tags the turn the timer was armed for and armSeq
// tags the arm itself (bumped on every arm — send and each progress-chunk
// re-arm): Bubble Tea commands cannot be cancelled, so timers accumulate
// and only the handler knows which one is current. The handler ignores any
// timeout whose tags do not BOTH match the live state — a stale timer armed
// by an older turn must never kill a newer turn still in flight, and a
// superseded timer from an earlier arm of the current turn must never end
// a turn that kept producing progress.
type timeoutMsg struct{ turnSeq, armSeq int }

// pingTickMsg fires on the liveness-heartbeat cadence (zmq-session-liveness):
// the handler sends one best-effort ping and re-arms the tick.
type pingTickMsg struct{}

// sentMsg confirms a message/exec request was queued on the dealer.
type sentMsg struct{}

// copyStatusExpireMsg clears the status line's copy confirmation
// (Amendment 3, mouse ownership).
type copyStatusExpireMsg struct{}

// copiedMsg signals the background pbcopy process completed. The result is
// best-effort (OSC52 already delivered); nothing to surface.
type copiedMsg struct{}

// errMsg surfaces a send/receive error.
type errMsg struct{ err error }

func (e errMsg) Error() string {
	if e.err == nil {
		return "chat: error"
	}

	return e.err.Error()
}

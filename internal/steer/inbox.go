// Package steer owns the per-address message inbox and the steering window
// of the Turn in flight (turn-steering S2–S4, S8, S11, S12).
//
// One mutex guards the queue, the Turn in flight and its pending steering
// candidates, so admission, claim, completion and deferral are atomic: every
// candidate ends in exactly one of injected (claimed), deferred (opens the
// next Turn) or dropped (by /stop). The dispatcher's intake goroutine is the
// only caller of Admit; the address worker is the only caller of
// Next/OpenTurn/CloseTurn.
package steer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
)

// QueueBound is the number of queued (non-candidate) items an inbox holds.
const QueueBound = 16

// StopCommand is the trimmed content of the out-of-band stop command.
const StopCommand = "/stop"

// ErrClosed is returned by Next once the inbox is closed and drained.
var ErrClosed = errors.New("steer: inbox closed")

// Disposition is what Admit decided for a message.
type Disposition int

const (
	// Queued: the message waits FIFO behind the Turn in flight (or starts a
	// Turn at once when the address is idle).
	Queued Disposition = iota
	// Steer: the message is a steering candidate of the Turn in flight.
	Steer
	// Stop: the message is the /stop command, handled out of band; it is
	// never queued and never a candidate.
	Stop
	// Dropped: the queue was saturated; the message was discarded.
	Dropped
)

// String returns the wire/diagnostic name of the disposition.
func (d Disposition) String() string {
	switch d {
	case Queued:
		return "queued"
	case Steer:
		return "steer"
	case Stop:
		return "stop"
	case Dropped:
		return "dropped"
	default:
		return "unknown"
	}
}

// Reason says why a Turn is preempted.
type Reason int

const (
	// ReasonNone: no preemption is due.
	ReasonNone Reason = iota
	// ReasonSteer: pending candidates must be claimed.
	ReasonSteer
	// ReasonStop: the user stopped the Turn.
	ReasonStop
)

// Candidate is a steering candidate: a message admitted while its Turn ran.
type Candidate struct {
	Msg        channel.Message
	ReceivedAt time.Time
}

// Admission is the outcome of Admit.
type Admission struct {
	Disposition Disposition
	// TurnID names the Turn a candidate joined or a /stop addressed (empty
	// when none).
	TurnID string
	// Stopping is set for Disposition Stop when a Turn is being stopped;
	// false means "nothing to stop".
	Stopping bool
	// Dropped lists the candidates a /stop discarded (never injected).
	Dropped []Candidate
}

// Work is the next item for the address worker.
type Work struct {
	Msg        channel.Message
	ReceivedAt time.Time
	// Deferred marks an opener that was a steering candidate deferred from
	// the previous Turn.
	Deferred bool
	// Preload holds the other deferred candidates; hand them to OpenTurn so
	// they are claimed at the Turn's first assembly.
	Preload []Candidate
}

// TurnSpec describes a Turn the worker is opening.
type TurnSpec struct {
	// ID is the Turn ID (UUIDv7 minted by the worker).
	ID string
	// Steerable: the Turn was opened by a human item and steering is enabled
	// for its channel.
	Steerable bool
	// MaxSteers bounds the candidates one Turn absorbs; <=0 means none.
	MaxSteers int
	// MaxRestarts bounds preemption restarts; <=0 means no preemption.
	MaxRestarts int
	// Preload carries the deferred candidates of the previous Turn.
	Preload []Candidate
}

type queued struct {
	msg        channel.Message
	receivedAt time.Time
}

type turnState struct {
	spec        TurnSpec
	windowOpen  bool
	candidates  []Candidate
	claimed     int
	restarts    int
	stopping    bool
	delivering  bool
	sig         chan struct{}
	sigClosed   bool
	droppedStop []Candidate
	stopMsgs    []channel.Message
}

// Inbox is the per-address queue plus steering window.
type Inbox struct {
	isCommand   func(content string) bool
	now         func() time.Time
	stopEnabled bool

	mu       sync.Mutex
	deque    []queued
	deferred *Work
	turn     *turnState
	closed   bool
	dropped  int

	wake chan struct{}
}

// Option configures an Inbox.
type Option func(*Inbox)

// WithClock overrides the admission clock (tests).
func WithClock(now func() time.Time) Option {
	return func(i *Inbox) { i.now = now }
}

// WithStopCommand makes the inbox recognise the human /stop command at
// admission. Without it /stop is an ordinary message: enable it only when the
// owner has a handler for the Stop disposition.
func WithStopCommand() Option {
	return func(i *Inbox) { i.stopEnabled = true }
}

// New builds an inbox. isCommand reports whether a message content is a
// Session command (never a steering candidate); nil means no command.
func New(isCommand func(content string) bool, opts ...Option) *Inbox {
	in := &Inbox{
		isCommand: isCommand,
		now:       time.Now,
		wake:      make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(in)
	}

	if in.isCommand == nil {
		in.isCommand = func(string) bool { return false }
	}

	return in
}

// IsStop reports whether msg is the /stop command from a human.
func IsStop(msg channel.Message) bool {
	return msg.Origin == channel.OriginHuman && strings.TrimSpace(msg.Content) == StopCommand
}

func (in *Inbox) notify() {
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// Admit classifies and admits one message. It never blocks.
func (in *Inbox) Admit(msg channel.Message) Admission {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.stopEnabled && IsStop(msg) {
		return in.admitStopLocked(msg)
	}

	if t := in.turn; t != nil && in.eligibleLocked(msg) {
		t.candidates = append(t.candidates, Candidate{Msg: msg, ReceivedAt: in.now()})
		in.fireLocked(t)

		return Admission{Disposition: Steer, TurnID: t.spec.ID}
	}

	if len(in.deque) >= QueueBound {
		in.dropped++

		return Admission{Disposition: Dropped}
	}

	in.deque = append(in.deque, queued{msg: msg, receivedAt: in.now()})

	// FIFO barrier (S3): any non-harness item queued behind the Turn closes
	// its window for the rest of the Turn. Unspecified and scripted origins
	// are conservative: they close it too.
	if t := in.turn; t != nil && msg.Origin != channel.OriginHarness {
		t.windowOpen = false
	}

	in.notify()

	return Admission{Disposition: Queued}
}

func (in *Inbox) eligibleLocked(msg channel.Message) bool {
	t := in.turn
	if msg.Origin != channel.OriginHuman || !t.spec.Steerable || !t.windowOpen {
		return false
	}

	if in.isCommand(msg.Content) {
		return false
	}

	return t.claimed+len(t.candidates) < t.spec.MaxSteers
}

func (in *Inbox) admitStopLocked(msg channel.Message) Admission {
	t := in.turn
	if t == nil || t.delivering || t.stopping {
		var id string
		if t != nil {
			id = t.spec.ID
		}

		return Admission{Disposition: Stop, TurnID: id}
	}

	t.stopping = true
	t.stopMsgs = append(t.stopMsgs, msg)
	t.windowOpen = false
	dropped := t.candidates
	t.candidates = nil
	t.droppedStop = append(t.droppedStop, dropped...)
	in.fireLocked(t)

	return Admission{Disposition: Stop, TurnID: t.spec.ID, Stopping: true, Dropped: dropped}
}

// fireLocked wakes every waiter of the Turn when steering or a stop is due
// and preemption is still allowed.
func (in *Inbox) fireLocked(t *turnState) {
	if t.sigClosed || !in.mayPreemptLocked(t) {
		return
	}

	close(t.sig)
	t.sigClosed = true
}

func (in *Inbox) mayPreemptLocked(t *turnState) bool {
	if t.delivering {
		return false
	}

	if t.stopping {
		return true
	}

	return len(t.candidates) > 0 && t.restarts < t.spec.MaxRestarts
}

// Next returns the next item for the worker, blocking until one is
// available, ctx ends, or the inbox is closed and drained. A deferred
// candidate always precedes every queued item.
func (in *Inbox) Next(ctx context.Context) (Work, error) {
	for {
		in.mu.Lock()

		if d := in.deferred; d != nil {
			in.deferred = nil
			in.mu.Unlock()

			return *d, nil
		}

		if len(in.deque) > 0 {
			q := in.deque[0]
			in.deque[0] = queued{}
			in.deque = in.deque[1:]
			in.mu.Unlock()

			return Work{Msg: q.msg, ReceivedAt: q.receivedAt}, nil
		}

		closed := in.closed
		in.mu.Unlock()

		if closed {
			return Work{}, ErrClosed
		}

		select {
		case <-in.wake:
		case <-ctx.Done():
			return Work{}, ctx.Err()
		}
	}
}

// Close ends the inbox: Next drains what remains, then returns ErrClosed.
func (in *Inbox) Close() {
	in.mu.Lock()
	in.closed = true
	in.mu.Unlock()
	in.notify()
}

// Discard reports and clears everything still held (shutdown accounting):
// queued items, deferred work and unclaimed candidates.
func (in *Inbox) Discard() (queuedItems, candidates int) {
	in.mu.Lock()
	defer in.mu.Unlock()

	queuedItems = len(in.deque)
	in.deque = nil

	if in.deferred != nil {
		queuedItems++
		candidates += len(in.deferred.Preload)
		in.deferred = nil
	}

	if t := in.turn; t != nil {
		candidates += len(t.candidates)
		t.candidates = nil
	}

	return queuedItems, candidates
}

// Dropped returns the number of queue-saturation drops so far.
func (in *Inbox) Dropped() int {
	in.mu.Lock()
	defer in.mu.Unlock()

	return in.dropped
}

// Len returns the number of queued items (deferred work included).
func (in *Inbox) Len() int {
	in.mu.Lock()
	defer in.mu.Unlock()

	n := len(in.deque)
	if in.deferred != nil {
		n++
	}

	return n
}

// OpenTurn registers the Turn the worker starts and returns its handle. The
// steering window opens when the Turn is steerable and no human item is
// already queued behind it.
func (in *Inbox) OpenTurn(spec TurnSpec) *Turn {
	in.mu.Lock()
	defer in.mu.Unlock()

	t := &turnState{spec: spec, sig: make(chan struct{})}
	t.windowOpen = spec.Steerable && !in.humanQueuedLocked()
	t.candidates = append(t.candidates, spec.Preload...)

	in.turn = t
	in.fireLocked(t)

	return &Turn{in: in, st: t}
}

func (in *Inbox) humanQueuedLocked() bool {
	for _, q := range in.deque {
		if q.msg.Origin != channel.OriginHarness {
			return true
		}
	}

	return false
}

// Turn is the worker/agent-side handle of the Turn in flight.
type Turn struct {
	in *Inbox
	st *turnState
}

// ID returns the Turn ID.
func (t *Turn) ID() string { return t.st.spec.ID }

// PendingCount returns the number of unclaimed candidates.
func (t *Turn) PendingCount() int {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	return len(t.st.candidates)
}

// Stopping reports whether /stop was accepted for this Turn.
func (t *Turn) Stopping() bool {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	return t.st.stopping
}

// StopMessages returns the accepted /stop messages of this Turn, in order:
// the stopped Turn's final reply answers them.
func (t *Turn) StopMessages() []channel.Message {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	return append([]channel.Message(nil), t.st.stopMsgs...)
}

// DroppedByStop returns the candidates a /stop discarded, in order.
func (t *Turn) DroppedByStop() []Candidate {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	return append([]Candidate(nil), t.st.droppedStop...)
}

// Signal returns a channel closed when a candidate is pending or a stop was
// accepted and preemption is still allowed (restart cap, delivery state). A
// receive on it interrupts waits and backoffs without cancelling the Turn
// context. It returns a nil channel (never ready) when no interruption can
// occur, which is safe in a select.
func (t *Turn) Signal() <-chan struct{} {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	if t.st.sigClosed {
		return t.st.sig
	}

	if !t.in.mayPreemptLocked(t.st) && t.st.restarts >= t.st.spec.MaxRestarts {
		return nil
	}

	return t.st.sig
}

// Preempt reports whether the running attempt should be preempted now.
func (t *Turn) Preempt() Reason {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	switch {
	case t.st.delivering:
		return ReasonNone
	case t.st.stopping:
		return ReasonStop
	case len(t.st.candidates) > 0 && t.st.restarts < t.st.spec.MaxRestarts:
		return ReasonSteer
	default:
		return ReasonNone
	}
}

// NoteRestart records one preemption restart of the Turn.
func (t *Turn) NoteRestart() {
	t.in.mu.Lock()
	t.st.restarts++
	t.in.mu.Unlock()
}

// Restarts returns the number of preemption restarts so far.
func (t *Turn) Restarts() int {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	return t.st.restarts
}

// MarkDelivering records that the final answer is being delivered: from now
// on the Turn is neither preemptible nor stoppable, and candidates defer.
func (t *Turn) MarkDelivering() {
	t.in.mu.Lock()
	t.st.delivering = true
	t.in.mu.Unlock()
}

// Claim removes and returns up to limit pending candidates in admission
// order, never exceeding the Turn's per-Turn steer cap. Once claimed they
// are the caller's to inject.
func (t *Turn) Claim(limit int) []Candidate {
	t.in.mu.Lock()
	defer t.in.mu.Unlock()

	st := t.st
	if room := st.spec.MaxSteers - st.claimed; limit > room {
		limit = room
	}

	if limit <= 0 || len(st.candidates) == 0 {
		return nil
	}

	if limit > len(st.candidates) {
		limit = len(st.candidates)
	}

	out := append([]Candidate(nil), st.candidates[:limit]...)
	st.candidates = append([]Candidate(nil), st.candidates[limit:]...)
	st.claimed += limit

	if len(st.candidates) == 0 && !st.stopping && st.sigClosed {
		st.sig = make(chan struct{})
		st.sigClosed = false
	}

	return out
}

// CloseTurn ends the Turn: acceptance stops atomically and every unclaimed
// candidate is deferred — the first becomes the next item (ahead of every
// queued one) and the rest are pre-loaded as candidates of that next Turn.
// It returns the deferred candidates in order.
func (t *Turn) CloseTurn() []Candidate {
	in := t.in
	in.mu.Lock()
	defer in.mu.Unlock()

	st := t.st
	if in.turn == st {
		in.turn = nil
	}

	pending := st.candidates
	st.candidates = nil

	if len(pending) == 0 {
		return nil
	}

	first := pending[0]
	in.deferred = &Work{
		Msg:        first.Msg,
		ReceivedAt: first.ReceivedAt,
		Deferred:   true,
		Preload:    append([]Candidate(nil), pending[1:]...),
	}
	in.notify()

	return pending
}

type ctxKey struct{}

// WithTurn carries the Turn handle in a context so tools and hooks can reach
// the per-Turn signal; probes, aux consumers and Subagent dispatches carry
// none.
func WithTurn(ctx context.Context, t *Turn) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// FromContext returns the Turn handle, or nil.
func FromContext(ctx context.Context) *Turn {
	t, _ := ctx.Value(ctxKey{}).(*Turn)

	return t
}

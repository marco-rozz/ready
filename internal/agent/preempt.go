package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// PreemptReason says why a running attempt was preempted (turn-steering S6).
type PreemptReason string

const (
	// PreemptSteer: a steering candidate is pending.
	PreemptSteer PreemptReason = "steer"
	// PreemptStop: the user stopped the Turn.
	PreemptStop PreemptReason = "stop"
)

// PreemptPoint names where in the ReAct loop the preemption happened.
type PreemptPoint string

const (
	// PointModelCall: an in-flight model call was cancelled before any
	// final-answer content was delivered.
	PointModelCall PreemptPoint = "model_call"
	// PointBeforeTools: the model's tool calls were proposed but none ran.
	PointBeforeTools PreemptPoint = "before_tools"
	// PointAfterTools: a tool-call group completed; the next model call did
	// not start.
	PointAfterTools PreemptPoint = "after_tools"
	// PointFinalUndelivered: a complete final answer was discarded unrecorded
	// and undelivered (raised by the turn-control decorator).
	PointFinalUndelivered PreemptPoint = "final_undelivered"
)

// PreemptError is the turn-control sentinel: an attempt ended because the
// user steered or stopped the Turn, not because anything failed. The
// orchestrator never classifies, retries, advances or demotes on it.
type PreemptError struct {
	Reason PreemptReason
	Point  PreemptPoint
}

// Error implements error.
func (e *PreemptError) Error() string {
	return "turn preempted (" + string(e.Reason) + " at " + string(e.Point) + ")"
}

// IsPreempt reports whether err carries a *PreemptError (through any wrapping).
func IsPreempt(err error) (*PreemptError, bool) {
	var pe *PreemptError
	if errors.As(err, &pe) {
		return pe, true
	}

	return nil, false
}

// PreemptSource is what the agent consults to decide whether to preempt. The
// daemon arms one per Turn; probes, aux consumers and Subagent dispatches
// carry none, so the agent is a pass-through for them.
type PreemptSource interface {
	// PreemptReason reports whether a preemption is due now.
	PreemptReason() (PreemptReason, bool)
	// Signal returns a channel closed when a preemption becomes due (nil =
	// never).
	Signal() <-chan struct{}
}

type preemptSourceKey struct{}

// WithPreemptSource arms steering preemption for the attempts run under ctx.
func WithPreemptSource(ctx context.Context, src PreemptSource) context.Context {
	return context.WithValue(ctx, preemptSourceKey{}, src)
}

// PreemptSourceFrom returns the source armed in ctx, or nil.
func PreemptSourceFrom(ctx context.Context) PreemptSource {
	return preemptSourceFrom(ctx)
}

func preemptSourceFrom(ctx context.Context) PreemptSource {
	src, _ := ctx.Value(preemptSourceKey{}).(PreemptSource)

	return src
}

const (
	gateOpen int32 = iota
	gatePreempted
	gateCommitted
)

type gateKey struct{}

// runGate is the per-attempt preemption state shared by the model wrappers
// (cancelling an in-flight call), the delivery callback (CAS: once the first
// final-answer chunk is forwarded the call is no longer preemptible, and
// once preempted no chunk is forwarded) and the tool-boundary hooks.
type runGate struct {
	src   PreemptSource
	state atomic.Int32

	mu      sync.Mutex
	cancel  context.CancelFunc // current model call, nil when none in flight
	err     *PreemptError      // first preemption recorded
	started chan struct{}      // pulsed when a model call begins
}

func newRunGate(src PreemptSource) *runGate {
	return &runGate{src: src, started: make(chan struct{}, 1)}
}

func gateFrom(ctx context.Context) *runGate {
	g, _ := ctx.Value(gateKey{}).(*runGate)

	return g
}

// commit marks final-answer content as delivered; false means the attempt
// was preempted and no chunk may be forwarded.
func (g *runGate) commit() bool {
	if g.state.CompareAndSwap(gateOpen, gateCommitted) {
		return true
	}

	return g.state.Load() == gateCommitted
}

func (g *runGate) record(reason PreemptReason, point PreemptPoint) *PreemptError {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.err == nil {
		g.err = &PreemptError{Reason: reason, Point: point}
	}

	return g.err
}

// preempted returns the recorded preemption, if any.
func (g *runGate) preempted() *PreemptError {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.err
}

func (g *runGate) beginCall(cancel context.CancelFunc) {
	g.mu.Lock()
	g.cancel = cancel
	g.mu.Unlock()

	select {
	case g.started <- struct{}{}:
	default:
	}
}

func (g *runGate) endCall() {
	g.mu.Lock()
	g.cancel = nil
	g.mu.Unlock()
}

// tryPreemptModelCall cancels the in-flight model call unless final-answer
// content was already delivered. It reports whether the call was preempted.
func (g *runGate) tryPreemptModelCall(reason PreemptReason) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.cancel == nil || !g.state.CompareAndSwap(gateOpen, gatePreempted) {
		return false
	}

	if g.err == nil {
		g.err = &PreemptError{Reason: reason, Point: PointModelCall}
	}

	g.cancel()

	return true
}

// watch preempts an in-flight model call as soon as the source signals. It
// returns when done closes or a call was preempted.
func (g *runGate) watch(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-g.src.Signal():
			if reason, ok := g.src.PreemptReason(); ok && g.tryPreemptModelCall(reason) {
				return
			}

			// No call in flight (a tool runs, or the signal is stale): wait
			// for the next model call to start instead of spinning on a
			// channel that stays closed.
			select {
			case <-done:
				return
			case <-g.started:
			}
		case <-g.started:
		}
	}
}

// hook returns a tool-boundary hook that preempts at point when due.
func (g *runGate) hook(point PreemptPoint) func(context.Context) error {
	return func(context.Context) error {
		reason, ok := g.src.PreemptReason()
		if !ok {
			return nil
		}

		// Never preempt after the attempt committed to delivering content.
		if g.state.Load() == gateCommitted {
			return nil
		}

		return g.record(reason, point)
	}
}

// attemptGuard arms steering preemption for one RunMessagesStreaming call.
type attemptGuard struct {
	gate *runGate
	done chan struct{}
}

// armAttempt returns the context to run under (carrying the gate), the
// per-run ADK options (tool-boundary hooks) and the guard; with no source in
// ctx it is a pass-through and the guard is nil.
func armAttempt(ctx context.Context) (context.Context, []adk.AgentRunOption, *attemptGuard) {
	src := preemptSourceFrom(ctx)
	if src == nil {
		return ctx, nil, nil
	}

	g := newRunGate(src)
	ctx = context.WithValue(ctx, gateKey{}, g)
	opts := []adk.AgentRunOption{
		adk.WithBeforeToolCallsHook(g.hook(PointBeforeTools)),
		adk.WithAfterToolCallsHook(g.hook(PointAfterTools)),
	}

	ag := &attemptGuard{gate: g, done: make(chan struct{})}
	go g.watch(ag.done)

	return ctx, opts, ag
}

// commit is the delivery-side CAS; a nil guard always allows delivery.
func (ag *attemptGuard) commit() bool {
	if ag == nil {
		return true
	}

	return ag.gate.commit()
}

// stop ends the watcher; safe on a nil guard and idempotent.
func (ag *attemptGuard) stop() {
	if ag == nil {
		return
	}

	select {
	case <-ag.done:
	default:
		close(ag.done)
	}
}

// result returns the preemption error to surface instead of lastErr, if the
// attempt was preempted.
func (ag *attemptGuard) result() *PreemptError {
	if ag == nil {
		return nil
	}

	return ag.gate.preempted()
}

// gatedStream wraps a model stream so the call is cancellable by the gate: it
// registers the call's cancel function, forwards every chunk, and clears the
// registration before the stream ends. With no gate in ctx it returns the
// reader untouched.
func gatedStream[T any](ctx context.Context, start func(context.Context) (*schema.StreamReader[T], error)) (*schema.StreamReader[T], error) {
	g := gateFrom(ctx)
	if g == nil {
		return start(ctx)
	}

	callCtx, cancel := context.WithCancel(ctx)
	g.beginCall(cancel)

	reader, err := start(callCtx)
	if err != nil {
		g.endCall()
		cancel()

		return nil, err
	}

	sr, sw := schema.Pipe[T](1)

	go func() {
		defer cancel()
		defer sw.Close()
		defer g.endCall()

		defer reader.Close()

		for {
			chunk, recvErr := reader.Recv()
			if errors.Is(recvErr, io.EOF) {
				return
			}

			if recvErr != nil {
				g.endCall()
				sw.Send(*new(T), recvErr)

				return
			}

			if closed := sw.Send(chunk, nil); closed {
				return
			}
		}
	}()

	return sr, nil
}

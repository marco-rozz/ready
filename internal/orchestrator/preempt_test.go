package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
)

type signalSource struct {
	mu  sync.Mutex
	sig chan struct{}
	set bool
}

func newSignalSource() *signalSource { return &signalSource{sig: make(chan struct{})} }

func (s *signalSource) PreemptReason() (agent.PreemptReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return agent.PreemptSteer, s.set
}

func (s *signalSource) Signal() <-chan struct{} { return s.sig }

func (s *signalSource) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.set {
		s.set = true
		close(s.sig)
	}
}

func preemptStep(point agent.PreemptPoint) runnerStep {
	return runnerStep{err: &agent.PreemptError{Reason: agent.PreemptSteer, Point: point}}
}

func simpleRequest() TurnRequest {
	return TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout, string) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}
}

// Three preemptions leave health, cooldown and the verified flag untouched,
// never retry or advance, and log no switch or demotion.
func TestRunTurn_PreemptionLeavesHealthUntouched(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{preemptStep(agent.PointModelCall), preemptStep(agent.PointBeforeTools), preemptStep(agent.PointAfterTools)}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("must not run")}}}

	buf := &syncBuffer{}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.New(buf))

	before := o.Snapshot()

	for i, want := range []agent.PreemptPoint{agent.PointModelCall, agent.PointBeforeTools, agent.PointAfterTools} {
		_, err := o.RunTurn(context.Background(), simpleRequest(), func(string) {})

		pe, ok := agent.IsPreempt(err)
		if !ok || pe.Point != want {
			t.Fatalf("turn %d: err = %v, want preempt at %s", i, err, want)
		}

		if errors.Is(err, llm.ErrChainExhausted) {
			t.Fatal("a preemption must never be wrapped as chain exhaustion")
		}
	}

	if r0.callCount() != 3 || r1.callCount() != 0 {
		t.Fatalf("calls = %d/%d, want 3/0 (no retry, no advance)", r0.callCount(), r1.callCount())
	}

	after := o.Snapshot()
	if len(after.Candidates) != len(before.Candidates) {
		t.Fatal("candidate set changed")
	}

	for i := range after.Candidates {
		b, a := before.Candidates[i], after.Candidates[i]
		if a.Cooling != b.Cooling || a.Verified != b.Verified || a.Unproven != b.Unproven || a.Active != b.Active || !a.CooldownUntil.Equal(b.CooldownUntil) {
			t.Fatalf("candidate %s health changed by a preemption: %+v -> %+v", a.Ref, b, a)
		}
	}

	if after.State != before.State || after.ActiveRef != before.ActiveRef {
		t.Fatalf("global state changed: %+v -> %+v", before, after)
	}

	logs := strings.ToLower(buf.String())
	for _, bad := range []string{"demot", "switch", "cooldown", "promot"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log mentions %q after preemptions: %s", bad, logs)
		}
	}
}

// A retry backoff ends as soon as a candidate is admitted, without
// cancelling the Turn context, and the next attempt runs.
func TestRunTurn_BackoffInterruptedBySteeringSignal(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{
		{err: errors.New("error 429: too many requests")},
		{outcome: okOutcome("recovered")},
	}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 0,
		Retries:        2,
		RetryBackoff:   8 * time.Second,
	}, zerolog.Nop())

	src := newSignalSource()
	ctx := agent.WithPreemptSource(context.Background(), src)

	go func() {
		time.Sleep(100 * time.Millisecond)
		src.fire()
	}()

	start := time.Now()

	outcome, err := o.RunTurn(ctx, simpleRequest(), func(string) {})
	if err != nil || outcome.Reply != "recovered" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}

	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("backoff not interrupted: took %s (8s backoff)", took)
	}

	if r0.callCount() != 2 {
		t.Fatalf("calls = %d, want 2", r0.callCount())
	}
}

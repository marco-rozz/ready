package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
)

// refRunner records the attempt reference its context carries.
type refRunner struct {
	mu   sync.Mutex
	refs []string
	err  error
}

var _ agent.Runner = (*refRunner)(nil)

func (r *refRunner) RunMessagesStreaming(ctx context.Context, _ string, _ []*schema.Message, _ func(string)) (agent.TurnOutcome, error) {
	ref, _ := llm.AttemptReference(ctx)
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.mu.Unlock()
	if r.err != nil {
		return agent.TurnOutcome{}, r.err
	}

	return okOutcome("ok"), nil
}

// A tool invoked during a failover attempt sees the attempting candidate, not
// the default one.
func TestRunTurn_AttemptReferenceFollowsFailover(t *testing.T) {
	r0 := &refRunner{err: errors.New("error 401: unauthorized")}
	r1 := &refRunner{}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	if _, err := o.RunTurn(context.Background(), TurnRequest{
		SessionKey:  testTurnChannel,
		Assemble:    staticAssemble,
		ResumeReady: func() bool { return true },
	}, func(string) {}); err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if len(r0.refs) != 1 || r0.refs[0] != testRefA0 {
		t.Fatalf("default candidate saw %v, want [%s]", r0.refs, testRefA0)
	}
	if len(r1.refs) != 1 || r1.refs[0] != testRefB1 {
		t.Fatalf("fallback candidate saw %v, want [%s]", r1.refs, testRefB1)
	}
}

func TestAttemptReference_AbsentOutsideAttempt(t *testing.T) {
	if _, ok := llm.AttemptReference(context.Background()); ok {
		t.Fatal("no reference expected outside an attempt")
	}
}

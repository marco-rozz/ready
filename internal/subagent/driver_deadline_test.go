package subagent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/usage"
)

// The dispatch deadline must conclude the owner loop even when the turn
// executor never returns (an executor wedged in a wait without a context
// path): the executor is abandoned, an interrupted handback is delivered,
// and the dispatch goroutine terminates — the owner-end cascade's join can
// never block on it.
func TestDispatchDeadlineInterruptsWedgedExecutor(t *testing.T) {
	wedge := make(chan struct{}) // never closed: the executor never returns

	deps := testDeps()
	deps.Generation = func(_ context.Context) (GenEnv, func()) {
		return GenEnv{
			ChainRefs:  []string{tChainRef},
			OwnerTools: nil,
			ExecuteTurn: func(_ context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
				<-wedge // wedged forever: ignores context cancellation

				return agent.TurnOutcome{Reply: "late"}, nil
			},
			RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
				return map[string]agent.Runner{tChainRef: nil}, nil
			},
		}, func() {}
	}

	rt := New(deps, NewDefinitions([]Definition{{
		Name:        tTypeGP,
		Description: "g",
		Timeout:     50 * time.Millisecond,
	}}))

	owner := testOwner("s1", "cli")

	st, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	select {
	case <-st.dispatchDone(rt):
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not conclude after the deadline; the wedged executor is not abandoned")
	}

	pending := rt.PendingHandbacks(owner)
	if len(pending) != 1 {
		t.Fatalf("pending handbacks = %d, want 1", len(pending))
	}

	if pending[0].Status != HandbackInterrupted {
		t.Fatalf("handback status = %q, want interrupted", pending[0].Status)
	}

	if !strings.Contains(pending[0].Report, "time budget") {
		t.Fatalf("handback report = %q, want the deadline note", pending[0].Report)
	}

	if st.Status != StatusInterrupted {
		t.Fatalf("child status = %q, want interrupted", st.Status)
	}
}

// An executor that DOES react to cancellation is joined normally: its result
// wins the race even when the deadline fires in the same instant.
func TestDispatchDeadlineJoinsResponsiveExecutor(t *testing.T) {
	release := make(chan struct{})
	deps := testDeps()
	deps.Generation = func(_ context.Context) (GenEnv, func()) {
		return GenEnv{
			ChainRefs:  []string{tChainRef},
			OwnerTools: nil,
			ExecuteTurn: func(ctx context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
				select {
				case <-release:
					return agent.TurnOutcome{Reply: "done"}, nil
				case <-ctx.Done():
					return agent.TurnOutcome{}, ctx.Err()
				}
			},
			RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
				return map[string]agent.Runner{tChainRef: nil}, nil
			},
		}, func() {}
	}

	rt := New(deps, NewDefinitions([]Definition{{
		Name:        tTypeGP,
		Description: "g",
		Timeout:     5 * time.Second,
	}}))

	owner := testOwner("s1", "cli")

	st, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	close(release)

	select {
	case <-st.dispatchDone(rt):
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not conclude")
	}

	pending := rt.PendingHandbacks(owner)
	if len(pending) != 1 {
		t.Fatalf("pending handbacks = %d, want 1", len(pending))
	}

	if pending[0].Status != HandbackCompleted {
		t.Fatalf("handback status = %q, want completed", pending[0].Status)
	}
}

// Owner-session end cancels the dispatch: the wedged executor is abandoned,
// the interrupted handback is delivered, and cancelAndJoin's wait (<-done)
// unblocks — the cascade can never hang on a wedged child.
func TestOwnerEndCascadeUnblocksOnWedgedExecutor(t *testing.T) {
	wedge := make(chan struct{})

	deps := testDeps()
	deps.Generation = func(_ context.Context) (GenEnv, func()) {
		return GenEnv{
			ChainRefs:  []string{tChainRef},
			OwnerTools: nil,
			ExecuteTurn: func(_ context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
				<-wedge

				return agent.TurnOutcome{}, nil
			},
			RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
				return map[string]agent.Runner{tChainRef: nil}, nil
			},
		}, func() {}
	}

	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))
	owner := testOwner("s1", "cli")

	if _, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	done := make(chan struct{})
	go func() {
		rt.cancelAndJoin(owner)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelAndJoin blocked on a wedged executor")
	}
}

// Daemon shutdown must conclude even when a dispatch goroutine is wedged in
// an executor that never returns: the bounded join abandons the stragglers
// after the grace instead of holding the process hostage.
func TestShutdownJoinBoundedOnWedgedExecutor(t *testing.T) {
	wedge := make(chan struct{})

	deps := testDeps()
	deps.Generation = func(_ context.Context) (GenEnv, func()) {
		return GenEnv{
			ChainRefs:  []string{tChainRef},
			OwnerTools: nil,
			ExecuteTurn: func(_ context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
				<-wedge

				return agent.TurnOutcome{}, nil
			},
			RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
				return map[string]agent.Runner{tChainRef: nil}, nil
			},
		}, func() {}
	}

	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))
	owner := testOwner("s1", "cli")

	if _, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	done := make(chan struct{})
	go func() {
		rt.Shutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown blocked beyond the join grace on a wedged executor")
	}
}

// A panic anywhere in the dispatch path must be recovered: it is logged
// (with its stack), an interrupted handback reaches the owner, and the
// dispatch goroutine terminates — the fleet is never left wedged.
// Reproduced 2026-11-16: a nil-map panic in recordChildUsage held r.mu and
// the launch defer deadlocked re-acquiring it.
func TestDispatchPanicRecovered(t *testing.T) {
	deps := testDeps()
	deps.Generation = func(_ context.Context) (GenEnv, func()) {
		return GenEnv{
			ChainRefs:  []string{tChainRef},
			OwnerTools: nil,
			ExecuteTurn: func(_ context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
				panic("boom: simulated dispatch panic")
			},
			RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
				return map[string]agent.Runner{tChainRef: nil}, nil
			},
		}, func() {}
	}

	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))
	owner := testOwner("s1", "cli")

	st, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	select {
	case <-st.dispatchDone(rt):
	case <-time.After(5 * time.Second):
		t.Fatal("panicked dispatch never concluded")
	}

	pending := rt.PendingHandbacks(owner)
	if len(pending) != 1 {
		t.Fatalf("pending handbacks = %d, want 1 (the interrupted handback)", len(pending))
	}

	// The executor panic is recovered in the executor goroutine and handed
	// to the handback machinery as a turn error: a panic is a bug, so the
	// outcome is "failed" with the panic text in the report.
	if pending[0].Status != HandbackFailed {
		t.Fatalf("handback status = %q, want failed", pending[0].Status)
	}

	if !strings.Contains(pending[0].Report, "panicked") {
		t.Fatalf("handback report = %q, want the panic error", pending[0].Report)
	}
}

// A fresh runtime (no boot restore) has a usable child baseline: the first
// dispatch's usage delta must be recorded without panicking (the nil-map
// panic that wedged the fleet on 2026-11-16).
func TestFreshRuntimeChildBaselineUsable(t *testing.T) {
	rt := testRuntime()
	if rt.childBaseline == nil {
		t.Fatal("New() left childBaseline nil — the first recordChildUsage would panic")
	}

	rt.mu.Lock()
	rt.childBaseline["c1"] = usage.Totals{}
	rt.mu.Unlock()
}

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/rs/zerolog"
)

func testGeneration(agentTools ...tool.BaseTool) *runtimeGeneration {
	ctx, cancel := context.WithCancel(context.Background())

	return &runtimeGeneration{ctx: ctx, cancel: cancel, agentReg: &agentRegistry{}, agentTools: agentTools}
}

func newTestCoordinator(t *testing.T, rt *sessionRuntime, build func(context.Context) (*runtimeGeneration, error), grace time.Duration) *reloadCoordinator {
	t.Helper()
	first := testGeneration()
	rt.applyGeneration(first)

	return &reloadCoordinator{rt: rt, build: build, grace: grace, logger: zerolog.Nop(), current: first}
}

// A save committed from a worker (Telegram, --exec) schedules an asynchronous
// reload; the worker has released the generation gate by then, so the reload
// never waits on itself, and the next Turn sees a fresh bootstrapper state.
func TestReloadCoordinator_AutoReloadFromWorkerDoesNotDeadlock(t *testing.T) {
	rt := &sessionRuntime{logger: zerolog.Nop()}
	next := testGeneration(tools.NewWaitTool())
	coord := newTestCoordinator(t, rt, func(context.Context) (*runtimeGeneration, error) { return next, nil }, 5*time.Second)
	rt.onAutoReload = coord.RequestAutoReload

	// Existing Session state, pinned skills applied, tool catalog measured.
	st := rt.state("sess-1")
	st.pinnedApplied.Store(true)
	if before := rt.toolCatalogChars(context.Background()); before != 0 {
		t.Fatalf("empty catalog chars = %d", before)
	}

	// The "worker": holds the gate for its item, then triggers the reload as
	// the very last statement, exactly like handleItem.
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.genMu.RLock()
		time.Sleep(20 * time.Millisecond)
		rt.genMu.RUnlock()
		rt.onAutoReload(context.Background(), channel.Message{ChannelName: telegram.ChannelName})
	}()
	<-done

	deadline := time.After(5 * time.Second)
	for coord.currentGen() != next {
		select {
		case <-deadline:
			t.Fatal("asynchronous reload never completed (worker self-deadlock?)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if st.pinnedApplied.Load() {
		t.Fatal("pinnedApplied must be reset so the Session re-creates its skills session")
	}
	if after := rt.toolCatalogChars(context.Background()); after == 0 {
		t.Fatal("toolChars must be recomputed against the new generation's tools")
	}
	rt.genMu.RLock()
	got := rt.agentTools
	rt.genMu.RUnlock()
	if len(got) != 1 {
		t.Fatalf("rt.agentTools = %d tools, want the new generation's 1", len(got))
	}
	coord.Close()
}

// A failed build keeps the previous generation and never touches the runtime.
func TestReloadCoordinator_AutoReloadDoesNotOutliveContext(t *testing.T) {
	rt := &sessionRuntime{logger: zerolog.Nop()}
	builds := 0
	coord := newTestCoordinator(t, rt, func(context.Context) (*runtimeGeneration, error) {
		builds++

		return testGeneration(), nil
	}, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	coord.RequestAutoReload(ctx, channel.Message{ChannelName: telegram.ChannelName})
	coord.wg.Wait()
	if builds != 0 {
		t.Fatalf("builds = %d, want no build after cancellation", builds)
	}
	coord.Close()
}

func TestReloadCoordinator_BuildFailureRetainsPreviousGeneration(t *testing.T) {
	rt := &sessionRuntime{logger: zerolog.Nop()}
	coord := newTestCoordinator(t, rt, func(context.Context) (*runtimeGeneration, error) { return nil, errors.New("boom") }, time.Second)
	prev := coord.currentGen()

	if _, err := coord.Reload(t.Context(), true); err == nil {
		t.Fatal("expected the build failure to be returned")
	}
	if coord.currentGen() != prev {
		t.Fatal("the previous generation must stay active")
	}
	if prev.ctx.Err() != nil {
		t.Fatal("the previous generation must not be cancelled")
	}
}

// In-flight items that do not drain within the grace abort the swap, and the
// abandoned lock request never leaves the gate closed.
func TestReloadCoordinator_DrainTimeoutAbortsAndReopensGate(t *testing.T) {
	rt := &sessionRuntime{logger: zerolog.Nop()}
	built := testGeneration()
	coord := newTestCoordinator(t, rt, func(context.Context) (*runtimeGeneration, error) { return built, nil }, 50*time.Millisecond)
	prev := coord.currentGen()

	rt.genMu.RLock() // a turn that never finishes within the grace
	_, err := coord.Reload(t.Context(), false)
	if !errors.Is(err, errDrainTimeout) {
		t.Fatalf("err = %v, want errDrainTimeout", err)
	}
	if coord.currentGen() != prev || built.ctx.Err() == nil {
		t.Fatal("the new generation must be released and the previous one kept")
	}
	rt.genMu.RUnlock()

	acquired := make(chan struct{})
	go func() { rt.genMu.RLock(); close(acquired); rt.genMu.RUnlock() }()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the generation gate stayed closed after an aborted reload")
	}
}

// Reloads are serialized and coalesced; Close refuses further ones.
func TestReloadCoordinator_SerializedAndClosed(t *testing.T) {
	rt := &sessionRuntime{logger: zerolog.Nop()}
	var mu sync.Mutex
	builds := 0
	coord := newTestCoordinator(t, rt, func(context.Context) (*runtimeGeneration, error) {
		mu.Lock()
		builds++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)

		return testGeneration(), nil
	}, time.Second)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = coord.Reload(context.Background(), true) }()
	}
	wg.Wait()
	if builds != 4 {
		t.Fatalf("builds = %d, want 4 serialized reloads", builds)
	}
	coord.Close()
	if _, err := coord.Reload(context.Background(), true); !errors.Is(err, errReloadClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

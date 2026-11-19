package main

import (
	"sync"
	"testing"
	"time"
)

// TestFleetLimiterTrailingFlushCarriesFinalState is the reproduced
// 2026-11-18 regression (subagent-panel-observability D2): a handback
// arrival (pending 1) followed within the window by its consumption
// (pending 0) emits exactly two snapshots and the second — the trailing
// flush — reads the registry's CURRENT state at flush time, showing zero
// pending. The leading-edge gate dropped the second change forever.
func TestFleetLimiterTrailingFlushCarriesFinalState(t *testing.T) {
	var mu sync.Mutex
	pending := 1 // the handback has arrived, not yet consumed
	var emitted []int
	done := make(chan struct{})

	l := newFleetLimiter(func(_ string) {
		mu.Lock()
		emitted = append(emitted, pending)
		if len(emitted) == 2 {
			close(done)
		}
		mu.Unlock()
	})

	l.notify("owner-a") // immediate: pending 1
	mu.Lock()
	pending = 0 // the handback is consumed inside the window
	mu.Unlock()
	l.notify("owner-a") // inside the window: armed trailing flush

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected exactly two emissions, got %v", emitted)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(emitted) != 2 {
		t.Fatalf("emissions = %v, want exactly two", emitted)
	}
	if emitted[0] != 1 || emitted[1] != 0 {
		t.Fatalf("emissions = %v, want [1 0] — the flush must read the state at flush time", emitted)
	}
}

// TestFleetLimiterRateBudget: under continuous churn the emissions stay at
// least 250 ms apart (the wire budget: at most four events per second).
func TestFleetLimiterRateBudget(t *testing.T) {
	var mu sync.Mutex
	var stamps []time.Time
	var wg sync.WaitGroup

	l := newFleetLimiter(func(_ string) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
	})

	// One immediate emission + churn inside the window: each later event
	// re-arms at most one trailing flush.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			l.notify("owner-a")
			time.Sleep(60 * time.Millisecond)
		}
	}()
	wg.Wait()

	// Wait past the last armed flush.
	time.Sleep(subagentsSnapshotInterval + 150*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(stamps) < 2 {
		t.Fatalf("expected an immediate emission and a trailing flush, got %d", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < subagentsSnapshotInterval-20*time.Millisecond {
			t.Fatalf("emissions %d/%d only %v apart (budget %v)", i-1, i, gap, subagentsSnapshotInterval)
		}
	}
}

// TestFleetLimiterSingleFlush: churn inside one window with no further
// change emits exactly once more (the flush), then nothing.
func TestFleetLimiterSingleFlush(t *testing.T) {
	var mu sync.Mutex
	count := 0

	l := newFleetLimiter(func(_ string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	l.notify("owner-a") // immediate
	for i := 0; i < 5; i++ {
		l.notify("owner-a") // all inside the window: one armed flush
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(subagentsSnapshotInterval + 150*time.Millisecond)

	mu.Lock()
	if count != 2 {
		mu.Unlock()
		t.Fatalf("emissions = %d, want 2 (immediate + one flush)", count)
	}
	mu.Unlock()

	// After the flush the limiter is re-armed from the flush stamp: one
	// notify outside the window emits immediately, a second inside the new
	// window only arms the trailing flush (never emits before it fires).
	time.Sleep(subagentsSnapshotInterval) // clear the flush stamp's window
	l.notify("owner-a")                   // immediate
	mu.Lock()
	if count != 3 {
		mu.Unlock()
		t.Fatalf("emissions = %d, want 3", count)
	}
	mu.Unlock()

	l.notify("owner-a") // inside the new window: arms the flush, no emit
	time.Sleep(subagentsSnapshotInterval + 150*time.Millisecond)
	mu.Lock()
	if count != 4 {
		mu.Unlock()
		t.Fatalf("emissions = %d, want 4 (immediate + trailing flush)", count)
	}
	mu.Unlock()
}

// TestFleetLimiterPerOwnerIsolation: no leak of timers or emissions across
// owners — owner B's changes never emit for owner A and vice versa.
func TestFleetLimiterPerOwnerIsolation(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}

	l := newFleetLimiter(func(owner string) {
		mu.Lock()
		seen[owner]++
		mu.Unlock()
	})

	l.notify("owner-a") // immediate for A
	l.notify("owner-b") // immediate for B
	l.notify("owner-b") // inside B's window: arms B's flush only

	time.Sleep(subagentsSnapshotInterval + 150*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if seen["owner-a"] != 1 {
		t.Fatalf("owner-a emissions = %d, want 1", seen["owner-a"])
	}
	if seen["owner-b"] != 2 {
		t.Fatalf("owner-b emissions = %d, want 2 (immediate + flush)", seen["owner-b"])
	}
}

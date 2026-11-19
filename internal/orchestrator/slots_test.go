package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const slotProv = "prov"

func TestSlotLimiter_UnlimitedWhenAbsent(t *testing.T) {
	l := NewSlotLimiter(nil)

	for i := 0; i < 10; i++ {
		release, err := l.acquire(context.Background(), "any", PriorityOwner)
		if err != nil {
			t.Fatal(err)
		}

		release()
	}
}

func TestSlotLimiter_BoundedAndFIFO(t *testing.T) {
	l := NewSlotLimiter(map[string]int{slotProv: 1})

	var order []int
	var mu sync.Mutex
	var running int32

	start := func(id, priority int) {
		go func() {
			release, err := l.acquire(context.Background(), slotProv, priority)
			if err != nil {
				t.Error(err)

				return
			}
			defer release()

			if n := atomic.AddInt32(&running, 1); n != 1 {
				t.Errorf("concurrency violation: %d holders", n)
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&running, -1)

			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		}()
	}

	start(1, PriorityOwner)
	time.Sleep(5 * time.Millisecond) // 1 holds the slot
	start(2, PriorityOwner)
	start(3, PriorityOwner)

	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("order = %v", order)
	}
	if order[0] != 1 {
		t.Fatalf("first = %d, want 1", order[0])
	}
}

func TestSlotLimiter_OwnerAheadOfSubagentAheadOfJob(t *testing.T) {
	l := NewSlotLimiter(map[string]int{slotProv: 1})

	var order []int
	var mu sync.Mutex
	var running int32

	start := func(id, priority int) {
		go func() {
			release, err := l.acquire(context.Background(), slotProv, priority)
			if err != nil {
				t.Error(err)

				return
			}
			defer release()

			if n := atomic.AddInt32(&running, 1); n != 1 {
				t.Errorf("concurrency violation: %d holders", n)
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&running, -1)

			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		}()
	}

	// Owner holds the slot; then queue subagent, job, owner. The freed slot
	// must be granted by priority: owner > subagent > job.
	start(0, PriorityOwner)
	time.Sleep(10 * time.Millisecond) // holder is inside
	start(3, PriorityJob)
	start(2, PrioritySubagent)
	start(1, PriorityOwner)

	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()

	want := []int{0, 1, 2, 3}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("grant order = %v, want %v", order, want)
		}
	}
}

func TestSlotLimiter_QueueWaitNoFalseDemotion(t *testing.T) {
	// A queued acquirer whose outer ctx stays alive must receive the slot
	// eventually — the liveness budget (armed by attemptRunner AFTER
	// acquire) cannot have fired during the queue wait.
	l := NewSlotLimiter(map[string]int{slotProv: 1})

	release, err := l.acquire(context.Background(), slotProv, PriorityOwner)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	rel, err := l.acquire(ctx, slotProv, PrioritySubagent)
	if err != nil {
		t.Fatalf("queued acquirer failed: %v", err)
	}
	defer rel()

	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Fatalf("acquired too early (%s): queueing did not happen", waited)
	}
}

func TestSlotLimiter_CancelWhileQueued(t *testing.T) {
	l := NewSlotLimiter(map[string]int{slotProv: 1})

	release, err := l.acquire(context.Background(), slotProv, PriorityOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if _, err := l.acquire(ctx, slotProv, PriorityJob); err == nil {
		t.Fatal("expected cancellation error while queued")
	}

	// The limiter still works afterwards.
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()

	go func() {
		time.Sleep(30 * time.Millisecond)
		release()
	}()

	rel, acqErr := l.acquire(ctx2, slotProv, PriorityOwner)
	if acqErr != nil {
		t.Fatalf("limiter wedged after a cancelled waiter: %v", acqErr)
	}

	rel()
}

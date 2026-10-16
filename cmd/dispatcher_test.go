//nolint:goconst
package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
)

// TestDispatcher_SeparateSessionsRunInParallel verifies the core requirement:
// two messages on different session keys must be processed concurrently, while
// a slow handler on one session does not block another.
func TestDispatcher_SeparateSessionsRunInParallel(t *testing.T) {
	in := make(chan channel.Message, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type stamp struct {
		key   string
		start time.Time
		end   time.Time
	}

	var (
		mu      sync.Mutex
		stamps  []stamp
		started = make(chan struct{}, 4)
	)

	d := &dispatcher{
		ctx:      ctx,
		incoming: in,
		keyFn: func(m channel.Message) string {
			if id, ok := m.Metadata["chat_id"]; ok {
				return m.ChannelName + "-" + id
			}

			return m.ChannelName
		},
		workers: make(map[string]chan channel.Message),
		processFn: func(_ context.Context, msg channel.Message) {
			started <- struct{}{}
			start := time.Now()

			// Slow session simulates a 200ms agent call; fast 20ms.
			if msg.Metadata["chat_id"] == "slow" {
				time.Sleep(200 * time.Millisecond)
			} else {
				time.Sleep(20 * time.Millisecond)
			}

			mu.Lock()
			stamps = append(stamps, stamp{
				key:   msg.Metadata["chat_id"],
				start: start,
				end:   time.Now(),
			})
			mu.Unlock()
		},
	}

	runDone := make(chan struct{})
	go func() { d.run(); close(runDone) }()

	t0 := time.Now()
	in <- channel.Message{ChannelName: "telegram", Content: "msg1", Metadata: map[string]string{"chat_id": "slow"}}
	in <- channel.Message{ChannelName: "telegram", Content: "msg2", Metadata: map[string]string{"chat_id": "fast"}}

	// Both workers must have started; if dispatcher were sequential the second
	// start would be deferred until the first finished.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("worker %d did not start in time", i)
		}
	}

	// Close incoming so run() exits after draining.
	close(in)

	// Wait for run() to finish (workers drain).
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not drain in time")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(stamps) != 2 {
		t.Fatalf("expected 2 processed messages, got %d", len(stamps))
	}

	var slowEnd, fastEnd time.Time

	for _, s := range stamps {
		if s.key == "slow" {
			slowEnd = s.end
		} else {
			fastEnd = s.end
		}
	}

	if !fastEnd.Before(slowEnd) {
		t.Errorf("fast session must finish before slow session for true parallelism: fastEnd=%v slowEnd=%v",
			fastEnd.Sub(t0), slowEnd.Sub(t0))
	}
}

// TestDispatcher_SameSessionFIFO verifies that two messages for the same
// session key are processed in order: the second message must not start until
// the first one finishes.
func TestDispatcher_SameSessionFIFO(t *testing.T) {
	in := make(chan channel.Message, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu          sync.Mutex
		order       []string
		active      int
		seenOverlap bool
	)

	d := &dispatcher{
		ctx:      ctx,
		incoming: in,
		keyFn: func(m channel.Message) string {
			return m.ChannelName
		},
		workers: make(map[string]chan channel.Message),
		processFn: func(_ context.Context, msg channel.Message) {
			mu.Lock()
			active++
			if active > 1 {
				seenOverlap = true
			}
			mu.Unlock()

			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			active--
			order = append(order, msg.Content)
			mu.Unlock()
		},
	}

	runDone := make(chan struct{})
	go func() { d.run(); close(runDone) }()

	in <- channel.Message{ChannelName: "cli", Content: "first"}
	in <- channel.Message{ChannelName: "cli", Content: "second"}
	close(in)

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not drain in time")
	}

	mu.Lock()
	defer mu.Unlock()

	if seenOverlap {
		t.Error("same-session messages were processed concurrently — FIFO violated")
	}

	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("expected order [first second], got %v", order)
	}
}

// TestDispatcher_RecoversFromPanic ensures one panicking message does not
// kill the session worker for subsequent messages.
func TestDispatcher_RecoversFromPanic(t *testing.T) {
	in := make(chan channel.Message, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu      sync.Mutex
		handled []string
	)

	d := &dispatcher{
		ctx:      ctx,
		incoming: in,
		keyFn:    func(m channel.Message) string { return m.ChannelName },
		workers:  make(map[string]chan channel.Message),
		processFn: func(_ context.Context, msg channel.Message) {
			if msg.Content == "boom" {
				panic("boom")
			}

			mu.Lock()
			handled = append(handled, msg.Content)
			mu.Unlock()
		},
	}

	runDone := make(chan struct{})
	go func() { d.run(); close(runDone) }()

	in <- channel.Message{ChannelName: "cli", Content: "boom"}
	in <- channel.Message{ChannelName: "cli", Content: "after"}
	close(in)

	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not drain in time")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(handled) != 1 || handled[0] != "after" {
		t.Errorf("expected ['after'] processed after panic recovery, got %v", handled)
	}
}

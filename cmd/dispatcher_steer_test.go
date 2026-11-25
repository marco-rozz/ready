package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/steer"
)

type steerHarness struct {
	d       *dispatcher
	mu      sync.Mutex
	handled []string
	started chan string
	release chan struct{}
}

func newSteerHarness(t *testing.T, gate *sync.RWMutex) *steerHarness {
	t.Helper()

	h := &steerHarness{started: make(chan string, 8), release: make(chan struct{})}

	h.d = &dispatcher{
		ctx:       context.Background(),
		incoming:  make(chan channel.Message),
		keyFn:     func(m channel.Message) string { return m.ChannelName },
		isCommand: isSessionCommandContent,
		logger:    zerolog.Nop(),
		workers:   map[string]*steer.Inbox{},
		handle: func(_ context.Context, in *steer.Inbox, w steer.Work) {
			if gate != nil {
				gate.RLock()
				defer gate.RUnlock()
			}

			turn := in.OpenTurn(steer.TurnSpec{
				ID:          "turn-" + w.Msg.Content,
				Steerable:   w.Msg.Origin == channel.OriginHuman,
				MaxSteers:   8,
				MaxRestarts: 4,
				Preload:     w.Preload,
			})

			h.started <- w.Msg.Content
			if w.Msg.Content == turnWorkText {
				<-h.release
			}

			turn.CloseTurn()

			h.mu.Lock()
			h.handled = append(h.handled, w.Msg.Content)
			h.mu.Unlock()
		},
	}

	return h
}

func (h *steerHarness) waitHandled(t *testing.T, n int) []string {
	t.Helper()

	deadline := time.After(3 * time.Second)

	for {
		h.mu.Lock()
		got := append([]string(nil), h.handled...)
		h.mu.Unlock()

		if len(got) >= n {
			return got
		}

		select {
		case <-deadline:
			t.Fatalf("handled %v, want %d items", got, n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func msgFrom(origin channel.Origin, content string) channel.Message {
	return channel.Message{ChannelName: testCLIChannelName, Content: content, Origin: origin}
}

func TestDispatcher_DeferredCandidateRunsBeforeQueuedHarnessItem(t *testing.T) {
	h := newSteerHarness(t, nil)

	h.d.enqueue(msgFrom(channel.OriginHuman, turnWorkText))
	<-h.started // the Turn is in flight and steerable

	h.d.enqueue(msgFrom(channel.OriginHarness, "alert")) // queued behind the Turn
	h.d.enqueue(msgFrom(channel.OriginHuman, "late"))    // still a candidate: harness does not close the window

	close(h.release)

	got := h.waitHandled(t, 3)
	want := []string{turnWorkText, "late", "alert"}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestDispatcher_ReloadDuringTurnKeepsCandidates(t *testing.T) {
	var gate sync.RWMutex

	h := newSteerHarness(t, &gate)

	h.d.enqueue(msgFrom(channel.OriginHuman, turnWorkText))
	<-h.started

	// A runtime reload takes the write side of the gate; it must wait for the
	// Turn, and a candidate admitted meanwhile must survive the swap.
	reloaded := make(chan struct{})

	go func() {
		gate.Lock()
		close(reloaded)
		gate.Unlock()
	}()

	time.Sleep(20 * time.Millisecond)
	h.d.enqueue(msgFrom(channel.OriginHuman, "during-reload"))
	close(h.release)

	select {
	case <-reloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("reload did not complete after the Turn")
	}

	got := h.waitHandled(t, 2)
	if got[0] != turnWorkText || got[1] != "during-reload" {
		t.Fatalf("handled = %v, want the candidate deferred across the reload", got)
	}
}

func TestDispatcher_UnspecifiedOriginIsAnOrdinaryTurn(t *testing.T) {
	h := newSteerHarness(t, nil)

	h.d.enqueue(msgFrom(channel.OriginHuman, turnWorkText))
	<-h.started
	h.d.enqueue(msgFrom(channel.OriginUnspecified, "plain")) // never injected: queued
	close(h.release)

	got := h.waitHandled(t, 2)
	if got[1] != "plain" {
		t.Fatalf("handled = %v, want the unspecified item processed as the next Turn", got)
	}
}

func TestDispatcher_SaturatedInboxDropsWithoutBlocking(t *testing.T) {
	h := newSteerHarness(t, nil)

	h.d.enqueue(msgFrom(channel.OriginHuman, turnWorkText))
	<-h.started

	done := make(chan struct{})

	go func() {
		for i := 0; i < steer.QueueBound+5; i++ {
			// Scripted items always queue (never candidates).
			h.d.enqueue(msgFrom(channel.OriginScripted, "x"))
		}

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a saturated inbox")
	}

	h.d.mu.Lock()
	dropped := h.d.workers[testCLIChannelName].Dropped()
	h.d.mu.Unlock()

	if dropped != 5 {
		t.Fatalf("dropped = %d, want 5", dropped)
	}

	close(h.release)
}

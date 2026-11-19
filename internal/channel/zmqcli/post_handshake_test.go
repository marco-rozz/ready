package zmqcli

// The post-handshake hook (wire-protocol spec, "Subagent fleet snapshot
// control event"): the attach snapshot is emitted strictly AFTER the
// authenticated ok frame — the client's handshake read consumes exactly one
// frame (the ok reply), so an unsolicited event emitted from the Connection
// start hook could reach it in the ok reply's place.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
)

// The shared integration Session ID (also used by session_events_test.go).
const testAttachSessionID = "0192b7a0-0000-7000-8000-00000000000a"

func TestIntegration_PostHandshakeHookRunsAfterOK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)

	// A Connection start hook decides the effective Session ID, as in
	// production.
	ch.OnConnectionStart(func(_ context.Context, _ channel.Message) channel.ConnectionInfo {
		return channel.ConnectionInfo{SessionID: testAttachSessionID}
	})

	var mu sync.Mutex
	var calls []string
	ch.SetPostHandshakeHook(func(sessionID string) {
		mu.Lock()
		calls = append(calls, sessionID)
		mu.Unlock()
	})

	dealer := dialStatusDealer(ctx, t, addr, "attach-connection")
	doHandshake(t, dealer, "attach-connection", "secret-usage")

	// The ok frame was already received (doHandshake asserts it): the hook
	// must have run by now, carrying the effective Session ID.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("post-handshake hook never ran after the ok frame")
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d (%v), want exactly 1", len(calls), calls)
	}
	if calls[0] != testAttachSessionID {
		t.Fatalf("hook sessionID = %q, want the effective session", calls[0])
	}
}

func TestIntegration_PostHandshakeHookSkipsEmptySession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)

	var called bool
	ch.SetPostHandshakeHook(func(_ string) {
		called = true
	})

	// No OnConnectionStart hook wired: the ok carries no Session ID.
	dealer := dialStatusDealer(ctx, t, addr, "bare-connection")
	doHandshake(t, dealer, "bare-connection", "secret-usage")

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if called {
			t.Fatalf("hook ran with no effective Session ID")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

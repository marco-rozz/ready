package zmqcli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
)

// session-lifecycle 5.6: the Connection start hook runs inside a successful
// session_start; the ok response carries the effective Session ID, and no
// usage seed when a reset was just decided.
func TestIntegration_ConnectionStartHookCarriesSessionID(t *testing.T) {
	for _, pending := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())

		ch, addr := newUsageTestChannel(ctx, t)

		var calls atomic.Int32
		var sawConnection atomic.Value
		ch.OnConnectionStart(func(_ context.Context, msg channel.Message) channel.ConnectionInfo {
			calls.Add(1)
			sawConnection.Store(msg.Metadata[ConnectionID])

			if msg.ChannelName != ChannelName {
				t.Errorf("hook channel = %q", msg.ChannelName)
			}

			return channel.ConnectionInfo{SessionID: "0192b7a0-0000-7000-8000-00000000000a", ResetPending: pending}
		})

		dealer := dialStatusDealer(ctx, t, addr, "hook-connection")
		resp := doHandshake(t, dealer, "hook-connection", "secret-usage")

		if calls.Load() != 1 || sawConnection.Load() != "hook-connection" {
			t.Fatalf("hook calls = %d, connection = %v", calls.Load(), sawConnection.Load())
		}

		if resp.SessionID != "0192b7a0-0000-7000-8000-00000000000a" || resp.ConnectionID != "hook-connection" {
			t.Errorf("handshake = session %q connection %q", resp.SessionID, resp.ConnectionID)
		}

		if pending && resp.Usage != nil {
			t.Errorf("a pending reset must omit the usage seed, got %+v", resp.Usage)
		}

		if !pending && resp.Usage == nil {
			t.Error("without a reset the usage seed is carried")
		}

		cancel()
	}
}

// session_changed / compacted ride unsolicited control responses to the
// active Connection without consuming any reply slot.
func TestIntegration_SessionEventsReachActiveConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)

	// No active Connection: dropped silently.
	if err := ch.SendSessionEvent(protocol.ControlSessionChanged, "s-new", nil); err != nil {
		t.Fatalf("no connection: %v", err)
	}

	dealer := dialStatusDealer(ctx, t, addr, "events-connection")
	doHandshake(t, dealer, "events-connection", "secret-usage")

	if err := ch.SendSessionEvent(protocol.ControlSessionChanged, "s-new", nil); err != nil {
		t.Fatal(err)
	}

	ev, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || ev.ResponseType != protocol.ResponseTypeControl || ev.Content != protocol.ControlSessionChanged ||
		ev.SessionID != "s-new" || ev.Usage != nil || ev.ErrorCode != "" {
		t.Fatalf("session_changed event = %+v (ok=%v)", ev, ok)
	}

	if err := ch.SendSessionEvent(protocol.ControlCompacted, "s-new", &protocol.CompactionInfo{TokensBefore: 142000, TokensAfter: 31000}); err != nil {
		t.Fatal(err)
	}

	ev, ok = recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || ev.Content != protocol.ControlCompacted || ev.Compaction == nil || ev.Compaction.TokensBefore != 142000 || ev.Compaction.TokensAfter != 31000 {
		t.Fatalf("compacted event = %+v (ok=%v)", ev, ok)
	}
}

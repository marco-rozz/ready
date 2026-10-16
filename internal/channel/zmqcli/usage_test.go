package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// testTimestamp is the RFC3339 timestamp used by every test request.
const testTimestamp = "2026-08-05T10:00:00Z"

// usageTotals builds a representative usage source returning fixed totals.
func usageTotals() *protocol.UsageInfo {
	return &protocol.UsageInfo{
		ReadTokens:    68000,
		WrittenTokens: 5400,
		TurnCount:     12,
		LLMCalls:      61,
	}
}

// newUsageTestChannel builds an integration channel with the usage source and
// control handler wired.
func newUsageTestChannel(ctx context.Context, t *testing.T) (*Channel, string) {
	t.Helper()

	ch, addr := newIntegrationChannel(ctx, t, "secret-usage", 0, zerolog.Nop())
	ch.SetUsageSource(usageTotals)

	return ch, addr
}

// TestIntegration_HandshakeSeedsUsage pins the "Reconnecting TUI restores the
// footer" scenario: the authenticated handshake response carries the live
// session totals from the wired usage source.
func TestIntegration_HandshakeSeedsUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)
	_ = ch

	dealer := dialStatusDealer(ctx, t, addr, "usage-handshake-session")
	resp := doHandshake(t, dealer, "usage-handshake-session", "secret-usage")

	if resp.Usage == nil {
		t.Fatal("handshake must carry the usage seed")
	}
	if *resp.Usage != *usageTotals() {
		t.Errorf("usage seed = %+v, want %+v", resp.Usage, usageTotals())
	}
}

// TestIntegration_HandshakeWithoutUsageOmitsField pins that a fresh session
// (nil totals) yields a pre-usage handshake payload.
func TestIntegration_HandshakeWithoutUsageOmitsField(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)
	ch.SetUsageSource(func() *protocol.UsageInfo { return nil }) // fresh: no totals

	dealer := dialStatusDealer(ctx, t, addr, "fresh-usage-session")
	resp := doHandshake(t, dealer, "fresh-usage-session", "secret-usage")

	if resp.Usage != nil {
		t.Errorf("fresh session handshake must omit usage, got %+v", resp.Usage)
	}
}

// TestIntegration_FinalReplyCarriesUsage verifies the final reply produced by
// Send carries the usage block, and progress notifications carry the live
// totals so the TUI footer's LLM-call counter advances mid-turn (the pair
// diverges exactly when ReAct iterations ran). A fresh session with no totals
// omits the field everywhere.
func TestIntegration_FinalReplyCarriesUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)

	// Reader: push one progress notification, then the final reply.
	go func() {
		msg, err := ch.Receive(ctx)
		if err != nil {
			return
		}

		sessionID := msg.Metadata[ConnectionID]
		_ = ch.SendNotification(ctx, sessionID, "partial") // progress: live usage
		_ = ch.Send(ctx, channel.Message{                  // final reply: usage attached
			Content:  "done",
			Metadata: map[string]string{ConnectionID: sessionID},
		})
	}()

	dealer := dialStatusDealer(ctx, t, addr, "usage-reply-session")
	doHandshake(t, dealer, "usage-reply-session", "secret-usage")

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "usage-reply-session",
		Type:         protocol.RequestTypeMessage,
		RequestID:    "req-usage-1",
		Timestamp:    testTimestamp,
	})

	progress, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for progress frame")
	}
	if progress.ResponseType != protocol.ResponseTypeProgress {
		t.Fatalf("expected progress frame first, got %q", progress.ResponseType)
	}
	if progress.Usage == nil {
		t.Fatal("progress frame must carry the live usage totals")
	}
	if *progress.Usage != *usageTotals() {
		t.Errorf("progress usage = %+v, want %+v", progress.Usage, usageTotals())
	}

	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for final reply")
	}
	if reply.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("expected reply frame, got %q", reply.ResponseType)
	}
	if reply.Usage == nil {
		t.Fatal("final reply must carry usage totals")
	}
	if *reply.Usage != *usageTotals() {
		t.Errorf("reply usage = %+v, want %+v", reply.Usage, usageTotals())
	}
}

// TestIntegration_ProgressWithoutUsageOmitsField pins that a fresh session
// (nil usage source) keeps progress payloads pre-usage: field omitted, never
// rendered as zero.
func TestIntegration_ProgressWithoutUsageOmitsField(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)
	ch.SetUsageSource(func() *protocol.UsageInfo { return nil }) // fresh: no totals

	go func() {
		msg, err := ch.Receive(ctx)
		if err != nil {
			return
		}
		_ = ch.SendNotification(ctx, msg.Metadata[ConnectionID], "partial")
	}()

	dealer := dialStatusDealer(ctx, t, addr, "fresh-progress-session")
	doHandshake(t, dealer, "fresh-progress-session", "secret-usage")

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "fresh-progress-session",
		Type:         protocol.RequestTypeMessage,
		RequestID:    "req-usage-2",
		Timestamp:    testTimestamp,
	})

	progress, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for progress frame")
	}
	if progress.ResponseType != protocol.ResponseTypeProgress {
		t.Fatalf("expected progress frame, got %q", progress.ResponseType)
	}
	if progress.Usage != nil {
		t.Errorf("fresh-session progress must omit usage, got %+v", progress.Usage)
	}
}

// TestIntegration_ControlRequestSessionInfo pins the zmq-transport scenario
// "session_info answers from the live session": an authenticated
// session_info request is answered with a reply carrying the handler's
// markdown content, without reaching the incoming queue (no agent, no
// history mutation).
func TestIntegration_ControlRequestSessionInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)

	called := false
	ch.SetControlRequestHandler(func(reqType protocol.RequestType, sessionID string) string {
		called = true
		if reqType != protocol.RequestTypeSessionInfo {
			t.Errorf("handler type = %q, want session_info", reqType)
		}
		if sessionID != "control-info-session" {
			t.Errorf("handler sessionID = %q, want control-info-session", sessionID)
		}

		return "## Session\n\n**Session ID:** `control-info-session`"
	})

	dealer := dialStatusDealer(ctx, t, addr, "control-info-session")
	doHandshake(t, dealer, "control-info-session", "secret-usage")

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "control-info-session",
		Type:         protocol.RequestTypeSessionInfo,
		RequestID:    "req-info-1",
		Timestamp:    testTimestamp,
	})

	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for control reply")
	}

	if !called {
		t.Fatal("control handler was not invoked")
	}
	if reply.Status != protocol.ResponseStatusOK || reply.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("reply status/response_type = %q/%q, want ok/reply", reply.Status, reply.ResponseType)
	}
	if reply.Usage == nil {
		t.Error("control reply should carry the usage block like any final reply")
	}

	// The control request never reached the agent queue.
	select {
	case msg := <-ch.incoming:
		t.Fatalf("control request must not reach the agent queue, got %+v", msg)
	default:
	}
}

// TestIntegration_ControlRequestWithoutSessionRejected pins the
// "unauthenticated request follows existing handling" requirement: a
// session_info from an unknown session gets a no_active_session error and
// the handler is never called.
func TestIntegration_ControlRequestWithoutSessionRejected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newUsageTestChannel(ctx, t)
	ch.SetControlRequestHandler(func(protocol.RequestType, string) string {
		t.Error("handler must not be called without an active session")

		return ""
	})

	dealer := dialStatusDealer(ctx, t, addr, "orphan-control-session")

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "orphan-control-session",
		Type:         protocol.RequestTypeSessionInfo,
		RequestID:    "req-orphan-1",
		Timestamp:    testTimestamp,
	})

	resp, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for rejection reply")
	}

	if resp.Status != protocol.ResponseStatusError || resp.ErrorCode != "no_active_session" {
		t.Fatalf("status/error_code = %q/%q, want error/no_active_session", resp.Status, resp.ErrorCode)
	}
}

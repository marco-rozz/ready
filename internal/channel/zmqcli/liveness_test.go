package zmqcli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// testBindAddr mirrors the ephemeral bind used by the integration harness.
const testBindAddr = "tcp://127.0.0.1:0"

// testErrNoActiveSession keeps the goconst literal count down.
const testErrNoActiveSession = "no_active_session"

// newLivenessChannel builds a channel with a comfortable session_timeout so
// only the liveness watchdog can fire, plus a log buffer for assertions.
func newLivenessChannel(ctx context.Context, t *testing.T, liveness time.Duration) (*Channel, string, *lockedBuf) {
	t.Helper()

	buf := &lockedBuf{}
	logger := zerolog.New(buf).Level(zerolog.TraceLevel)

	cfg := config.ZMQCLIChannelConfig{
		Bind:                  testBindAddr,
		AuthToken:             "secret-liveness",
		ConnectionIdleTimeout: 15 * time.Minute,
		LivenessTimeout:       liveness,
	}

	ch, err := NewChannel(ctx, cfg, "secret-liveness", false, logger)
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}

	t.Cleanup(func() { _ = ch.Close() })

	return ch, ch.listener.Addr(), buf
}

// sessionIsActive reports whether a message from sessionID is still accepted
// (no no_active_session error), i.e. the lock has not been released.
func sessionIsActive(t *testing.T, req zmq4.Socket, sessionID string) bool {
	t.Helper()

	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypePing,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	return resp.Status == protocol.ResponseStatusOK
}

// ----------------------------------------------------------------------------
// Task 1.3: session_end releases the lock regardless of identity
// ----------------------------------------------------------------------------

// TestSessionEnd_FromDifferentIdentityReleases verifies the zmq-session-
// liveness D1 semantics: the client's cleanup (chatSendSessionEnd) arrives on
// a fresh DEALER whose identity differs from the session's — the lock must be
// released anyway.
func TestSessionEnd_FromDifferentIdentityReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newLivenessChannel(ctx, t, 45*time.Second)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	reqA := dialNewREQ(reqCtx, t, addr)
	defer reqA.Close()

	sessionID := doFullHandshake(t, reqA, "secret-liveness")

	// A second, independent socket: different connection, different identity.
	reqB := dialNewREQ(reqCtx, t, addr)
	defer reqB.Close()

	resp := sendAndRecv(t, reqB, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionEnd,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for session_end, got %q", resp.Status)
	}

	// The lock must now be free: a brand-new handshake succeeds instead of
	// returning busy.
	reqC := dialNewREQ(reqCtx, t, addr)
	defer reqC.Close()

	okResp := doHandshake(t, reqC, "next-session", "secret-liveness")
	if okResp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for a new session after session_end, got %q", okResp.Status)
	}

	_ = ch
}

// TestSessionEnd_ForeignSessionIDDoesNotRelease verifies that a session_end
// carrying someone else's session id is answered ok (so the cleanup Recv
// never blocks) but does NOT release the active lock.
func TestSessionEnd_ForeignSessionIDDoesNotRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newLivenessChannel(ctx, t, 45*time.Second)
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	reqA := dialNewREQ(reqCtx, t, addr)
	defer reqA.Close()

	sessionID := doFullHandshake(t, reqA, "secret-liveness")

	reqB := dialNewREQ(reqCtx, t, addr)
	defer reqB.Close()

	resp := sendAndRecv(t, reqB, protocol.Request{
		ConnectionID: "not-the-active-session",
		Type:         protocol.RequestTypeSessionEnd,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for foreign session_end reply, got %q", resp.Status)
	}

	// The original session must still hold the lock.
	if !sessionIsActive(t, reqA, sessionID) {
		t.Fatal("active session was released by a foreign session_end")
	}
}

// ----------------------------------------------------------------------------
// Task 2.3 + 3.4: ping handler and liveness watchdog
// ----------------------------------------------------------------------------

// TestPing_UpdatesHeartbeatAndReplies verifies the ping handler: ok reply for
// the active session, heartbeat recorded, no pending/history interaction.
func TestPing_UpdatesHeartbeatAndReplies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newLivenessChannel(ctx, t, 45*time.Second)
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, "secret-liveness")

	if !ch.sess.lastPingTime().IsZero() {
		t.Fatal("lastPing must be zero before the first ping (liveness exemption)")
	}

	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypePing,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for ping, got %q (%s)", resp.Status, resp.ErrorCode)
	}

	if ch.sess.lastPingTime().IsZero() {
		t.Fatal("ping did not update the heartbeat timestamp")
	}

	// A ping from a foreign session id is an error, and must not touch the
	// active session's heartbeat.
	reqB := dialNewREQ(reqCtx, t, addr)
	defer reqB.Close()

	errResp := sendAndRecv(t, reqB, protocol.Request{
		ConnectionID: "foreign-session",
		Type:         protocol.RequestTypePing,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if errResp.ErrorCode != testErrNoActiveSession {
		t.Fatalf("expected no_active_session for foreign ping, got %q", errResp.ErrorCode)
	}
}

// TestLiveness_PingsCeeded_ReleasesSession verifies the core liveness
// scenario: a heartbeating client stops pinging (killed process, closed
// terminal) — the watchdog releases the lock with reason client_unresponsive.
func TestLiveness_PingsCeeded_ReleasesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, logBuf := newLivenessChannel(ctx, t, 200*time.Millisecond)
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, "secret-liveness")

	// Client heartbeats once, then dies.
	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypePing,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for ping, got %q", resp.Status)
	}

	// Backdate the heartbeat past the liveness window (in-package test: the
	// watchdog timestamps are time.Now-based).
	ch.sess.mu.Lock()
	ch.sess.lastPing = time.Now().Add(-1 * time.Second)
	ch.sess.mu.Unlock()

	time.Sleep(250 * time.Millisecond)
	ch.checkIdleTimeout()

	if sessionIsActive(t, req, sessionID) {
		t.Fatal("session still active after pings ceased past the liveness window")
	}

	time.Sleep(10 * time.Millisecond) // let log line flush
	if log := logBuf.String(); !strings.Contains(log, "client_unresponsive") {
		t.Fatalf("expected session_closed reason=client_unresponsive in logs:\n%s", log)
	}
}

// TestLiveness_RecentPing_KeepsSession verifies a live client: a fresh
// heartbeat keeps the session through watchdog checks.
func TestLiveness_RecentPing_KeepsSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newLivenessChannel(ctx, t, 45*time.Second)
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, "secret-liveness")

	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypePing,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok for ping, got %q", resp.Status)
	}

	ch.checkIdleTimeout()

	if !sessionIsActive(t, req, sessionID) {
		t.Fatal("session released despite a fresh heartbeat")
	}
}

// TestLiveness_NeverPinged_Exempt verifies the backward-compatibility
// exemption: a client that never pinged (--exec, older clients) is never
// released by the liveness watchdog — only session_timeout may reap it.
func TestLiveness_NeverPinged_Exempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newLivenessChannel(ctx, t, 1*time.Millisecond)
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, "secret-liveness")

	// Liveness window is already expired — but the client never pinged.
	time.Sleep(5 * time.Millisecond)
	ch.checkIdleTimeout()

	if !sessionIsActive(t, req, sessionID) {
		t.Fatal("liveness watchdog fired for a session that never pinged")
	}
}

// ----------------------------------------------------------------------------
// Task 4.3: handshake carries liveness_timeout
// ----------------------------------------------------------------------------

// TestHandshake_CarriesLivenessTimeout verifies the authenticated handshake
// response negotiates the heartbeat window.
func TestHandshake_CarriesLivenessTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, addr, _ := newLivenessChannel(ctx, t, 90*time.Second)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	okResp := doHandshake(t, req, "liveness-meta-session", "secret-liveness")

	if okResp.LivenessTimeout != "1m30s" {
		t.Fatalf("expected liveness_timeout 1m30s in handshake, got %q", okResp.LivenessTimeout)
	}
}

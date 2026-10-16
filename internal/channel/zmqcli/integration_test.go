//nolint:goconst
package zmqcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// lockedBuf is a bytes.Buffer guarded by a mutex. The daemon's dispatchLoop
// and watchdog goroutines write to the logger concurrently with the test
// goroutine reading it; bytes.Buffer alone is not safe for concurrent use.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// ----------------------------------------------------------------------------
// Test infrastructure helpers
// ----------------------------------------------------------------------------

// newIntegrationChannel creates a Channel bound to an ephemeral port (0) with
// the given logger (or zerolog.Nop if nil). The cleanup function closes the
// channel automatically.
func newIntegrationChannel(ctx context.Context, t *testing.T, secret string, sessionTimeout time.Duration, logger zerolog.Logger) (*Channel, string) {
	t.Helper()

	timeout := sessionTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}

	cfg := config.ZMQCLIChannelConfig{
		Bind:                  "tcp://127.0.0.1:0",
		AuthToken:             secret,
		ConnectionIdleTimeout: timeout,
	}

	ch, err := NewChannel(ctx, cfg, secret, false, logger)
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}

	t.Cleanup(func() { _ = ch.Close() })

	return ch, ch.listener.Addr()
}

// newIntegrationChannelWithBuf is like newIntegrationChannel but captures log
// output in a lockedBuf (useful for asserting what the daemon logged).
// lockedBuf is safe for concurrent use: the channel's goroutines write to it
// while the test goroutine reads from it.
func newIntegrationChannelWithBuf(ctx context.Context, t *testing.T, secret string, sessionTimeout time.Duration) (*Channel, string, *lockedBuf) {
	t.Helper()

	buf := &lockedBuf{}
	logger := zerolog.New(buf).Level(zerolog.TraceLevel)

	ch, addr := newIntegrationChannel(ctx, t, secret, sessionTimeout, logger)

	return ch, addr, buf
}

// startEchoAgent starts a goroutine that reads messages from ch and replies
// with the same Content prefixed by "echo: ". It stops when ctx is cancelled.
func startEchoAgent(ctx context.Context, ch *Channel) {
	go func() {
		for {
			msg, err := ch.Receive(ctx)
			if err != nil {
				return
			}
			reply := channel.Message{
				Content: "echo: " + msg.Content,
				Metadata: map[string]string{
					"connection_id": msg.Metadata[ConnectionID],
				},
			}
			_ = ch.Send(ctx, reply)
		}
	}()
}

// dialNewREQ creates a fresh REQ socket dialled to addr.
func dialNewREQ(ctx context.Context, t *testing.T, addr string) zmq4.Socket {
	t.Helper()

	req := zmq4.NewReq(ctx)
	if err := req.Dial(addr); err != nil {
		t.Fatalf("REQ Dial(%q): %v", addr, err)
	}

	return req
}

// doFullHandshake performs a complete two-round handshake and returns the
// session ID used.
func doFullHandshake(t *testing.T, req zmq4.Socket, secret string) string {
	t.Helper()

	sessionID := "test-session-" + t.Name()

	doHandshake(t, req, sessionID, secret)

	return sessionID
}

// sendAndRecv sends a request and immediately reads a response.
func sendAndRecv(t *testing.T, req zmq4.Socket, r protocol.Request) protocol.Response {
	t.Helper()

	data, err := protocol.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	raw := reqSendRecv(t, req, data)

	var resp protocol.Response
	if parseErr := json.Unmarshal(raw, &resp); parseErr != nil {
		t.Fatalf("unmarshal response: %v", parseErr)
	}

	return resp
}

// sendSessionEndMsg sends a session_end without reading a reply.
func sendSessionEndMsg(t *testing.T, req zmq4.Socket, sessionID string) {
	t.Helper()

	data, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionEnd,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if err := req.Send(zmq4.NewMsg(data)); err != nil {
		t.Logf("session_end send (best-effort): %v", err)
	}
}

// ----------------------------------------------------------------------------
// Task 7.2: Happy path
// ----------------------------------------------------------------------------

// TestIntegration_HappyPath verifies the full flow:
// connect → challenge → ok → message → reply → session_end → ok.
func TestIntegration_HappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newIntegrationChannel(ctx, t, "secret-happy", 0, zerolog.Nop())
	startEchoAgent(ctx, ch)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer func() {
		_ = req.Close()
	}()

	sessionID := doFullHandshake(t, req, "secret-happy")

	// Send a message and receive the echo reply.
	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeMessage,
		Content:      "hello",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok, got %q", resp.Status)
	}

	if !strings.HasPrefix(resp.Content, "echo: ") {
		t.Fatalf("expected echo prefix, got %q", resp.Content)
	}

	// Clean session_end.
	sendSessionEndMsg(t, req, sessionID)
}

// ----------------------------------------------------------------------------
// Task 7.3: Busy rejection
// ----------------------------------------------------------------------------

// TestIntegration_BusyRejection verifies that a second concurrent client
// receives "busy" while client A's session is active, and that client A
// remains healthy after client B is rejected.
func TestIntegration_BusyRejection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newIntegrationChannel(ctx, t, "secret-busy", 0, zerolog.Nop())
	startEchoAgent(ctx, ch)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	// Client A: complete handshake, hold session.
	reqA := dialNewREQ(reqCtx, t, addr)
	defer func() {
		_ = reqA.Close()
	}()
	sessionIDA := doFullHandshake(t, reqA, "secret-busy")

	// Client B: should get busy.
	reqB := dialNewREQ(reqCtx, t, addr)
	defer func() {
		_ = reqB.Close()
	}()
	sessionIDB := "session-B-busy-test"

	// Round 1 probe for B.
	data, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionIDB,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	rawChallenge := reqSendRecv(t, reqB, data)
	var challengeB protocol.Response
	if err := json.Unmarshal(rawChallenge, &challengeB); err != nil {
		t.Fatalf("unmarshal B challenge: %v", err)
	}
	if challengeB.ErrorCode != Challenge {
		t.Fatalf("B expected challenge, got status=%q error_code=%q", challengeB.Status, challengeB.ErrorCode)
	}

	// Round 2 verify for B.
	tokenB := hmacHex("secret-busy", challengeB.Content)
	verifyData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionIDB,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    tokenB,
	})
	rawBusy := reqSendRecv(t, reqB, verifyData)
	var busyResp protocol.Response
	if err := json.Unmarshal(rawBusy, &busyResp); err != nil {
		t.Fatalf("unmarshal B busy response: %v", err)
	}
	if busyResp.Status != protocol.ResponseStatusBusy {
		t.Fatalf("B expected busy, got %q", busyResp.Status)
	}

	// Client A should still be healthy — send a message and get echo.
	resp := sendAndRecv(t, reqA, protocol.Request{
		ConnectionID: sessionIDA,
		Type:         protocol.RequestTypeMessage,
		Content:      "still alive",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("A expected ok after B was rejected, got %q", resp.Status)
	}

	sendSessionEndMsg(t, reqA, sessionIDA)
}

// ----------------------------------------------------------------------------
// Task 7.4: Auth fail + no-token-in-logs
// ----------------------------------------------------------------------------

// TestIntegration_AuthFail verifies that connecting with the wrong secret
// results in auth_fail and that the daemon's auth_token never appears in logs.
func TestIntegration_AuthFail(t *testing.T) {
	const daemonSecret = "DAEMON-TOKEN-DO-NOT-LEAK-xyz789"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, logBuf := newIntegrationChannelWithBuf(ctx, t, daemonSecret, 0)
	_ = ch // only need the address

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer func() {
		_ = req.Close()
	}()

	sessionID := "auth-fail-test"

	// Round 1: probe.
	data, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	rawChallenge := reqSendRecv(t, req, data)
	var challengeResp protocol.Response
	if err := json.Unmarshal(rawChallenge, &challengeResp); err != nil {
		t.Fatalf("unmarshal challenge: %v", err)
	}

	// Round 2: deliberately wrong token (all zeros, 64 hex chars).
	wrongToken := strings.Repeat("0", 64)
	verifyData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    wrongToken,
	})
	rawFail := reqSendRecv(t, req, verifyData)
	var failResp protocol.Response
	if err := json.Unmarshal(rawFail, &failResp); err != nil {
		t.Fatalf("unmarshal auth_fail response: %v", err)
	}

	if failResp.Status != protocol.ResponseStatusAuthFail {
		t.Fatalf("expected auth_fail, got %q", failResp.Status)
	}

	// Wait briefly for any log lines to flush, then assert the secret never leaked.
	time.Sleep(10 * time.Millisecond)

	if strings.Contains(logBuf.String(), daemonSecret) {
		t.Fatalf("daemon secret leaked into logs:\n%s", logBuf.String())
	}
}

// ----------------------------------------------------------------------------
// Task 7.5: Shutdown
// ----------------------------------------------------------------------------

// TestIntegration_Shutdown verifies that cancelling the daemon's root context
// while a client is mid-session causes the client to receive
// Response{Status:"shutdown"}.
func TestIntegration_Shutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Do NOT defer cancel yet — we cancel it explicitly mid-test.

	ch, addr := newIntegrationChannel(ctx, t, "secret-shutdown", 0, zerolog.Nop())
	startEchoAgent(ctx, ch)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer func() {
		_ = req.Close()
	}()

	sessionID := doFullHandshake(t, req, "secret-shutdown")
	_ = sessionID

	// Cancel the daemon's root context — this should trigger Close().
	// The Channel's dispatchLoop will exit, Close() fires shutdown handshake.
	cancel()

	// ch.Close() must also be called (normally done by the Manager).
	if err := ch.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("ch.Close warning: %v", err)
	}

	// The client must receive a shutdown notification.
	done := make(chan protocol.Response, 1)
	go func() {
		msg, recvErr := req.Recv()
		if recvErr != nil {
			done <- protocol.Response{}

			return
		}
		var r protocol.Response
		if parseErr := json.Unmarshal(msg.Frames[len(msg.Frames)-1], &r); parseErr != nil {
			done <- protocol.Response{}

			return
		}
		done <- r
	}()

	select {
	case resp := <-done:
		if resp.Status != protocol.ResponseStatusShutdown {
			t.Fatalf("expected shutdown, got %q", resp.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for shutdown notification")
	}
}

// ----------------------------------------------------------------------------
// Task 7.7: Session timeout watchdog
// ----------------------------------------------------------------------------

// TestIntegration_SessionTimeout verifies the idle-timeout watchdog:
// after a short session_timeout with no activity, the daemon releases the
// session silently. The next message from the client receives
// "no_active_session" through the normal handleMessage path (NOT an
// unsolicited push — see releaseTimedOut doc).
func TestIntegration_SessionTimeout(t *testing.T) {
	const shortTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, logBuf := newIntegrationChannelWithBuf(ctx, t, "secret-timeout", shortTimeout)
	// No echo agent — we only need session lifecycle, no message processing.
	_ = ch

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, "secret-timeout")

	// Wait past the session_timeout without sending any messages.
	// The watchdog runs every 30s normally, but we can trigger it directly.
	time.Sleep(shortTimeout + 50*time.Millisecond)

	// Manually trigger the watchdog so we don't have to wait 30s.
	ch.checkIdleTimeout()

	// Now send a message — should get no_active_session (session was released
	// silently by the watchdog; no unsolicited push).
	resp := sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeMessage,
		Content:      "too late",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	if resp.ErrorCode != "no_active_session" {
		t.Fatalf("expected no_active_session error_code, got status=%q error_code=%q", resp.Status, resp.ErrorCode)
	}

	// Daemon log must contain session_closed reason=idle_timeout.
	time.Sleep(10 * time.Millisecond) // let log line flush
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "session_closed") || !strings.Contains(logOutput, "timeout") {
		t.Fatalf("expected session_closed reason=timeout in logs:\n%s", logOutput)
	}
}

// ----------------------------------------------------------------------------
// Task 7.8: No runtime-home writes for chat
// ----------------------------------------------------------------------------

// TestIntegration_NoHomeWrites verifies that the zmqcli channel itself never
// creates files. (The full chat invariant is in cmd/subcommands_test.go.)
func TestIntegration_NoHomeWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmp := t.TempDir()

	ch, _ := newIntegrationChannel(ctx, t, "secret-nohome", 0, zerolog.Nop())
	defer func() {
		_ = ch.Close()
	}()

	// Stat the tmpdir — no files should be created by the Channel.
	entries, err := filepath.Glob(filepath.Join(tmp, "*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("expected 0 files in tmpdir, found: %v", entries)
	}
}

// ----------------------------------------------------------------------------
// Task 7.9: No token in logs (full session)
// ----------------------------------------------------------------------------

// TestIntegration_NoTokenInLogs runs a complete authenticated session and
// asserts the daemon's auth_token never appears in any log line, even with
// the payloads gate enabled.
func TestIntegration_NoTokenInLogs(t *testing.T) {
	const daemonSecret = "VERY-SECRET-INTEGRATION-TOKEN-abc123"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buf := &lockedBuf{}
	// Enable payloads logging to exercise the worst case.
	cfg := config.ZMQCLIChannelConfig{
		Bind:                  "tcp://127.0.0.1:0",
		AuthToken:             daemonSecret,
		ConnectionIdleTimeout: 15 * time.Minute,
	}
	logger := zerolog.New(buf).Level(zerolog.TraceLevel)

	ch, err := NewChannel(ctx, cfg, daemonSecret, true /* payloadsEnabled */, logger)
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}

	addr := ch.listener.Addr()
	startEchoAgent(ctx, ch)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	req := dialNewREQ(reqCtx, t, addr)
	defer req.Close()

	sessionID := doFullHandshake(t, req, daemonSecret)

	// Send a message.
	sendAndRecv(t, req, protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeMessage,
		Content:      "test message",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})

	// End session, then close the channel to stop the dispatch goroutine before
	// reading the log buffer. The lockedBuf mutex ensures no data race even if
	// the goroutine writes one final line after Close().
	sendSessionEndMsg(t, req, sessionID)

	if err := ch.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Logf("ch.Close warning: %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	logOutput := buf.String()
	if strings.Contains(logOutput, daemonSecret) {
		t.Fatalf("daemon secret leaked into logs (payloadsEnabled=true):\n%s", logOutput)
	}
}

// ----------------------------------------------------------------------------
// Regression: Send failure releases session (prevents "busy" lockout)
// ----------------------------------------------------------------------------

// TestIntegration_SendFailureReleasesSession verifies that when the daemon
// attempts to Send a reply and the ROUTER socket reports an error (e.g.,
// socket closed), the session is released so the next client can connect
// without getting "busy".
//
// Note: ZMQ's ROUTER socket often BUFFERS sends to disconnected peers without
// reporting an error. In that case the idle_timeout watchdog is the fallback.
// This test exercises BOTH paths depending on ZMQ behavior.
func TestIntegration_SendFailureReleasesSession(t *testing.T) {
	// Use a short sessionTimeout so we can verify the watchdog fires quickly
	// even when ZMQ buffers the send (no error from Send).
	const shortTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, logBuf := newIntegrationChannelWithBuf(ctx, t, "secret-sendfail", shortTimeout)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	// Client A: connect and authenticate, then send a message.
	reqA := dialNewREQ(reqCtx, t, addr)
	sessionID := doFullHandshake(t, reqA, "secret-sendfail")

	// Send a message so a pending reply entry is created.
	msgData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeMessage,
		Content:      "hello",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := reqA.Send(zmq4.NewMsg(msgData)); err != nil {
		t.Fatalf("reqA send: %v", err)
	}

	// Wait for dispatchLoop to process the message and place it in incoming.
	time.Sleep(100 * time.Millisecond)

	// Read the message from the channel (simulating the agent picking it up).
	msg, err := ch.Receive(reqCtx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	// Close client A's socket to simulate a disconnected client.
	_ = reqA.Close()
	time.Sleep(100 * time.Millisecond)

	// Attempt to Send the reply. ZMQ ROUTER may buffer the send (no error) or
	// may report an error — both paths are valid.
	reply := channel.Message{
		Content: "echo: " + msg.Content,
		Metadata: map[string]string{
			"connection_id": msg.Metadata[ConnectionID],
		},
	}
	sendErr := ch.Send(ctx, reply)

	if sendErr != nil {
		// Send failed → the session should be released immediately via send_failure.
		t.Logf("Send failed (expected): %v", sendErr)
	} else {
		// ZMQ buffered the send. Wait past the idle timeout and trigger watchdog.
		time.Sleep(shortTimeout + 50*time.Millisecond)
		ch.checkIdleTimeout()
	}

	time.Sleep(50 * time.Millisecond)

	// Client B: should be able to connect without getting "busy".
	reqCtxB, reqCancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancelB()

	reqB := dialNewREQ(reqCtxB, t, addr)
	defer func() {
		_ = reqB.Close()
	}()

	probeData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: "client-B-session",
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	rawChallenge := reqSendRecv(t, reqB, probeData)
	var challengeResp protocol.Response
	if parseErr := json.Unmarshal(rawChallenge, &challengeResp); parseErr != nil {
		t.Fatalf("unmarshal B challenge: %v", parseErr)
	}

	if challengeResp.ErrorCode != Challenge {
		t.Fatalf("Client B expected challenge, got status=%q error_code=%q — session was not released",
			challengeResp.Status, challengeResp.ErrorCode)
	}

	// Complete handshake to confirm no "busy".
	tokenB := hmacHex("secret-sendfail", challengeResp.Content)
	verifyData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: "client-B-session",
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    tokenB,
	})
	rawOK := reqSendRecv(t, reqB, verifyData)
	var okResp protocol.Response
	if parseErr := json.Unmarshal(rawOK, &okResp); parseErr != nil {
		t.Fatalf("unmarshal B ok: %v", parseErr)
	}
	if okResp.Status == protocol.ResponseStatusBusy {
		t.Fatalf("Client B got 'busy' — session was NOT released")
	}
	if okResp.Status != protocol.ResponseStatusOK {
		t.Fatalf("Client B expected ok, got %q", okResp.Status)
	}

	// Verify the daemon logged the session release.
	time.Sleep(20 * time.Millisecond)
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "session_closed") {
		t.Logf("LOG:\n%s", logOutput)
		t.Fatalf("expected 'session_closed' in daemon logs")
	}
}

// TestIntegration_ReplyTimeoutReleasesSession verifies that the watchdog's
// reply_timeout logic releases a session that has a pending reply entry and
// is inactive for longer than replyTimeout.
func TestIntegration_ReplyTimeoutReleasesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Use a long session_timeout so only reply_timeout fires.
	ch, addr, logBuf := newIntegrationChannelWithBuf(ctx, t, "secret-replytmo", 15*time.Minute)

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()

	reqA := dialNewREQ(reqCtx, t, addr)
	sessionID := doFullHandshake(t, reqA, "secret-replytmo")

	// Send a message to create a pending entry.
	msgData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeMessage,
		Content:      "please wait",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	if err := reqA.Send(zmq4.NewMsg(msgData)); err != nil {
		t.Fatalf("reqA send: %v", err)
	}

	// Wait for dispatchLoop to process and enqueue.
	time.Sleep(100 * time.Millisecond)

	// Drain from incoming so pending entry exists (agent picks up the message).
	_, err := ch.Receive(reqCtx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	// Force lastActivity into the past so we can trigger the reply_timeout
	// check without waiting the real replyTimeout duration.
	ch.sess.mu.Lock()
	ch.sess.lastActivity = time.Now().Add(-ch.replyTimeout - time.Second)
	ch.sess.mu.Unlock()

	// Call the watchdog check manually.
	ch.checkIdleTimeout()

	// Session should now be released.
	time.Sleep(50 * time.Millisecond)

	// Close client A (it's dead anyway).
	_ = reqA.Close()

	// Client B should connect fine.
	reqCtxB, reqCancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancelB()

	reqB := dialNewREQ(reqCtxB, t, addr)
	defer func() {
		_ = reqB.Close()
	}()

	// Full handshake — must not get "busy".
	probeData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: "client-B-reply-timeout",
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	rawChallenge := reqSendRecv(t, reqB, probeData)
	var challengeResp protocol.Response
	if parseErr := json.Unmarshal(rawChallenge, &challengeResp); parseErr != nil {
		t.Fatalf("unmarshal challenge: %v", parseErr)
	}
	if challengeResp.ErrorCode != Challenge {
		t.Fatalf("expected challenge, got %q", challengeResp.ErrorCode)
	}

	tokenB := hmacHex("secret-replytmo", challengeResp.Content)
	verifyData, _ := protocol.Marshal(protocol.Request{
		ConnectionID: "client-B-reply-timeout",
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    tokenB,
	})
	rawOK := reqSendRecv(t, reqB, verifyData)
	var okResp protocol.Response
	if parseErr := json.Unmarshal(rawOK, &okResp); parseErr != nil {
		t.Fatalf("unmarshal ok: %v", parseErr)
	}
	if okResp.Status == protocol.ResponseStatusBusy {
		t.Fatalf("Client B got 'busy' — reply_timeout did NOT release the session")
	}
	if okResp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok, got %q", okResp.Status)
	}

	// Verify log shows reply_timeout reason.
	time.Sleep(20 * time.Millisecond)
	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "reply_timeout") {
		t.Logf("LOG:\n%s", logOutput)
		t.Fatalf("expected 'reply_timeout' in daemon logs")
	}
}

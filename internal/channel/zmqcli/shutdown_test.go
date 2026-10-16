//nolint:goconst
package zmqcli

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// hmacHex computes hex(HMAC-SHA256(secret, hex-decoded nonce)).
// Used by tests to derive the correct auth_token from a challenge nonce.
func hmacHex(secret, nonceHex string) string {
	nonce, _ := hex.DecodeString(nonceHex)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nonce)

	return hex.EncodeToString(mac.Sum(nil))
}

// newTestChannel creates a Channel bound to tcp://127.0.0.1:0 using a nop
// logger. The test's cleanup calls ch.Close() automatically.
func newTestChannel(ctx context.Context, t *testing.T, sessionTimeout time.Duration) (*Channel, string) {
	t.Helper()

	timeout := sessionTimeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}

	cfg := config.ZMQCLIChannelConfig{
		Bind:                  "tcp://127.0.0.1:0",
		AuthToken:             "test-secret",
		ConnectionIdleTimeout: timeout,
	}

	ch, err := NewChannel(ctx, cfg, "test-secret", false, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewChannel: %v", err)
	}

	t.Cleanup(func() { _ = ch.Close() })

	return ch, ch.listener.Addr()
}

// dialREQ opens a REQ socket and dials addr. The caller must call Close when done.
func dialREQ(ctx context.Context, t *testing.T, addr string) zmq4.Socket {
	t.Helper()

	req := zmq4.NewReq(ctx)
	if err := req.Dial(addr); err != nil {
		req.Close()
		t.Fatalf("REQ Dial(%q): %v", addr, err)
	}

	return req
}

// reqSendRecv sends raw JSON on req and returns the response JSON.
func reqSendRecv(t *testing.T, req zmq4.Socket, payload []byte) []byte {
	t.Helper()

	if err := req.Send(zmq4.NewMsg(payload)); err != nil {
		t.Fatalf("REQ Send: %v", err)
	}

	msg, err := req.Recv()
	if err != nil {
		t.Fatalf("REQ Recv: %v", err)
	}

	if len(msg.Frames) == 0 {
		t.Fatal("received empty message")
	}

	return msg.Frames[len(msg.Frames)-1]
}

// doHandshake performs the two-round session_start handshake and asserts ok.
// It returns the final authenticated response so callers can inspect its
// payload (llm-status-visibility: the model-status fields).
func doHandshake(t *testing.T, req zmq4.Socket, sessionID, secret string) protocol.Response {
	t.Helper()

	// Round 1: probe.
	probe, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	raw := reqSendRecv(t, req, probe)

	var challengeResp protocol.Response
	if err := json.Unmarshal(raw, &challengeResp); err != nil {
		t.Fatalf("unmarshal challenge response: %v", err)
	}
	if challengeResp.ErrorCode != Challenge {
		t.Fatalf("expected challenge error_code, got status=%q error_code=%q", challengeResp.Status, challengeResp.ErrorCode)
	}

	// Round 2: verify.
	verify, _ := protocol.Marshal(protocol.Request{
		ConnectionID: sessionID,
		Type:         protocol.RequestTypeSessionStart,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AuthToken:    hmacHex(secret, challengeResp.Content),
	})
	raw = reqSendRecv(t, req, verify)

	var okResp protocol.Response
	if err := json.Unmarshal(raw, &okResp); err != nil {
		t.Fatalf("unmarshal ok response: %v", err)
	}
	if okResp.Status != protocol.ResponseStatusOK {
		t.Fatalf("expected ok after handshake, got %q", okResp.Status)
	}

	return okResp
}

// TestClose_SendsShutdownToActiveClient verifies that calling ch.Close() while
// a client session is active sends Response{Status:"shutdown"} to the client
// .
func TestClose_SendsShutdownToActiveClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newTestChannel(ctx, t, 0)

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req := dialREQ(reqCtx, t, addr)
	defer req.Close()

	// Complete the auth handshake so a session is active.
	doHandshake(t, req, "sess-shutdown-test", "test-secret")

	// Close the channel — this should send a shutdown notification.
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The client must receive the shutdown response. Give it a short deadline.
	type result struct {
		resp protocol.Response
		err  error
	}

	done := make(chan result, 1)
	go func() {
		msg, recvErr := req.Recv()
		if recvErr != nil {
			done <- result{err: recvErr}

			return
		}
		var r protocol.Response
		if parseErr := json.Unmarshal(msg.Frames[len(msg.Frames)-1], &r); parseErr != nil {
			done <- result{err: parseErr}

			return
		}
		done <- result{resp: r}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("client did not receive shutdown notification: %v", res.err)
		}
		if res.resp.Status != protocol.ResponseStatusShutdown {
			t.Fatalf("expected shutdown status, got %q", res.resp.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shutdown notification from daemon")
	}
}

// TestClose_Idempotent verifies that calling Close multiple times does not panic
// or return an error.
func TestClose_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := newTestChannel(ctx, t, 0)

	if err := ch.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	if err := ch.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

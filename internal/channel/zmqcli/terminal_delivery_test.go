package zmqcli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// TestIntegration_ProgressThenTerminalReply_ExactlyOnce is the channel-level
// regression test for terminal delivery (a ZMQ terminal reply
// consumes the pending slot exactly once): a turn that
// streamed progress chunks and then failed must deliver exactly one final
// reply carrying the terminal message — the progress notifications must not
// consume the pending reply slot, and a second Send must be dropped.
func TestIntegration_ProgressThenTerminalReply_ExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newIntegrationChannel(ctx, t, "secret-terminal", 0, zerolog.Nop())

	// DEALER client, mirroring the ready chat client's socket shape.
	dealer := zmq4.NewDealer(ctx, zmq4.WithID(zmq4.SocketIdentity("terminal-reply-session")))
	if err := dealer.Dial(addr); err != nil {
		t.Fatalf("dealer.Dial(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = dealer.Close() })

	// Two-round HMAC handshake over the DEALER socket.
	doHandshake(t, dealer, "terminal-reply-session", "secret-terminal")

	const (
		progressChunk = "partial answer chunk"
		terminalMsg   = "The response could not be completed. Some actions may already have run; check the current state before repeating the request."
	)

	// Server-side driver: receive the user message, then replay the
	// dispatcher's failed-turn delivery order — a progress notification for
	// the streamed chunk, the terminal reply via Send, and a duplicate Send
	// that must be dropped because the pending slot was already consumed.
	driverDone := make(chan struct{})

	go func() {
		defer close(driverDone)

		msg, err := ch.Receive(ctx)
		if err != nil {
			return
		}

		sessionID := msg.Metadata[ConnectionID]

		if err := ch.SendNotification(ctx, sessionID, progressChunk); err != nil {
			t.Errorf("SendNotification: %v", err)
		}

		if err := ch.Send(ctx, channel.Message{
			Content:  terminalMsg,
			Metadata: map[string]string{ConnectionID: sessionID},
		}); err != nil {
			t.Errorf("Send: %v", err)
		}

		// Duplicate final reply: the pending entry is gone, so the channel
		// must drop it rather than deliver a second reply.
		//
		// criticalities-remediation (LOW zmq): the drop is now observable —
		// the second Send returns the drop error instead of a silent nil
		// success, while the socket stays silent (exactly-once delivery).
		dupErr := ch.Send(ctx, channel.Message{
			Content:  terminalMsg,
			Metadata: map[string]string{ConnectionID: sessionID},
		})
		if dupErr == nil {
			t.Error("duplicate Send reported success — a dropped reply must not read as nil")
		}
	}()

	// Client: send the message request (raw send; responses are read
	// individually below).
	data, err := protocol.Marshal(protocol.Request{
		ConnectionID: "terminal-reply-session",
		Type:         protocol.RequestTypeMessage,
		Content:      "driver message",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := dealer.Send(zmq4.NewMsg(data)); err != nil {
		t.Fatalf("dealer.Send: %v", err)
	}

	// First wire message: the progress chunk — not the final reply.
	progress, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for progress notification")
	}

	if progress.ResponseType != protocol.ResponseTypeProgress {
		t.Fatalf("expected first delivery to be progress, got %q", progress.ResponseType)
	}

	if progress.Content != progressChunk {
		t.Fatalf("expected progress chunk %q, got %q", progressChunk, progress.Content)
	}

	// Second wire message: exactly one final reply carrying the terminal text.
	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for final reply")
	}

	if reply.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("expected final delivery to be a reply, got %q", reply.ResponseType)
	}

	if reply.Content != terminalMsg {
		t.Fatalf("expected terminal message, got %q", reply.Content)
	}

	// No third delivery: the duplicate Send must have been dropped, so the
	// socket stays silent past a generous quiet period.
	if extra, ok := recvDealerResponse(t, dealer, 750*time.Millisecond); ok {
		t.Fatalf("expected exactly one reply, received an extra delivery: %+v", extra)
	}

	select {
	case <-driverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server-side driver did not finish")
	}
}

// recvDealerResponse reads one response from the dealer socket with a
// deadline. ok=false means nothing arrived in time.
func recvDealerResponse(t *testing.T, dealer zmq4.Socket, timeout time.Duration) (protocol.Response, bool) {
	t.Helper()

	type recvResult struct {
		msg zmq4.Msg
		err error
	}

	resCh := make(chan recvResult, 1)

	go func() {
		msg, err := dealer.Recv()
		resCh <- recvResult{msg: msg, err: err}
	}()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("dealer.Recv: %v", res.err)
		}

		if len(res.msg.Frames) == 0 {
			t.Fatal("received empty message")
		}

		var resp protocol.Response
		if err := json.Unmarshal(res.msg.Frames[len(res.msg.Frames)-1], &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}

		return resp, true
	case <-time.After(timeout):
		return protocol.Response{}, false
	}
}

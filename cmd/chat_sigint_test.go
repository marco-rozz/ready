package main

// Regression test for: ^C (SIGINT) does not exit the `chat` client while
// waiting for the LLM reply.
//
// Root cause: go-zeromq/zmq4's Recv() does NOT honour context cancellation
// once the underlying read is already in progress. The fix (calling
// dealer.Close() from the SIGINT handler) is what this test exercises directly.
//
// This test exercises the non-TUI interactive path (the line-reader fallback
// with a piped stdin / non-TTY stdout), which keeps the synchronous recvLoop
// and remains directly testable without a PTY. The TUI path's SIGINT handling
// is covered by internal/tui tests.

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/urfave/cli/v3"
)

// TestChatInteractive_SIGINTWhileWaitingForReply verifies that chatInteractive
// returns ExitSIGINT promptly when:
//  1. A message has been sent to the daemon (DEALER → ROUTER delivered).
//  2. The daemon never replies (simulates a long-running LLM call).
//  3. sigIntFired is closed AND dealer.Close() is called (what the SIGINT handler
//     in runChat now does).
func TestChatInteractive_SIGINTWhileWaitingForReply(t *testing.T) {
	t.Parallel()

	// --- minimal "slow daemon": accepts connections + consumes messages but
	// never sends any reply. This simulates the LLM taking >120 s. ----------
	routerCtx := t.Context()
	router := zmq4.NewRouter(routerCtx)
	if err := router.Listen("tcp://127.0.0.1:0"); err != nil {
		t.Fatalf("router.Listen: %v", err)
	}
	t.Cleanup(func() { _ = router.Close() })

	netAddr := router.Addr()
	if netAddr == nil {
		t.Fatal("router.Addr() returned nil — cannot determine bound address")
	}

	addr := "tcp://" + netAddr.String()

	// Drain arriving messages but never reply.
	go func() {
		for {
			if _, err := router.Recv(); err != nil {
				return
			}
		}
	}()

	// --- set up the DEALER side (simulates a post-auth chat session) -----------
	dealer := zmq4.NewDealer(t.Context(), zmq4.WithID(zmq4.SocketIdentity("test-sigint-session")))
	if err := dealer.Dial(addr); err != nil {
		t.Fatalf("dealer.Dial(%s): %v", addr, err)
	}
	// Note: do NOT defer dealer.Close() here — the test closes it explicitly to
	// simulate what the SIGINT handler does.

	sigIntFired := make(chan struct{})

	// Provide a stdin pipe: write one line so chatInteractive sends a message
	// and blocks waiting for the reply that never comes.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	var stdout, stderr bytes.Buffer

	// Start the receive loop (non-TUI path).
	recv := newRecvLoop(dealer)
	go recv.run(t.Context(), sigIntFired)

	// Run chatInteractive in the background (non-TTY stdout path).
	done := make(chan error, 1)
	go func() {
		done <- chatInteractive(
			t.Context(),
			dealer, "test-sigint-session",
			&stdout, &stderr,
			pr,
			sigIntFired,
			func() {}, // doSessionEnd no-op for this test
			120*time.Second,
			recv,
			privacy.FromHome(nil),
			false, // raw
			false, // stdoutTTY
		)
	}()

	// Allow the goroutine to start and reach the lineCh select.
	time.Sleep(50 * time.Millisecond)

	// Write a message — chatInteractive sends it to the router (which consumes
	// but never replies), leaving waitReply blocked indefinitely.
	if _, err := io.WriteString(pw, "hello, are you there?\n"); err != nil {
		t.Fatalf("write to stdin pipe: %v", err)
	}

	// Allow time for the message to travel DEALER → ROUTER and chatInteractive
	// to reach the waitReply() call.
	time.Sleep(150 * time.Millisecond)

	// ---- simulate the SIGINT handler ----------------------------------------
	close(sigIntFired)
	_ = dealer.Close()

	// chatInteractive MUST return within a short deadline with ExitSIGINT (130).
	select {
	case err := <-done:
		var exitErr cli.ExitCoder
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected cli.ExitCoder, got %T: %v", err, err)
		}

		if got := exitErr.ExitCode(); got != ExitSIGINT {
			t.Errorf("exit code = %d, want %d (ExitSIGINT)", got, ExitSIGINT)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("chatInteractive did not return within 2 s after dealer.Close() — " +
			"the SIGINT-unblocking fix in runChat is not working")
	}
}

// TestExitCodeMapping pins the dispatcher's control/error → exit-code mapping
// for the non-TUI paths (post-handshake control, error reply, timeout).
func TestExitCodeMapping(t *testing.T) {
	t.Parallel()

	// handleControlAfter: shutdown → 0.
	if got := exitCode(t, handleControlAfter(
		response("shutdown", ""), &bytes.Buffer{}, func() {},
	)); got != ExitOK {
		t.Errorf("control shutdown exit = %d, want %d", got, ExitOK)
	}

	// handleControlAfter: no_active_session → 1 (session expired).
	if got := exitCode(t, handleControlAfter(
		response("error", "no_active_session"), &bytes.Buffer{}, func() {},
	)); got != ExitErr {
		t.Errorf("control no_active_session exit = %d, want %d", got, ExitErr)
	}

	// finalizeReply: error reply with session_timeout → 1.
	if got := exitCode(t, finalizeReply(
		response("error", "session_timeout"), &bytes.Buffer{}, &bytes.Buffer{}, func() {}, privacy.FromHome(nil), false,
	)); got != ExitErr {
		t.Errorf("reply session_timeout exit = %d, want %d", got, ExitErr)
	}

	// finalizeReply: shutdown reply → 0.
	if got := exitCode(t, finalizeReply(
		response("shutdown", ""), &bytes.Buffer{}, &bytes.Buffer{}, func() {}, privacy.FromHome(nil), false,
	)); got != ExitOK {
		t.Errorf("reply shutdown exit = %d, want %d", got, ExitOK)
	}
}

func response(status, errorCode string) protocol.Response {
	return protocol.Response{
		Status:    protocol.ResponseStatus(status),
		ErrorCode: errorCode,
	}
}

func exitCode(t *testing.T, err error) int {
	t.Helper()

	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected cli.ExitCoder, got %T: %v", err, err)
	}

	return exitErr.ExitCode()
}

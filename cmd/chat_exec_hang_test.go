package main

// Regression tests for criticalities-review-2026-09-24 C1: runChat blocks
// forever on cleanupWg.Wait() on every successful --exec and on every
// handshake-failure return path — doSessionEnd is only ever invoked by the
// SIGINT/cancel branches, so the documented exit codes 0/2/3/124 are never
// delivered and the daemon-side ZMQ session stays locked until the idle
// watchdog.
//
// Both tests run the `chat` Action end-to-end against a fake ZMQ daemon
// (ROUTER) that speaks the wire protocol (challenge/OK handshake, exec
// reply, session_end acknowledgement) and assert the process returns its
// exit code promptly. Pre-fix, both hang in cleanupWg.Wait() and fail on
// the deadline.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/urfave/cli/v3"
)

const execHangTestTimeout = 10 * time.Second

// fakeChatDaemon is a minimal ROUTER-speaking daemon for chat e2e tests:
// it issues the round-1 challenge, accepts or rejects the round-2 HMAC
// handshake, answers exec requests with a fixed OK reply, and reports
// session_end receptions on the returned channel.
type fakeChatDaemon struct {
	router     zmq4.Socket
	addr       string
	authOK     bool
	execReply  string
	sessionEnd chan string
	done       chan struct{}
}

func newFakeChatDaemon(t *testing.T, authOK bool) *fakeChatDaemon {
	t.Helper()

	d := &fakeChatDaemon{
		router:     zmq4.NewRouter(t.Context()),
		authOK:     authOK,
		execReply:  "lights off",
		sessionEnd: make(chan string, 4),
		done:       make(chan struct{}),
	}

	if err := d.router.Listen("tcp://127.0.0.1:0"); err != nil {
		t.Fatalf("router.Listen: %v", err)
	}

	netAddr := d.router.Addr()
	if netAddr == nil {
		t.Fatal("router.Addr() returned nil")
	}

	d.addr = "tcp://" + netAddr.String()

	go d.serve()

	t.Cleanup(func() { _ = d.router.Close() })

	return d
}

func (d *fakeChatDaemon) serve() {
	defer close(d.done)

	for {
		msg, err := d.router.Recv()
		if err != nil {
			return
		}

		if len(msg.Frames) < 2 {
			continue
		}

		identity := msg.Frames[0]
		payload := msg.Frames[len(msg.Frames)-1]

		var req protocol.Request
		if json.Unmarshal(payload, &req) != nil {
			continue
		}

		switch req.Type {
		case protocol.RequestTypeSessionStart:
			switch {
			case req.AuthToken == "":
				nonce := make([]byte, 16)
				_, _ = rand.Read(nonce)
				d.reply(identity, protocol.Response{
					ConnectionID: req.ConnectionID,
					Status:       protocol.ResponseStatusOK,
					RequestID:    req.RequestID,
					ResponseType: protocol.ResponseTypeControl,
					Content:      hex.EncodeToString(nonce),
					ErrorCode:    "challenge",
				})
			case d.authOK:
				d.reply(identity, protocol.Response{
					ConnectionID: req.ConnectionID,
					Status:       protocol.ResponseStatusOK,
					RequestID:    req.RequestID,
					ResponseType: protocol.ResponseTypeControl,
				})
			default:
				d.reply(identity, protocol.Response{
					ConnectionID: req.ConnectionID,
					Status:       protocol.ResponseStatusAuthFail,
					RequestID:    req.RequestID,
					ResponseType: protocol.ResponseTypeControl,
				})
			}

		case protocol.RequestTypeExec, protocol.RequestTypeMessage:
			d.reply(identity, protocol.Response{
				ConnectionID: req.ConnectionID,
				Status:       protocol.ResponseStatusOK,
				RequestID:    req.RequestID,
				ResponseType: protocol.ResponseTypeReply,
				Content:      d.execReply,
			})

		case protocol.RequestTypeSessionEnd:
			d.sessionEnd <- req.ConnectionID
			d.reply(identity, protocol.Response{
				ConnectionID: req.ConnectionID,
				Status:       protocol.ResponseStatusOK,
				RequestID:    req.RequestID,
				ResponseType: protocol.ResponseTypeControl,
			})
		}
	}
}

func (d *fakeChatDaemon) reply(identity []byte, resp protocol.Response) {
	payload, _ := protocol.Marshal(resp)
	// Framing mirrors internal/transport/zmq.Listener.Send: [identity, "", payload].
	_ = d.router.Send(zmq4.NewMsgFrom(identity, []byte{}, payload))
}

func (d *fakeChatDaemon) waitSessionEnd(t *testing.T) {
	t.Helper()

	select {
	case <-d.sessionEnd:
	case <-time.After(execHangTestTimeout):
		t.Fatal("session_end never reached the fake daemon within the deadline")
	}
}

// newChatActionCmd builds the chat command with the real flag wiring; the
// test calls runChat directly (not cmd.Run, whose HandleExitCoder would
// os.Exit inside the test process).
func newChatActionCmd(t *testing.T, d *fakeChatDaemon) *cli.Command {
	t.Helper()

	cmd := &cli.Command{
		Name: "chat",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "address"},
			&cli.StringFlag{Name: "token"},
			&cli.StringFlag{Name: "exec"},
			&cli.DurationFlag{Name: "reply-timeout", Value: 5 * time.Second},
			&cli.BoolFlag{Name: "raw"},
		},
	}

	for _, set := range [][2]string{
		{"address", d.addr},
		{"token", "test-secret"},
		{"exec", "turn off the lights"},
		{"reply-timeout", "5s"},
		{"raw", "false"},
	} {
		if err := cmd.Set(set[0], set[1]); err != nil {
			t.Fatalf("cmd.Set(%s): %v", set[0], err)
		}
	}

	return cmd
}

// TestChat_ExecSuccessExitsPromptly reproduces C1: after a successful exec
// the client must send session_end and deliver exit 0 — never hang on its
// cleanup wait-group.
func TestChat_ExecSuccessExitsPromptly(t *testing.T) {
	t.Parallel()

	d := newFakeChatDaemon(t, true)

	var stdout, stderr bytes.Buffer
	cmd := newChatActionCmd(t, d)

	done := make(chan error, 1)
	go func() {
		done <- runChat(context.Background(), cmd, &stdout, &stderr, bytes.NewReader(nil))
	}()

	select {
	case err := <-done:
		var exitErr cli.ExitCoder
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected cli.ExitCoder, got %T: %v (stderr: %q)", err, err, stderr.String())
		}

		if got := exitErr.ExitCode(); got != ExitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %q)", got, ExitOK, stderr.String())
		}

		// session_end must have reached the daemon (no locked session until
		// the idle watchdog).
		d.waitSessionEnd(t)

	case <-time.After(execHangTestTimeout):
		t.Fatalf("runChat hung after a successful --exec (C1): cleanupWg.Wait() never returned because doSessionEnd was never called (stdout: %q stderr: %q)", stdout.String(), stderr.String())
	}
}

// TestChat_HandshakeAuthFailExitsPromptly pins the handshake-failure
// variant of C1: an auth_fail must deliver exit 3 promptly — not block on
// a cleanup wait-group whose Done was never signalled.
func TestChat_HandshakeAuthFailExitsPromptly(t *testing.T) {
	t.Parallel()

	d := newFakeChatDaemon(t, false)

	var stdout, stderr bytes.Buffer
	cmd := newChatActionCmd(t, d)
	_ = cmd.Set("token", "wrong-secret")

	done := make(chan error, 1)
	go func() {
		done <- runChat(context.Background(), cmd, &stdout, &stderr, bytes.NewReader(nil))
	}()

	select {
	case err := <-done:
		var exitErr cli.ExitCoder
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected cli.ExitCoder, got %T: %v (stderr: %q)", err, err, stderr.String())
		}

		if got := exitErr.ExitCode(); got != ExitAuthFail {
			t.Fatalf("exit code = %d, want %d (stderr: %q)", got, ExitAuthFail, stderr.String())
		}

		// The client must still tell the daemon to release the session lock
		// (endOnce makes the SIGINT-branch session_end idempotent).
		d.waitSessionEnd(t)

	case <-time.After(execHangTestTimeout):
		t.Fatalf("runChat hung after an auth-failed handshake (C1): exit code 3 never delivered (stderr: %q)", stderr.String())
	}
}

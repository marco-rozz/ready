package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"

	"github.com/marco-rozz/ready/internal/protocol"
)

func noticeEventResponse() protocol.Response {
	return protocol.Response{
		ConnectionID: "c", Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl,
		Content: protocol.ControlNotice, Notice: &protocol.NoticeInfo{Text: "x"}, Turn: &protocol.TurnInfo{TurnID: "t"},
	}
}

// Only the interactive TUI declares it renders Mid-turn Notices; the loop
// path used by --exec, --raw and the line reader declares nothing.
func TestChatHandshake_DeclaresNoticeRenderingOnTUIPathOnly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		tui  bool
	}{{"tui handshake declares", true}, {"loop handshake declares nothing", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := newFakeChatDaemon(t, true)
			dealer := zmq4.NewDealer(t.Context())

			if err := dealer.Dial(d.addr); err != nil {
				t.Fatalf("Dial: %v", err)
			}

			t.Cleanup(func() { _ = dealer.Close() })

			var stderr bytes.Buffer

			var err error

			if tc.tui {
				_, err = chatHandshakeDirect(t.Context(), dealer, "conn-notice", "tok", 3*time.Second, &stderr, nil)
			} else {
				recv := newRecvLoop(dealer)
				ctx, cancel := context.WithCancel(t.Context())

				defer cancel()

				go recv.run(ctx, nil)

				_, err = chatHandshakeLoop(ctx, dealer, "conn-notice", "tok", 3*time.Second, &stderr, nil, recv)
			}

			if err != nil {
				t.Fatalf("handshake: %v (stderr %q)", err, stderr.String())
			}

			select {
			case got := <-d.authRenders:
				if got != tc.tui {
					t.Fatalf("renders_notices = %v, want %v", got, tc.tui)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("daemon never saw the authenticating round")
			}
		})
	}
}

// A stray notice on a non-rendering path is ignored: the reply wait resumes
// and only the reply is returned.
func TestWaitReplyForRequest_IgnoresStrayNotice(t *testing.T) {
	r := newRecvLoop(nil)

	r.control <- noticeEventResponse()
	r.replies <- protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply, RequestID: "r1", Content: "reply-text"}

	resp, err := waitReplyForRequest(context.Background(), r, time.Second, nil, "r1")
	if err != nil || resp.Content != "reply-text" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}

	if !isModelStatusControl(noticeEventResponse()) {
		t.Fatal("a notice event must be treated as ignorable informational control")
	}
}

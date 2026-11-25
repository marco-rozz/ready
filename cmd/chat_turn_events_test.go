package main

import (
	"context"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/protocol"
)

func turnControlEvent(content string, turn *protocol.TurnInfo) protocol.Response {
	return protocol.Response{ConnectionID: "c", Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, RequestID: "r1", Content: content, Turn: turn}
}

// 13.2/13.3 — the non-TUI paths ignore the acknowledgement and Turn events:
// the reply wait resumes and only the reply is returned.
func TestWaitReplyForRequest_IgnoresTurnEvents(t *testing.T) {
	r := newRecvLoop(nil)

	r.control <- turnControlEvent(protocol.ControlMessageAccepted, &protocol.TurnInfo{Disposition: protocol.DispositionQueued})
	r.control <- turnControlEvent(protocol.ControlTurnStarted, &protocol.TurnInfo{TurnID: "t", RequestIDs: []string{"r1"}})
	r.control <- turnControlEvent(protocol.ControlSteerInjected, &protocol.TurnInfo{TurnID: "t", RequestIDs: []string{"r2"}})
	r.replies <- protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply, RequestID: "r1", Content: "the reply"}

	resp, err := waitReplyForRequest(context.Background(), r, time.Second, nil, "r1")
	if err != nil || resp.Content != "the reply" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

// A reply addressed to another request is skipped; a reply whose
// turn.request_ids contains the request answers it.
func TestWaitReplyForRequest_MatchesByRequestSet(t *testing.T) {
	r := newRecvLoop(nil)

	r.replies <- protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply, RequestID: "other", Content: "not mine"}
	r.replies <- protocol.Response{
		Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeReply, RequestID: "r0", Content: "consumed me",
		Turn: &protocol.TurnInfo{TurnID: "t", RequestIDs: []string{"r0", "r5"}},
	}

	resp, err := waitReplyForRequest(context.Background(), r, time.Second, nil, "r5")
	if err != nil || resp.Content != "consumed me" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

// Lifecycle control (shutdown / session expiry) is still honoured.
func TestWaitReplyForRequest_ShutdownStillEndsTheWait(t *testing.T) {
	r := newRecvLoop(nil)
	r.control <- protocol.Response{Status: protocol.ResponseStatusShutdown, ResponseType: protocol.ResponseTypeControl}

	if _, err := waitReplyForRequest(context.Background(), r, time.Second, nil, "r1"); err == nil {
		t.Fatal("a shutdown control must end the wait")
	}
}

func TestReplyAnswers(t *testing.T) {
	cases := []struct {
		name string
		resp protocol.Response
		want bool
	}{
		{"own id", protocol.Response{RequestID: "r1"}, true},
		{"legacy no id", protocol.Response{}, true},
		{"other id", protocol.Response{RequestID: "r9"}, false},
		{"consumed by turn", protocol.Response{RequestID: "r9", Turn: &protocol.TurnInfo{RequestIDs: []string{"r9", "r1"}}}, true},
		{"turn without it", protocol.Response{RequestID: "r9", Turn: &protocol.TurnInfo{RequestIDs: []string{"r9"}}}, false},
	}

	for _, tc := range cases {
		if got := replyAnswers(tc.resp, "r1"); got != tc.want {
			t.Errorf("%s: replyAnswers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

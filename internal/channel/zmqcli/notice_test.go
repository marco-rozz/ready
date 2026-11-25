package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
)

const noticeTurnID = "turn-9"

func newNoticeRig(t *testing.T, conn string, rendersNotices bool) *turnRig {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, addr := newIntegrationChannel(ctx, t, turnSecret, 0, zerolog.Nop())
	dealer := dialStatusDealer(ctx, t, addr, conn)
	doHandshakeNotices(t, dealer, conn, turnSecret, rendersNotices)

	return &turnRig{
		ctx:  ctx,
		ch:   ch,
		conn: conn,
		send: func(typ protocol.RequestType, id, content string) {
			sendDealerRequest(t, dealer, protocol.Request{ConnectionID: conn, Type: typ, RequestID: id, Content: content, Timestamp: testTimestamp})
		},
		recv: func() (protocol.Response, bool) { return recvDealerResponse(t, dealer, 2*time.Second) },
	}
}

func TestSendNotice_NoConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _ := newIntegrationChannel(ctx, t, turnSecret, 0, zerolog.Nop())

	res, err := ch.SendNotice(ctx, nil, "x")
	if err != nil || res.Outcome != channel.NoticeNotShown || res.Reason != channel.NoticeReasonNoConnection {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSendNotice_ClientDoesNotRender_SendsNothing(t *testing.T) {
	r := newNoticeRig(t, "notice-1", false)

	res, err := r.ch.SendNotice(r.ctx, map[string]string{MetaTurnID: "t1"}, "hello")
	if err != nil || res.Outcome != channel.NoticeNotShown || res.Reason != channel.NoticeReasonClientDoesNotRender {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	if resp, ok := r.recv(); ok {
		t.Fatalf("no event may be sent to a non-rendering Connection, got %+v", resp)
	}
}

// A notice between the opener's progress and the final reply never touches
// the pending slot: the reply still closes it exactly once.
func TestSendNotice_EventAndPendingSlotUntouched(t *testing.T) {
	r := newNoticeRig(t, "notice-2", true)

	r.send(protocol.RequestTypeMessage, "r1", "go")
	msg := r.receive(t)

	meta := map[string]string{MetaTurnID: noticeTurnID}

	for _, text := range []string{"first", "second"} {
		res, err := r.ch.SendNotice(r.ctx, meta, text)
		if err != nil || res.Outcome != channel.NoticeDelivered {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	}

	for _, want := range []string{"first", "second"} {
		resp := r.mustRecv(t)
		if resp.Content != protocol.ControlNotice || resp.Notice == nil || resp.Notice.Text != want || resp.Turn == nil || resp.Turn.TurnID != noticeTurnID {
			t.Fatalf("want notice %q, got %+v", want, resp)
		}

		if resp.RequestID != "" || resp.Usage != nil {
			t.Fatalf("a notice carries neither request_id nor usage: %+v", resp)
		}
	}

	reply := channel.Message{Content: "final-answer", Metadata: map[string]string{ConnectionID: msg.Metadata[ConnectionID], RequestID: "r1", MetaTurnID: noticeTurnID}}
	if err := r.ch.Send(r.ctx, reply); err != nil {
		t.Fatalf("final reply must still consume the pending slot after notices: %v", err)
	}

	final := r.mustRecv(t)
	if final.ResponseType != protocol.ResponseTypeReply || final.Content != "final-answer" || final.RequestID != "r1" {
		t.Fatalf("want the final reply, got %+v", final)
	}
}

// The capability is fixed per Connection: a reconnect with another
// declaration switches it.
func TestSendNotice_ReconnectSwitchesCapability(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newIntegrationChannel(ctx, t, turnSecret, 0, zerolog.Nop())

	first := dialStatusDealer(ctx, t, addr, "cap-a")
	doHandshakeNotices(t, first, "cap-a", turnSecret, true)

	if res, err := ch.SendNotice(ctx, nil, "x"); err != nil || res.Outcome != channel.NoticeDelivered {
		t.Fatalf("rendering Connection: res=%+v err=%v", res, err)
	}

	sendDealerRequest(t, first, protocol.Request{ConnectionID: "cap-a", Type: protocol.RequestTypeSessionEnd, Timestamp: testTimestamp})
	// Drain the notice and the session_end acknowledgement, if any.
	for {
		if _, ok := recvDealerResponse(t, first, 300*time.Millisecond); !ok {
			break
		}
	}

	second := dialStatusDealer(ctx, t, addr, "cap-b")
	doHandshakeNotices(t, second, "cap-b", turnSecret, false)

	res, err := ch.SendNotice(ctx, nil, "x")
	if err != nil || res.Outcome != channel.NoticeNotShown || res.Reason != channel.NoticeReasonClientDoesNotRender {
		t.Fatalf("non-rendering Connection: res=%+v err=%v", res, err)
	}
}

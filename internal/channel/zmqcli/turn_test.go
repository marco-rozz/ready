package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
)

const (
	turnSecret = "secret-turn"
	// autoReqID is the request id of tests that send one message and do not care.
	autoReqID = "req-auto"
)

type turnRig struct {
	ctx  context.Context
	ch   *Channel
	conn string
	send func(typ protocol.RequestType, id, content string)
	recv func() (protocol.Response, bool)
}

func newTurnRig(t *testing.T, conn string) *turnRig {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, addr := newIntegrationChannel(ctx, t, turnSecret, 0, zerolog.Nop())
	dealer := dialStatusDealer(ctx, t, addr, conn)
	doHandshake(t, dealer, conn, turnSecret)

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

func (r *turnRig) receive(t *testing.T) channel.Message {
	t.Helper()

	ctx, cancel := context.WithTimeout(r.ctx, 3*time.Second)
	defer cancel()

	msg, err := r.ch.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	return msg
}

func (r *turnRig) mustRecv(t *testing.T) protocol.Response {
	t.Helper()

	resp, ok := r.recv()
	if !ok {
		t.Fatal("timed out waiting for a response")
	}

	return resp
}

// A second `message` while the first is unanswered is admitted — no
// request_in_flight — and the Turn's single final reply answers both ids;
// nothing answers the consumed id a second time.
func TestMidTurnMessageAdmittedAndOneReplyConsumesBoth(t *testing.T) {
	r := newTurnRig(t, "turn-1")

	r.send(protocol.RequestTypeMessage, "r1", "first")
	m1 := r.receive(t)
	r.send(protocol.RequestTypeMessage, "r2", "second")
	m2 := r.receive(t)

	if m1.Origin != channel.OriginHuman || m2.Metadata[RequestID] != "r2" {
		t.Fatalf("messages = %+v / %+v", m1, m2)
	}

	meta := map[string]string{
		ConnectionID:       "turn-1",
		RequestID:          "r1",
		MetaTurnID:         "turn-abc",
		MetaTurnRequestIDs: EncodeIDs([]string{"r1", "r2"}),
	}
	if err := r.ch.Send(r.ctx, channel.Message{Content: "done", Metadata: meta}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	reply := r.mustRecv(t)
	if reply.ResponseType != protocol.ResponseTypeReply || reply.RequestID != "r1" || reply.ErrorCode != "" {
		t.Fatalf("reply = %+v", reply)
	}

	if reply.Turn == nil || reply.Turn.TurnID != "turn-abc" || len(reply.Turn.RequestIDs) != 2 || reply.Turn.RequestIDs[1] != "r2" {
		t.Fatalf("turn = %+v", reply.Turn)
	}

	// r2 was consumed by the same reply: a second reply for it is dropped.
	meta2 := map[string]string{ConnectionID: "turn-1", RequestID: "r2"}
	if err := r.ch.Send(r.ctx, channel.Message{Content: "again", Metadata: meta2}); err == nil {
		t.Fatal("a consumed request id must not be answered twice")
	}

	if r.ch.hasPending("turn-1") {
		t.Fatal("nothing may remain unanswered")
	}
}

// An exec overlapping an unanswered message is rejected structurally under
// its own id, and the earlier request's reply is unaffected.
func TestExecOverlappingMessageRejected(t *testing.T) {
	r := newTurnRig(t, "turn-2")

	r.send(protocol.RequestTypeMessage, "r1", "work")
	_ = r.receive(t)

	r.send(protocol.RequestTypeExec, "e1", "oneshot")

	rej := r.mustRecv(t)
	if rej.ErrorCode != protocol.ErrorCodeRequestInFlight || rej.RequestID != "e1" {
		t.Fatalf("rejection = %+v", rej)
	}

	if err := r.ch.Send(r.ctx, channel.Message{Content: "ok", Metadata: map[string]string{ConnectionID: "turn-2", RequestID: "r1", MetaTurnID: "t"}}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if reply := r.mustRecv(t); reply.RequestID != "r1" || reply.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("reply = %+v", reply)
	}
}

// A message overlapping an unanswered exec is rejected too.
func TestMessageOverlappingExecRejected(t *testing.T) {
	r := newTurnRig(t, "turn-3")

	r.send(protocol.RequestTypeExec, "e1", "oneshot")
	_ = r.receive(t)

	r.send(protocol.RequestTypeMessage, "r1", "chat")

	if rej := r.mustRecv(t); rej.ErrorCode != protocol.ErrorCodeRequestInFlight || rej.RequestID != "r1" {
		t.Fatalf("rejection = %+v", rej)
	}
}

// Idle send: message_accepted precedes turn_started even when the worker
// starts the Turn first; progress then carries the opener's id.
func TestIdleSendEventOrder(t *testing.T) {
	r := newTurnRig(t, "turn-4")

	r.send(protocol.RequestTypeMessage, "r1", "hello")
	_ = r.receive(t)

	// The worker starts the Turn BEFORE the intake acknowledges: turn_started
	// must wait for the ack.
	started := make(chan struct{})

	go func() {
		r.ch.NotifyTurnStarted("turn-4", "r1", "turn-xyz", []string{"r1"}, false)
		close(started)
	}()

	time.Sleep(50 * time.Millisecond)
	r.ch.AcknowledgeMessage("turn-4", "r1", protocol.DispositionQueued, "")

	first := r.mustRecv(t)
	if first.Content != protocol.ControlMessageAccepted || first.Turn.Disposition != protocol.DispositionQueued || first.RequestID != "r1" {
		t.Fatalf("first event = %+v, want message_accepted", first)
	}

	second := r.mustRecv(t)
	if second.Content != protocol.ControlTurnStarted || second.Turn.TurnID != "turn-xyz" || len(second.Turn.RequestIDs) != 1 || second.Turn.Deferred {
		t.Fatalf("second event = %+v, want turn_started", second)
	}

	<-started

	if err := r.ch.SendNotification(r.ctx, "turn-4", "partial"); err != nil {
		t.Fatal(err)
	}

	if prog := r.mustRecv(t); prog.ResponseType != protocol.ResponseTypeProgress || prog.RequestID != "r1" {
		t.Fatalf("progress = %+v", prog)
	}
}

// Mid-turn: message_accepted{steer} then steer_injected with an explicit zero.
func TestMidTurnSteerEvents(t *testing.T) {
	r := newTurnRig(t, "turn-5")

	r.send(protocol.RequestTypeMessage, "r1", "go")
	_ = r.receive(t)
	r.send(protocol.RequestTypeMessage, "r2", "correction")
	_ = r.receive(t)

	r.ch.AcknowledgeMessage("turn-5", "r2", protocol.DispositionSteer, "turn-q")
	r.ch.NotifySteerInjected("turn-5", "r1", "turn-q", []string{"r2"}, 0)

	acc := r.mustRecv(t)
	if acc.Turn.Disposition != protocol.DispositionSteer || acc.Turn.TurnID != "turn-q" || acc.RequestID != "r2" {
		t.Fatalf("accepted = %+v", acc)
	}

	inj := r.mustRecv(t)
	if inj.Content != protocol.ControlSteerInjected || inj.Turn.SupersededToolCalls == nil || *inj.Turn.SupersededToolCalls != 0 || inj.Turn.RequestIDs[0] != "r2" {
		t.Fatalf("injected = %+v", inj)
	}
}

// A deferred Turn: after the first reply, turn_started{deferred} makes the
// deferred request the slot; its own final reply follows.
func TestDeferredTurnStartsWithItsOwnSlot(t *testing.T) {
	r := newTurnRig(t, "turn-6")

	r.send(protocol.RequestTypeMessage, "r1", "one")
	_ = r.receive(t)
	r.send(protocol.RequestTypeMessage, "r2", "late")
	_ = r.receive(t)

	if err := r.ch.Send(r.ctx, channel.Message{Content: "a1", Metadata: map[string]string{ConnectionID: "turn-6", RequestID: "r1", MetaTurnID: "t1", MetaTurnRequestIDs: EncodeIDs([]string{"r1"})}}); err != nil {
		t.Fatal(err)
	}

	if reply := r.mustRecv(t); reply.RequestID != "r1" || len(reply.Turn.RequestIDs) != 1 {
		t.Fatalf("first reply = %+v", reply)
	}

	// r2 is still unanswered; its deferred Turn opens.
	r.ch.AcknowledgeMessage("turn-6", "r2", protocol.DispositionSteer, "t1")
	_ = r.mustRecv(t)

	r.ch.NotifyTurnStarted("turn-6", "r2", "t2", []string{"r2"}, true)

	started := r.mustRecv(t)
	if started.Content != protocol.ControlTurnStarted || !started.Turn.Deferred || started.RequestID != "r2" {
		t.Fatalf("started = %+v", started)
	}

	if err := r.ch.SendNotification(r.ctx, "turn-6", "chunk"); err != nil {
		t.Fatal(err)
	}

	if prog := r.mustRecv(t); prog.RequestID != "r2" {
		t.Fatalf("progress must correlate to the deferred opener: %+v", prog)
	}

	if err := r.ch.Send(r.ctx, channel.Message{Content: "a2", Metadata: map[string]string{ConnectionID: "turn-6", RequestID: "r2", MetaTurnID: "t2", MetaTurnRequestIDs: EncodeIDs([]string{"r2"})}}); err != nil {
		t.Fatal(err)
	}

	if reply := r.mustRecv(t); reply.RequestID != "r2" || reply.Content != "a2" {
		t.Fatalf("second reply = %+v", reply)
	}
}

// A stopped Turn's reply answers the /stop request and the dropped
// candidates with the one final reply.
func TestStoppedTurnReplyAnswersStopAndDropped(t *testing.T) {
	r := newTurnRig(t, "turn-7")

	for _, id := range []string{"r1", "r2", "r4"} {
		r.send(protocol.RequestTypeMessage, id, "x")
		_ = r.receive(t)
	}

	meta := map[string]string{
		ConnectionID:       "turn-7",
		RequestID:          "r1",
		MetaTurnID:         "ts",
		MetaTurnRequestIDs: EncodeIDs([]string{"r1", "r4"}),
		MetaTurnDroppedIDs: EncodeIDs([]string{"r2"}),
	}
	if err := r.ch.Send(r.ctx, channel.Message{Content: "Stopped.", Metadata: meta}); err != nil {
		t.Fatal(err)
	}

	reply := r.mustRecv(t)
	if reply.Turn == nil || len(reply.Turn.RequestIDs) != 2 || reply.Turn.RequestIDs[1] != "r4" || len(reply.Turn.DroppedRequestIDs) != 1 || reply.Turn.DroppedRequestIDs[0] != "r2" {
		t.Fatalf("turn = %+v", reply.Turn)
	}

	if r.ch.hasPending("turn-7") {
		t.Fatal("r1, r2 and r4 must all be answered by that one reply")
	}
}

// A command reply (no Turn metadata) answers its own request and carries no
// turn object; it routes by id even when the slot belongs to another request.
func TestCommandReplyRoutesByRequestID(t *testing.T) {
	r := newTurnRig(t, "turn-8")

	r.send(protocol.RequestTypeMessage, "r1", "turn")
	_ = r.receive(t)
	r.send(protocol.RequestTypeMessage, "r2", "/compact")
	_ = r.receive(t)

	if err := r.ch.Send(r.ctx, channel.Message{Content: "Context compacted.", Metadata: map[string]string{ConnectionID: "turn-8", RequestID: "r2"}}); err != nil {
		t.Fatal(err)
	}

	reply := r.mustRecv(t)
	if reply.RequestID != "r2" || reply.Turn != nil {
		t.Fatalf("command reply = %+v", reply)
	}

	// The Turn's slot (r1) is untouched.
	if err := r.ch.SendNotification(r.ctx, "turn-8", "still working"); err != nil {
		t.Fatal(err)
	}

	if prog := r.mustRecv(t); prog.RequestID != "r1" {
		t.Fatalf("progress must still correlate to r1: %+v", prog)
	}
}

// The unanswered set does not outlive the Connection.
func TestConnectionEndClearsUnanswered(t *testing.T) {
	r := newTurnRig(t, "turn-9")

	r.send(protocol.RequestTypeMessage, "r1", "x")
	_ = r.receive(t)
	r.send(protocol.RequestTypeSessionEnd, "", "")

	deadline := time.After(2 * time.Second)

	for r.ch.hasPending("turn-9") {
		select {
		case <-deadline:
			t.Fatal("session_end must clear every unanswered request")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

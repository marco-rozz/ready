package zmqcli

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/activity"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

const (
	testActivityRequestID = "turn-1"
	testActivityThinking  = "thinking"
)

func TestActivityKeepsReplySlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, addr := newIntegrationChannel(ctx, t, "secret-activity", 0, zerolog.Nop())
	dealer := zmq4.NewDealer(ctx, zmq4.WithID(zmq4.SocketIdentity("activity-connection")))
	if err := dealer.Dial(addr); err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	doHandshake(t, dealer, "activity-connection", "secret-activity")
	req := protocol.Request{ConnectionID: "activity-connection", RequestID: testActivityRequestID, Type: protocol.RequestTypeMessage, Content: "activity request", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	data, err := protocol.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if sendErr := dealer.Send(zmq4.NewMsg(data)); sendErr != nil {
		t.Fatal(sendErr)
	}
	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	if ch.ActivitySender(msg.Metadata[ConnectionID], "old") != nil {
		t.Fatal("old request acquired sender")
	}
	sender := ch.ActivitySender(msg.Metadata[ConnectionID], testActivityRequestID)
	if sender == nil {
		t.Fatal("missing sender")
	}
	if sendErr := sender(protocol.Activity{State: "using_tool", Sequence: 2, ToolName: "test", AdditionalTools: &n}); sendErr != nil {
		t.Fatal(sendErr)
	}
	got, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || got.Activity == nil || got.RequestID != testActivityRequestID {
		t.Fatalf("activity: %+v", got)
	}
	if sendErr := ch.Send(ctx, channel.Message{Content: "activity reply", Metadata: msg.Metadata}); sendErr != nil {
		t.Fatal(sendErr)
	}
	got, ok = recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || got.ResponseType != protocol.ResponseTypeReply || got.Content != "activity reply" {
		t.Fatalf("reply: %+v", got)
	}
	if sendErr := ch.Send(ctx, channel.Message{Content: "again", Metadata: msg.Metadata}); sendErr == nil {
		t.Fatal("duplicate reply accepted")
	}
	req.RequestID = testActivityRequestID // Deliberately reuse the wire ID for a new request.
	data, err = protocol.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err = dealer.Send(zmq4.NewMsg(data)); err != nil {
		t.Fatal(err)
	}
	next, err := ch.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = sender(protocol.Activity{State: testActivityThinking, Sequence: 3}); err != nil {
		t.Fatal(err)
	}
	if err = ch.Send(ctx, channel.Message{Content: "second reply", Metadata: next.Metadata}); err != nil {
		t.Fatal(err)
	}
	got, ok = recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || got.RequestID != testActivityRequestID || got.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("old activity crossed requests: %+v", got)
	}
}

type liveActivityTool struct {
	started chan struct{}
	release chan struct{}
}

func (*liveActivityTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "parallel_tool"}, nil
}

func (t *liveActivityTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	t.started <- struct{}{}
	<-t.release

	return "ok", nil
}

func TestActualParallelToolActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, addr := newIntegrationChannel(ctx, t, "secret-real-activity", 0, zerolog.Nop())
	dealer := zmq4.NewDealer(ctx, zmq4.WithID(zmq4.SocketIdentity("real-activity")))
	if err := dealer.Dial(addr); err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	doHandshake(t, dealer, "real-activity", "secret-real-activity")
	request := protocol.Request{ConnectionID: "real-activity", RequestID: "real-turn", Type: protocol.RequestTypeMessage, Content: "run", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	payload, err := protocol.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = dealer.Send(zmq4.NewMsg(payload)); err != nil {
		t.Fatal(err)
	}
	msg, err := ch.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sender := ch.ActivitySender(msg.Metadata[ConnectionID], msg.Metadata[RequestID])
	if sender == nil {
		t.Fatal("missing activity sender")
	}
	tracker := activity.New()
	defer tracker.Close()
	go func() {
		for s := range tracker.Updates() {
			a := protocol.Activity{State: testActivityThinking, Sequence: s.Sequence}
			if s.ToolName != "" {
				a.State = "using_tool"
				a.ToolName = s.ToolName
				n := s.AdditionalTools
				a.AdditionalTools = &n
			}
			_ = sender(a)
		}
	}()
	inner := &liveActivityTool{started: make(chan struct{}, 2), release: make(chan struct{})}
	wrapped := errdigest.NewTrackedTool(inner, errdigest.NewClassifier(), func(context.Context) string { return "session" }, zerolog.Nop())
	toolCtx := activity.WithTracker(ctx, tracker)
	done := make(chan struct{}, 2)
	invoke := func() { _, _ = wrapped.InvokableRun(toolCtx, `{}`); done <- struct{}{} }
	go invoke()
	<-inner.started
	first, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || first.Activity == nil || first.Activity.ToolName != "parallel_tool" {
		t.Fatalf("first activity: %+v", first)
	}
	go invoke()
	<-inner.started
	both, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || both.Activity == nil || both.Activity.AdditionalTools == nil || *both.Activity.AdditionalTools != 1 {
		t.Fatalf("parallel activity: %+v", both)
	}
	close(inner.release)
	<-done
	<-done
	for {
		event, received := recvDealerResponse(t, dealer, 3*time.Second)
		if !received {
			t.Fatal("missing tool end")
		}
		if event.Activity != nil && event.Activity.State == testActivityThinking {
			break
		}
	}
	if err = ch.Send(ctx, channel.Message{Content: "finished", Metadata: msg.Metadata}); err != nil {
		t.Fatal(err)
	}
	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok || reply.Content != "finished" {
		t.Fatalf("reply: %+v", reply)
	}
}

func TestOldActivityFailureCannotReleaseReusedConnectionID(t *testing.T) {
	const reusedConnection = "same-connection"
	var sessions sessionState
	identity := []byte("same-identity")
	if !sessions.tryAcquire(reusedConnection, identity) {
		t.Fatal("first acquire")
	}
	old := sessions.generationFor(reusedConnection, identity)
	sessions.release()
	if !sessions.tryAcquire(reusedConnection, identity) {
		t.Fatal("second acquire")
	}
	sessions.releaseGeneration(old)
	if got, _, _, _ := sessions.snapshot(); got != reusedConnection {
		t.Fatal("old activity released the new connection")
	}
}

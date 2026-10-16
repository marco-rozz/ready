package zmqcli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

// fakeStatusSource is a controllable ModelStatusSource for tests: current is
// what the handshake reports; pushed events flow to the subscription.
type fakeStatusSource struct {
	mu      sync.Mutex
	current ModelStatusEvent
	events  chan ModelStatusEvent
}

// Guarantees at compile time that *fakeStatusSource implements
// ModelStatusSource.
var _ ModelStatusSource = (*fakeStatusSource)(nil)

func (f *fakeStatusSource) ModelStatus() ModelStatusEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.current
}

func (f *fakeStatusSource) SubscribeModelStatus() (<-chan ModelStatusEvent, func()) {
	return f.events, func() {}
}

// push updates the current status and delivers a transition event.
func (f *fakeStatusSource) push(ev ModelStatusEvent) {
	f.mu.Lock()
	f.current = ev
	f.mu.Unlock()
	f.events <- ev
}

// newStatusTestChannel creates an integration channel with a wired fake
// status source and returns both.
func newStatusTestChannel(ctx context.Context, t *testing.T, current ModelStatusEvent) (*Channel, string, *fakeStatusSource) {
	t.Helper()

	ch, addr := newIntegrationChannel(ctx, t, "secret-status", 0, zerolog.Nop())
	src := &fakeStatusSource{
		current: current,
		events:  make(chan ModelStatusEvent, 8),
	}
	ch.WatchModelStatus(src)

	return ch, addr, src
}

// dialStatusDealer dials a DEALER client with the given session identity.
func dialStatusDealer(ctx context.Context, t *testing.T, addr, sessionID string) zmq4.Socket {
	t.Helper()

	dealer := zmq4.NewDealer(ctx, zmq4.WithID(zmq4.SocketIdentity(sessionID)))
	if err := dealer.Dial(addr); err != nil {
		t.Fatalf("dealer.Dial(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = dealer.Close() })

	return dealer
}

// sendDealerRequest marshals and sends one request on the dealer socket.
func sendDealerRequest(t *testing.T, dealer zmq4.Socket, req protocol.Request) {
	t.Helper()

	data, err := protocol.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := dealer.Send(zmq4.NewMsg(data)); err != nil {
		t.Fatalf("dealer.Send: %v", err)
	}
}

// TestIntegration_HandshakeReportsModelStatus pins the llm-status-visibility
// wire-protocol scenario "Handshake reports active default": the successful
// authenticated session-start response carries the current model reference
// and state.
func TestIntegration_HandshakeReportsModelStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const ref = "ollama-gemma::gemma4:31b"
	ch, addr, _ := newStatusTestChannel(ctx, t, ModelStatusEvent{Ref: ref, State: protocol.ModelStateDefault})
	_ = ch // closed via t.Cleanup in newIntegrationChannel

	dealer := dialStatusDealer(ctx, t, addr, "status-handshake-session")

	resp := doHandshake(t, dealer, "status-handshake-session", "secret-status")
	if resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("handshake status = %q, want ok", resp.Status)
	}

	if resp.ModelRef != ref {
		t.Errorf("handshake model_ref = %q, want %q", resp.ModelRef, ref)
	}

	if resp.ModelState != protocol.ModelStateDefault {
		t.Errorf("handshake model_state = %q, want %q", resp.ModelState, protocol.ModelStateDefault)
	}
}

// TestIntegration_HandshakeWithoutStatusOmitsFields verifies a channel with
// no wired status source omits the model fields entirely, preserving the
// pre-change wire shape.
func TestIntegration_HandshakeWithoutStatusOmitsFields(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr := newIntegrationChannel(ctx, t, "secret-nostatus", 0, zerolog.Nop())
	_ = ch

	dealer := dialStatusDealer(ctx, t, addr, "nostatus-session")

	resp := doHandshake(t, dealer, "nostatus-session", "secret-nostatus")
	if resp.ModelRef != "" || resp.ModelState != "" {
		t.Errorf("expected no model fields without a status source, got ref=%q state=%q", resp.ModelRef, resp.ModelState)
	}
}

// TestIntegration_ModelStatusTransitionBroadcast pins the wire-protocol
// scenario "Fallback promotion is pushed": every connected session receives
// an unsolicited control response with the updated model fields on each
// transition, both fallback promotion and unavailable.
func TestIntegration_ModelStatusTransitionBroadcast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, src := newStatusTestChannel(ctx, t, ModelStatusEvent{
		Ref:   "ollama-gemma::gemma4:31b",
		State: protocol.ModelStateDefault,
	})
	_ = ch // closed via t.Cleanup in newIntegrationChannel

	dealer := dialStatusDealer(ctx, t, addr, "status-transition-session")
	doHandshake(t, dealer, "status-transition-session", "secret-status")

	// Fallback promotion.
	src.push(ModelStatusEvent{Ref: "or-gemma::google/gemma-3-27b-it", State: protocol.ModelStateFallback})

	ev, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for fallback model-status event")
	}

	if ev.ResponseType != protocol.ResponseTypeControl {
		t.Errorf("event response_type = %q, want control", ev.ResponseType)
	}

	if ev.Status != protocol.ResponseStatusOK {
		t.Errorf("event status = %q, want ok", ev.Status)
	}

	if ev.ModelRef != "or-gemma::google/gemma-3-27b-it" || ev.ModelState != protocol.ModelStateFallback {
		t.Errorf("fallback event = ref %q state %q, want or-gemma ref and fallback", ev.ModelRef, ev.ModelState)
	}

	// Transition to unavailable.
	src.push(ModelStatusEvent{State: protocol.ModelStateUnavailable})

	ev2, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for unavailable model-status event")
	}

	if ev2.ModelState != protocol.ModelStateUnavailable {
		t.Errorf("unavailable event state = %q, want unavailable", ev2.ModelState)
	}

	if ev2.ModelRef != "" {
		t.Errorf("unavailable event ref = %q, want empty", ev2.ModelRef)
	}
}

// TestIntegration_LLMUnavailableReply_SessionSurvives pins the wire-protocol
// requirement "LLM-unavailable final reply": an exhausted-chain turn produces
// exactly one final reply with Status=error, ErrorCode=llm_unavailable, and
// the terminal message in Content — and the session stays registered and
// accepts a subsequent message request.
func TestIntegration_LLMUnavailableReply_SessionSurvives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newStatusTestChannel(ctx, t, ModelStatusEvent{Ref: "x::y", State: protocol.ModelStateDefault})

	dealer := dialStatusDealer(ctx, t, addr, "llm-unavailable-session")
	doHandshake(t, dealer, "llm-unavailable-session", "secret-status")

	const terminalMsg = "The AI service is temporarily unavailable. Please try again in a few minutes."

	driverDone := make(chan struct{})

	go func() {
		defer close(driverDone)

		// First turn: the dispatcher reports an exhausted chain via the
		// error_code metadata (processMessage sets it from
		// errors.Is(err, llm.ErrChainExhausted)).
		msg, err := ch.Receive(ctx)
		if err != nil {
			return
		}

		sessionID := msg.Metadata[ConnectionID]

		sendErr := ch.Send(ctx, channel.Message{
			Content: terminalMsg,
			Metadata: map[string]string{
				ConnectionID: sessionID,
				ErrorCode:    protocol.ErrorCodeLLMUnavailable,
			},
		})
		if sendErr != nil {
			t.Errorf("Send: %v", sendErr)
		}

		// Second turn on the SAME session proves the session survived the
		// llm_unavailable reply.
		msg2, err := ch.Receive(ctx)
		if err != nil {
			return
		}

		if err := ch.Send(ctx, channel.Message{
			Content:  "echo: " + msg2.Content,
			Metadata: map[string]string{ConnectionID: sessionID},
		}); err != nil {
			t.Errorf("second Send: %v", err)
		}
	}()

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "llm-unavailable-session",
		Type:         protocol.RequestTypeMessage,
		Content:      "first message",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	})

	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for llm_unavailable reply")
	}

	if reply.ResponseType != protocol.ResponseTypeReply {
		t.Fatalf("llm_unavailable delivery response_type = %q, want reply", reply.ResponseType)
	}

	if reply.Status != protocol.ResponseStatusError {
		t.Fatalf("llm_unavailable reply status = %q, want error", reply.Status)
	}

	if reply.ErrorCode != protocol.ErrorCodeLLMUnavailable {
		t.Fatalf("llm_unavailable reply error_code = %q, want %q", reply.ErrorCode, protocol.ErrorCodeLLMUnavailable)
	}

	if reply.Content != terminalMsg {
		t.Fatalf("llm_unavailable reply content = %q, want the terminal message", reply.Content)
	}

	// The session must still be registered: a subsequent message gets a
	// normal reply.
	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "llm-unavailable-session",
		Type:         protocol.RequestTypeMessage,
		Content:      "second message",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	})

	reply2, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for post-outage reply — session did not survive")
	}

	if reply2.Status != protocol.ResponseStatusOK {
		t.Fatalf("post-outage reply status = %q, want ok", reply2.Status)
	}

	if reply2.Content != "echo: second message" {
		t.Fatalf("post-outage reply content = %q, want echoed message", reply2.Content)
	}

	// No further deliveries: each pending slot was consumed exactly once.
	if extra, ok := recvDealerResponse(t, dealer, 750*time.Millisecond); ok {
		t.Fatalf("expected no extra delivery after the two replies, got %+v", extra)
	}

	select {
	case <-driverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server-side driver did not finish")
	}
}

// TestIntegration_GenericFailureKeepsOKStatus pins the wire-protocol scenario
// "A non-exhaustion failure is not reported as llm_unavailable": a Send
// without the error_code metadata (any failure that does not satisfy
// errors.Is(err, llm.ErrChainExhausted)) produces a plain Status=ok reply.
func TestIntegration_GenericFailureKeepsOKStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, addr, _ := newStatusTestChannel(ctx, t, ModelStatusEvent{Ref: "x::y", State: protocol.ModelStateDefault})

	dealer := dialStatusDealer(ctx, t, addr, "generic-failure-session")
	doHandshake(t, dealer, "generic-failure-session", "secret-status")

	const terminalMsg = "The response could not be completed. Some actions may already have run; check the current state before repeating the request."

	driverDone := make(chan struct{})

	go func() {
		defer close(driverDone)

		msg, err := ch.Receive(ctx)
		if err != nil {
			return
		}

		// No ErrorCode metadata: the failure is not an exhausted chain.
		if err := ch.Send(ctx, channel.Message{
			Content:  terminalMsg,
			Metadata: map[string]string{ConnectionID: msg.Metadata[ConnectionID]},
		}); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()

	sendDealerRequest(t, dealer, protocol.Request{
		ConnectionID: "generic-failure-session",
		Type:         protocol.RequestTypeMessage,
		Content:      "generic failure message",
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
	})

	reply, ok := recvDealerResponse(t, dealer, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for reply")
	}

	if reply.Status != protocol.ResponseStatusOK {
		t.Fatalf("generic-failure reply status = %q, want ok (no llm_unavailable)", reply.Status)
	}

	if reply.ErrorCode != "" {
		t.Fatalf("generic-failure reply error_code = %q, want empty", reply.ErrorCode)
	}

	select {
	case <-driverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server-side driver did not finish")
	}
}

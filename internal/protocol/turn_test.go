package protocol

import (
	"errors"
	"testing"
)

func ptr(n int) *int { return &n }

func marshalValidate(t *testing.T, r Response) error {
	t.Helper()

	data, err := Marshal(r)
	if err != nil {
		return err
	}

	_, err = UnmarshalResponse(data)

	return err
}

func TestTurnObject_RoundTripAndStrictness(t *testing.T) {
	good := []Response{
		{
			ConnectionID: "c", Status: ResponseStatusOK, RequestID: "r1", ResponseType: ResponseTypeControl, Content: ControlMessageAccepted,
			Turn: &TurnInfo{Disposition: DispositionSteer, TurnID: "0193-a"},
		},
		{
			ConnectionID: "c", Status: ResponseStatusOK, RequestID: "r1", ResponseType: ResponseTypeControl, Content: ControlMessageAccepted,
			Turn: &TurnInfo{Disposition: DispositionQueued},
		},
		{
			ConnectionID: "c", Status: ResponseStatusOK, RequestID: "r1", ResponseType: ResponseTypeControl, Content: ControlTurnStarted,
			Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r1", "r2"}, Deferred: true},
		},
		{
			ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlSteerInjected,
			Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r2"}, SupersededToolCalls: ptr(0)},
		},
		{
			ConnectionID: "c", Status: ResponseStatusOK, RequestID: "r1", ResponseType: ResponseTypeReply, Content: "ok",
			Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r1", "r4"}, DroppedRequestIDs: []string{"r2"}},
		},
	}

	for i, r := range good {
		if err := marshalValidate(t, r); err != nil {
			t.Errorf("good[%d]: %v", i, err)
		}
	}

	// An explicit zero survives the round trip; absence stays absent.
	data, _ := Marshal(good[3])
	got, _ := UnmarshalResponse(data)

	if got.Turn.SupersededToolCalls == nil || *got.Turn.SupersededToolCalls != 0 {
		t.Fatal("an explicit superseded_tool_calls: 0 must be distinguishable from absent")
	}

	data, _ = Marshal(good[2])
	got, _ = UnmarshalResponse(data)

	if got.Turn.SupersededToolCalls != nil || !got.Turn.Deferred || len(got.Turn.RequestIDs) != 2 {
		t.Fatalf("turn_started round trip = %+v", got.Turn)
	}
}

func TestTurnObject_RejectsDisallowedCombinations(t *testing.T) {
	bad := map[string]Response{
		"accepted without disposition": {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlMessageAccepted, Turn: &TurnInfo{TurnID: "t"}},
		"accepted with request_ids":    {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlMessageAccepted, Turn: &TurnInfo{Disposition: DispositionQueued, RequestIDs: []string{"r"}}},
		"started without ids":          {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlTurnStarted, Turn: &TurnInfo{TurnID: "t"}},
		"started with disposition":     {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlTurnStarted, Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r"}, Disposition: DispositionQueued}},
		"injected with deferred":       {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlSteerInjected, Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r"}, Deferred: true}},
		"injected negative superseded": {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlSteerInjected, Turn: &TurnInfo{TurnID: "t", RequestIDs: []string{"r"}, SupersededToolCalls: ptr(-1)}},
		"reply with disposition":       {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeReply, Content: "x", Turn: &TurnInfo{Disposition: DispositionSteer}},
		"reply with superseded":        {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeReply, Content: "x", Turn: &TurnInfo{SupersededToolCalls: ptr(1)}},
		"progress with turn":           {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeProgress, Content: "x", Turn: &TurnInfo{TurnID: "t"}},
		"other control with turn":      {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlCompacted, Turn: &TurnInfo{TurnID: "t"}},
	}

	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			err := marshalValidate(t, r)
			if !errors.Is(err, ErrMalformedResponse) {
				t.Fatalf("err = %v, want ErrMalformedResponse", err)
			}
		})
	}
}

func TestTurnObject_UnknownFieldRejected(t *testing.T) {
	raw := `{"connection_id":"c","status":"ok","response_type":"reply","content":"x","turn":{"turn_id":"t","mystery":1}}`
	if _, err := UnmarshalResponse([]byte(raw)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("unknown field inside turn: err = %v", err)
	}
}

func TestMessageRequestRequiresRequestID(t *testing.T) {
	req := Request{ConnectionID: "c", Type: RequestTypeMessage, Timestamp: "2026-11-23T10:00:00Z", Content: "hi"}
	if err := req.Validate(); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("message without request_id: err = %v", err)
	}

	req.RequestID = "r1"
	if err := req.Validate(); err != nil {
		t.Fatalf("message with request_id: %v", err)
	}
}

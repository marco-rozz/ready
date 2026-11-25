package protocol

import (
	"errors"
	"testing"
	"time"
)

func noticeEvent() Response {
	return Response{
		ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlNotice,
		Notice: &NoticeInfo{Text: "Report 1 ready"}, Turn: &TurnInfo{TurnID: "turn-notice-1"},
	}
}

func TestNoticeEvent_RoundTrip(t *testing.T) {
	data, err := Marshal(noticeEvent())
	if err != nil {
		t.Fatal(err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("valid notice event refused: %v", err)
	}

	if got.Notice == nil || got.Notice.Text != "Report 1 ready" || got.Turn == nil || got.Turn.TurnID != "turn-notice-1" {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	if _, err = UnmarshalResponse([]byte(`{"connection_id":"c","status":"ok","response_type":"control","content":"notice","notice":{"text":"x","extra":1},"turn":{"turn_id":"t"}}`)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("unknown field inside notice must be rejected, got %v", err)
	}
}

func TestNoticeEvent_Strictness(t *testing.T) {
	mut := func(f func(*Response)) Response {
		r := noticeEvent()
		f(&r)

		return r
	}

	bad := map[string]Response{
		"empty text":          mut(func(r *Response) { r.Notice.Text = "" }),
		"event without body":  mut(func(r *Response) { r.Notice = nil }),
		"turn with requests":  mut(func(r *Response) { r.Turn.RequestIDs = []string{"r1"} }),
		"turn without id":     mut(func(r *Response) { r.Turn.TurnID = "" }),
		"turn with deferred":  mut(func(r *Response) { r.Turn.Deferred = true }),
		"carries request id":  mut(func(r *Response) { r.RequestID = "r1" }),
		"carries usage":       mut(func(r *Response) { r.Usage = &UsageInfo{} }),
		"carries error code":  mut(func(r *Response) { r.ErrorCode = "x" }),
		"error status":        mut(func(r *Response) { r.Status = ResponseStatusError }),
		"object on a reply":   {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeReply, Content: "hi", RequestID: "r1", Notice: &NoticeInfo{Text: "x"}},
		"object on a control": {ConnectionID: "c", Status: ResponseStatusOK, ResponseType: ResponseTypeControl, Content: ControlCompacted, Notice: &NoticeInfo{Text: "x"}},
	}

	for name, r := range bad {
		if err := marshalValidate(t, r); !errors.Is(err, ErrMalformedResponse) {
			t.Errorf("%s: want ErrMalformedResponse, got %v", name, err)
		}
	}
}

func TestRendersNotices_SessionStartOnly(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)

	for _, typ := range []RequestType{RequestTypeSessionStart} {
		req := Request{ConnectionID: "c", Type: typ, Timestamp: now, ProtocolVersion: Version, RendersNotices: true}
		data, err := Marshal(req)
		if err != nil {
			t.Fatal(err)
		}

		got, err := UnmarshalRequest(data)
		if err != nil || !got.RendersNotices {
			t.Fatalf("session_start with renders_notices must round trip: %v %+v", err, got)
		}
	}

	req := Request{ConnectionID: "c", Type: RequestTypeMessage, Timestamp: now, RequestID: "r1", Content: "hi", RendersNotices: true}
	if err := req.Validate(); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("renders_notices on message must be refused, got %v", err)
	}
}

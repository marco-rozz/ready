package protocol

import (
	"errors"
	"strings"
	"testing"
)

func TestActivityStrictValidation(t *testing.T) {
	tests := []struct {
		name, data string
		valid      bool
	}{
		{"thinking", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":1}}`, true},
		{"tool", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"using_tool","sequence":2,"tool_name":"test","additional_tools":0}}`, true},
		{"missing count", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"using_tool","sequence":2,"tool_name":"test"}}`, false},
		{"negative", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"using_tool","sequence":2,"tool_name":"test","additional_tools":-1}}`, false},
		{"unknown", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":1,"secret":"value"}}`, false},
		{"thinking tool field", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":1,"tool_name":"test"}}`, false},
		{"thinking explicit empty", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":1,"tool_name":""}}`, false},
		{"thinking null count", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":1,"additional_tools":null}}`, false},
		{"zero sequence", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"thinking","sequence":0}}`, false},
		{"unknown state", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","activity":{"state":"secret_value","sequence":1}}`, false},
		{"content", `{"connection_id":"c","status":"ok","request_id":"r","response_type":"control","content":"secret_value","activity":{"state":"thinking","sequence":1}}`, false},
		{"no request", `{"connection_id":"c","status":"ok","response_type":"control","activity":{"state":"thinking","sequence":1}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := UnmarshalResponse([]byte(tt.data))
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				b, marshalErr := Marshal(r)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, err = UnmarshalResponse(b); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, ErrMalformedResponse) {
					t.Fatalf("expected malformed response, got %v", err)
				}
				if strings.Contains(err.Error(), "secret_value") {
					t.Fatal("validation leaked payload")
				}
			}
		})
	}
}

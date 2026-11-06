// Protocol-version handshake tests (guided-init C.19/C.20): field rules at
// the wire layer and the daemon's gate order (mismatch beats challenge, no
// pending entry retained), plus exact round-trips and absence outside
// session_start.
package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func versionedSessionStart() Request {
	return Request{
		ConnectionID:    "c1",
		Type:            RequestTypeSessionStart,
		Timestamp:       "2026-10-25T10:00:00Z",
		ProtocolVersion: Version,
	}
}

func TestRequestVersionFieldRules(t *testing.T) {
	t.Run("missing session_start version reaches the daemon gate", func(t *testing.T) {
		req := versionedSessionStart()
		if err := req.Validate(); err != nil {
			t.Fatalf("Validate(named version): %v", err)
		}

		req.ProtocolVersion = 0
		if err := req.Validate(); err != nil {
			t.Fatalf("missing version must reach the daemon gate: %v", err)
		}
	})

	t.Run("mismatched session_start round-trips and is the daemon's refusal basis", func(t *testing.T) {
		req := versionedSessionStart()
		req.ProtocolVersion = Version + 7
		data, err := Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalRequest(data)
		if err != nil || got.ProtocolVersion != Version+7 {
			t.Fatalf("round-trip lost the declared version: %v / %v", err, got)
		}
	})

	t.Run("non-session_start types reject the field", func(t *testing.T) {
		for _, typ := range []RequestType{RequestTypeMessage, RequestTypePing, RequestTypeSessionEnd} {
			req := Request{
				ConnectionID:    "c1",
				Type:            typ,
				Timestamp:       "2026-10-25T10:00:00Z",
				ProtocolVersion: Version,
			}
			if err := req.Validate(); err == nil || !strings.Contains(err.Error(), "session_start only") {
				t.Fatalf("type %s accepted protocol_version: %v", typ, err)
			}
		}
	})
}

func TestUnknownNestedFieldsRejectedStillHolds(t *testing.T) {
	raw := `{"connection_id":"c","type":"session_start","timestamp":"2026-10-25T10:00:00Z","protocol_version":1,"extra":"x"}`
	if _, err := UnmarshalRequest([]byte(raw)); err == nil {
		t.Fatal("unknown top-level field accepted")
	}
}

func TestVersionResponseFieldRoundTrip(t *testing.T) {
	resp := Response{
		ConnectionID:    "c1",
		Status:          ResponseStatusOK,
		ProtocolVersion: Version,
	}
	data, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"protocol_version"`) {
		t.Fatalf("marshalled response lost protocol_version: %s", data)
	}
	got, err := UnmarshalResponse(data)
	if err != nil || got.ProtocolVersion != Version {
		t.Fatalf("response round-trip: %v / %v", err, got)
	}

	// The omitted form stays decodable (a legacy, non-versioning response).
	var empty Response
	if err = json.Unmarshal([]byte(`{"connection_id":"c","status":"ok"}`), &empty); err != nil {
		t.Fatalf("payload without protocol_version must decode: %v", err)
	}
}

func TestErrorCodeConstants(t *testing.T) {
	if Version != 3 {
		t.Fatalf("oauth_login and runtime_reload are protocol 3, got %d", Version)
	}
	if ErrorCodeProtocolMismatch != "protocol_mismatch" {
		t.Fatalf("unexpected mismatch code %q", ErrorCodeProtocolMismatch)
	}
	if !strings.Contains(PredatesVersioningHint, "predates") {
		t.Fatalf("legacy hint text = %q", PredatesVersioningHint)
	}
}

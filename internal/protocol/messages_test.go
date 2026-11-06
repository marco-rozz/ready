//nolint:goconst
package protocol

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	in := Request{
		ConnectionID: "s1",
		Type:         RequestTypeMessage,
		Timestamp:    "2026-05-21T10:00:00Z",
		RequestID:    "r1",
		AuthToken:    "tok",
		Content:      "hello",
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	got, err := UnmarshalRequest(data)
	if err != nil {
		t.Fatalf("UnmarshalRequest returned error: %v", err)
	}

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	in := Response{
		ConnectionID: "s1",
		Status:       ResponseStatusOK,
		RequestID:    "r1",
		ResponseType: ResponseTypeReply,
		Content:      "world",
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalResponse returned error: %v", err)
	}

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

// TestResponseModelStatusRoundTrip pins the llm-status-visibility scenario
// "Model-status response round-trip": a control response carrying the
// model fields survives a marshal/unmarshal cycle with every field intact.
func TestResponseModelStatusRoundTrip(t *testing.T) {
	in := Response{
		ConnectionID: "s1",
		Status:       ResponseStatusOK,
		ResponseType: ResponseTypeControl,
		ModelRef:     "or-gemma::google/gemma-3-27b-it",
		ModelState:   ModelStateFallback,
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalResponse returned error: %v", err)
	}

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

// TestResponseValidate_AcceptsAllowedModelStates covers the three allowed
// model_state values plus the omitted (empty) case.
func TestResponseValidate_AcceptsAllowedModelStates(t *testing.T) {
	for _, state := range []ModelState{"", ModelStateDefault, ModelStateFallback, ModelStateUnavailable} {
		t.Run(string(state), func(t *testing.T) {
			resp := Response{ConnectionID: "s", Status: ResponseStatusOK, ModelState: state}
			if err := resp.Validate(); err != nil {
				t.Fatalf("Validate returned error for model_state %q: %v", state, err)
			}
		})
	}
}

// TestResponseValidate_RejectsUnknownModelState covers both entry points:
// direct Validate and UnmarshalResponse (which calls Validate after the
// strict decode).
func TestResponseValidate_RejectsUnknownModelState(t *testing.T) {
	resp := Response{ConnectionID: "s", Status: ResponseStatusOK, ModelState: ModelState("degraded")}

	err := resp.Validate()
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse, got %v", err)
	}

	if !strings.Contains(err.Error(), "degraded") {
		t.Errorf("error message should name the offending model_state 'degraded': %v", err)
	}

	raw := []byte(`{"connection_id":"s","status":"ok","model_state":"degraded"}`)
	if _, unmarshalErr := UnmarshalResponse(raw); !errors.Is(unmarshalErr, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse from UnmarshalResponse, got %v", unmarshalErr)
	}
}

// TestResponseValidate_AcceptsLLMUnavailable pins the llm-status-visibility
// wire-protocol scenario "LLM unavailable is a valid structured error": a
// final reply with Status=error and ErrorCode=llm_unavailable passes
// validation (both via Validate and the strict unmarshal path).
func TestResponseValidate_AcceptsLLMUnavailable(t *testing.T) {
	in := Response{
		ConnectionID: "s1",
		Status:       ResponseStatusError,
		RequestID:    "r1",
		ResponseType: ResponseTypeReply,
		Content:      "The AI service is temporarily unavailable. Please try again in a few minutes.",
		ErrorCode:    ErrorCodeLLMUnavailable,
	}

	if err := in.Validate(); err != nil {
		t.Fatalf("Validate rejected the llm_unavailable reply: %v", err)
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalResponse returned error: %v", err)
	}

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, in)
	}
}

func TestUnmarshalRequestRejectsUnknownField(t *testing.T) {
	raw := []byte(`{"connection_id":"s","type":"message","timestamp":"2026-05-21T10:00:00Z","extra":"x"}`)

	_, err := UnmarshalRequest(raw)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}

	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected errors.Is(err, ErrMalformedRequest); got %v", err)
	}
}

func TestUnmarshalResponseRejectsUnknownField(t *testing.T) {
	raw := []byte(`{"connection_id":"s","status":"ok","mystery":1}`)

	_, err := UnmarshalResponse(raw)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}

	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected errors.Is(err, ErrMalformedResponse); got %v", err)
	}
}

// TestResponseUsageRoundTrip pins the tui-usage-observability scenario
// "Round-trip preserves all fields": a reply carrying the usage block
// survives a marshal/unmarshal cycle field-by-field.
func TestResponseUsageRoundTrip(t *testing.T) {
	in := Response{
		ConnectionID: "s1",
		Status:       ResponseStatusOK,
		RequestID:    "r1",
		ResponseType: ResponseTypeReply,
		Content:      "world",
		ModelRef:     "ollama-gemma::gemma4:31b",
		ModelState:   ModelStateDefault,
		Usage: &UsageInfo{
			ReadTokens:    68000,
			WrittenTokens: 5400,
			CachedTokens:  38000,
			TurnCount:     12,
			LLMCalls:      61,
			AuxCalls:      3,
		},
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalResponse returned error: %v", err)
	}

	if got.ConnectionID != in.ConnectionID || got.Status != in.Status ||
		got.RequestID != in.RequestID || got.ResponseType != in.ResponseType ||
		got.Content != in.Content || got.ModelRef != in.ModelRef ||
		got.ModelState != in.ModelState {
		t.Fatalf("round-trip scalar mismatch: got %+v want %+v", got, in)
	}

	if got.Usage == nil {
		t.Fatal("round-trip lost the usage block")
	}
	if *got.Usage != *in.Usage {
		t.Fatalf("usage round-trip mismatch: got %+v want %+v", *got.Usage, *in.Usage)
	}

	// omitempty: cached_tokens is dropped when zero.
	in.Usage.CachedTokens = 0
	data, _ = Marshal(in)
	if strings.Contains(string(data), "cached_tokens") {
		t.Errorf("zero cached_tokens must be omitted, got %s", data)
	}
}

// TestResponseWithoutUsageValidates pins the "Pre-usage payload still
// validates" scenario: a response without the usage field decodes cleanly
// and Usage stays nil.
func TestResponseWithoutUsageValidates(t *testing.T) {
	raw := []byte(`{"connection_id":"s","status":"ok","response_type":"reply","content":"hello"}`)

	got, err := UnmarshalResponse(raw)
	if err != nil {
		t.Fatalf("UnmarshalResponse returned error: %v", err)
	}

	if got.Usage != nil {
		t.Errorf("Usage must be nil on a pre-usage payload, got %+v", got.Usage)
	}
}

// TestUsageInfoOmittedWhenNil pins that a nil Usage pointer serialises as a
// response with no usage key at all.
func TestUsageInfoOmittedWhenNil(t *testing.T) {
	data, err := Marshal(Response{ConnectionID: "s", Status: ResponseStatusOK})
	if err != nil {
		t.Fatalf("Marshal returned error: %v", err)
	}

	if strings.Contains(string(data), "usage") {
		t.Errorf("nil Usage must be omitted, got %s", data)
	}
}

func TestRequestValidate_AcceptsAllowedTypes(t *testing.T) {
	for _, typ := range []RequestType{
		RequestTypeSessionStart,
		RequestTypeSessionEnd,
		RequestTypeMessage,
		RequestTypeExec,
		RequestTypeSessionInfo,
		RequestTypeContextUsage,
		RequestTypePing,
		RequestTypeRuntimeReload,
	} {
		t.Run(string(typ), func(t *testing.T) {
			req := Request{
				ConnectionID: "s",
				Type:         typ,
				Timestamp:    "2026-05-21T10:00:00Z",
			}
			if typ == RequestTypeSessionStart {
				// guided-init C.19: session_start carries the wire version.
				req.ProtocolVersion = Version
			}
			if typ == RequestTypeRuntimeReload {
				req.RequestID = "r1"
			}
			if err := req.Validate(); err != nil {
				t.Fatalf("Validate returned error for allowed type %q: %v", typ, err)
			}
		})
	}
}

func TestOAuthLoginRequestValidationAndRoundTrip(t *testing.T) {
	code := "http://localhost:1455/auth/callback?code=abc&state=xyz"
	cases := []struct {
		name string
		p    *OAuthLoginRequest
		ok   bool
	}{
		{"begin llm", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "llm:codex-work"}, true},
		{"begin mcp", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "mcp:ha"}, true},
		{"submit", &OAuthLoginRequest{Action: OAuthLoginActionSubmit, OperationID: "op1", Target: "llm:codex-work", Value: &code}, true},
		{"cancel", &OAuthLoginRequest{Action: OAuthLoginActionCancel, OperationID: "op1", Target: "llm:codex-work"}, true},
		{"begin missing target", &OAuthLoginRequest{Action: OAuthLoginActionBegin}, false},
		{"begin invalid target prefix", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "other:x"}, false},
		{"begin empty target name", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "llm:"}, false},
		{"begin with operation_id", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "llm:x", OperationID: "op1"}, false},
		{"begin with value", &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "llm:x", Value: &code}, false},
		{"submit missing operation_id", &OAuthLoginRequest{Action: OAuthLoginActionSubmit, Target: "llm:x", Value: &code}, false},
		{"submit missing value", &OAuthLoginRequest{Action: OAuthLoginActionSubmit, OperationID: "op1", Target: "llm:x"}, false},
		{"cancel missing operation_id", &OAuthLoginRequest{Action: OAuthLoginActionCancel, Target: "llm:x"}, false},
		{"cancel with value", &OAuthLoginRequest{Action: OAuthLoginActionCancel, OperationID: "op1", Target: "llm:x", Value: &code}, false},
		{"unknown action", &OAuthLoginRequest{Action: "list", Target: "llm:x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{
				ConnectionID: "c",
				Type:         RequestTypeOAuthLogin,
				Timestamp:    "2026-05-21T10:00:00Z",
				RequestID:    "r",
				OAuthLogin:   tc.p,
			}
			err := req.Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate error = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok {
				b, merr := Marshal(req)
				if merr != nil {
					t.Fatal(merr)
				}
				got, uerr := UnmarshalRequest(b)
				if uerr != nil || !reflect.DeepEqual(got, req) {
					t.Fatalf("round trip = %+v, err = %v", got, uerr)
				}
			}
		})
	}

	// oauth_login payload on conversational request is rejected.
	badMsg := Request{
		ConnectionID: "c",
		Type:         RequestTypeMessage,
		Timestamp:    "2026-05-21T10:00:00Z",
		RequestID:    "r",
		Content:      "hi",
		OAuthLogin:   &OAuthLoginRequest{Action: OAuthLoginActionBegin, Target: "llm:x"},
	}
	if err := badMsg.Validate(); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("oauth_login on message accepted: %v", err)
	}

	// Unknown nested field in oauth_login is rejected.
	if _, err := UnmarshalRequest([]byte(`{"connection_id":"c","type":"oauth_login","timestamp":"2026-05-21T10:00:00Z","request_id":"r","oauth_login":{"action":"begin","target":"llm:x","extra":"bad"}}`)); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("unknown nested field accepted: %v", err)
	}
}

func TestRuntimeReloadAndConfigSavedRoundTrip(t *testing.T) {
	// runtime_reload requires request_id and forbids content/auth_token.
	for _, bad := range []Request{
		{ConnectionID: "c", Type: RequestTypeRuntimeReload, Timestamp: "2026-05-21T10:00:00Z"},
		{ConnectionID: "c", Type: RequestTypeRuntimeReload, Timestamp: "2026-05-21T10:00:00Z", RequestID: "r", Content: "x"},
		{ConnectionID: "c", Type: RequestTypeRuntimeReload, Timestamp: "2026-05-21T10:00:00Z", RequestID: "r", AuthToken: "t"},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrMalformedRequest) {
			t.Fatalf("invalid runtime_reload accepted: %+v (%v)", bad, err)
		}
	}

	resp := Response{
		ConnectionID: "c1",
		Status:       ResponseStatusOK,
		RequestID:    "r1",
		ResponseType: ResponseTypeReply,
		Content:      "saved",
		ConfigSaved:  true,
		OAuthLogin: &OAuthLoginResponse{
			Action:        OAuthLoginActionBegin,
			OperationID:   "op1",
			Target:        "llm:codex-work",
			Outcome:       OAuthLoginOutcomeWaiting,
			AuthURL:       "https://auth.openai.com/authorize?x=1",
			AcceptsManual: true,
		},
		Secrets:     []PendingSecret{{Name: "p_api_key", State: SecretStatePending}},
		Credentials: []CredentialInfo{{Target: "llm:codex-work", State: "login_required"}},
	}
	b, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalResponse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, resp)
	}

	// Unknown oauth_login outcome or nested field in response is rejected.
	if _, err := UnmarshalResponse([]byte(`{"connection_id":"c","status":"ok","oauth_login":{"outcome":"bogus"}}`)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("unknown oauth_login outcome accepted: %v", err)
	}
	if _, err := UnmarshalResponse([]byte(`{"connection_id":"c","status":"ok","oauth_login":{"outcome":"waiting","value":"secret"}}`)); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("unknown oauth_login field accepted: %v", err)
	}
}

func TestSecretEntryRequestValidation(t *testing.T) {
	empty := ""
	cases := []struct {
		name string
		p    *SecretEntryRequest
		ok   bool
	}{
		{"list", &SecretEntryRequest{Action: "list"}, true},
		{"begin", &SecretEntryRequest{Action: "begin"}, true},
		{"begin rotation", &SecretEntryRequest{Action: "begin", Name: "openai_api_key"}, true},
		{"submit empty value", &SecretEntryRequest{Action: "submit", OperationID: "o", Name: "n", Value: &empty}, true},
		{"delete", &SecretEntryRequest{Action: "delete", Name: "n"}, true},
		{"end", &SecretEntryRequest{Action: "end", OperationID: "o"}, true},
		{"list with a name", &SecretEntryRequest{Action: "list", Name: "n"}, false},
		{"submit missing value", &SecretEntryRequest{Action: "submit", OperationID: "o", Name: "n"}, false},
		{"submit missing name", &SecretEntryRequest{Action: "submit", OperationID: "o", Value: &empty}, false},
		{"delete without name", &SecretEntryRequest{Action: "delete"}, false},
		{"delete with a value", &SecretEntryRequest{Action: "delete", Name: "n", Value: &empty}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (Request{ConnectionID: "c", Type: RequestTypeSecretEntry, Timestamp: "2026-05-21T10:00:00Z", RequestID: "r", SecretEntry: tc.p}).Validate()
			if (err == nil) != tc.ok {
				t.Fatalf("Validate error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestSecretEntryStrictNestedDecoding(t *testing.T) {
	_, err := UnmarshalRequest([]byte(`{"connection_id":"c","type":"secret_entry","timestamp":"2026-05-21T10:00:00Z","request_id":"r","secret_entry":{"action":"list","unexpected":"x"}}`))
	if err == nil || !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("unknown nested field error = %v, want malformed request", err)
	}

	value := "literal"
	b, err := Marshal(Request{ConnectionID: "c", Type: RequestTypeSecretEntry, Timestamp: "2026-05-21T10:00:00Z", RequestID: "r", SecretEntry: &SecretEntryRequest{Action: "submit", OperationID: "o", Name: "n", Value: &value}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalRequest(b)
	if err != nil || got.SecretEntry == nil || got.SecretEntry.Value == nil || *got.SecretEntry.Value != value {
		t.Fatalf("literal round trip = %#v, err %v", got, err)
	}
}

func TestRequestValidate_RejectsHeartbeat(t *testing.T) {
	req := Request{
		ConnectionID: "s",
		Type:         RequestType("heartbeat"),
		Timestamp:    "2026-05-21T10:00:00Z",
	}

	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for heartbeat type, got nil")
	}

	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected errors.Is(err, ErrMalformedRequest); got %v", err)
	}

	if !strings.Contains(err.Error(), "unknown request type") {
		t.Errorf("expected safe type diagnostic: %v", err)
	}
}

func TestRequestValidate_RejectsEmptyType(t *testing.T) {
	req := Request{
		ConnectionID: "s",
		Type:         "",
		Timestamp:    "2026-05-21T10:00:00Z",
	}

	err := req.Validate()
	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected ErrMalformedRequest, got %v", err)
	}
}

func TestRequestValidate_RejectsEmptyConnectionID(t *testing.T) {
	req := Request{
		ConnectionID: "",
		Type:         RequestTypeSessionStart,
		Timestamp:    "2026-05-21T10:00:00Z",
	}

	err := req.Validate()
	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected ErrMalformedRequest, got %v", err)
	}
}

func TestRequestValidate_RejectsMalformedTimestamp(t *testing.T) {
	req := Request{
		ConnectionID: "s",
		Type:         RequestTypeMessage,
		Timestamp:    "not-a-date",
	}

	err := req.Validate()
	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected ErrMalformedRequest, got %v", err)
	}
}

func TestResponseValidate_AcceptsAllowedStatuses(t *testing.T) {
	for _, status := range []ResponseStatus{
		ResponseStatusOK,
		ResponseStatusError,
		ResponseStatusBusy,
		ResponseStatusAuthFail,
		ResponseStatusShutdown,
	} {
		t.Run(string(status), func(t *testing.T) {
			resp := Response{ConnectionID: "s", Status: status}
			if err := resp.Validate(); err != nil {
				t.Fatalf("Validate returned error for allowed status %q: %v", status, err)
			}
		})
	}
}

func TestResponseValidate_RejectsUnknownStatus(t *testing.T) {
	resp := Response{ConnectionID: "s", Status: ResponseStatus("partial")}

	err := resp.Validate()
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse, got %v", err)
	}
}

func TestResponseValidate_RejectsEmptyStatus(t *testing.T) {
	resp := Response{ConnectionID: "s", Status: ""}

	err := resp.Validate()
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse, got %v", err)
	}
}

func TestUnmarshalRequest_RejectsInvalidJSON(t *testing.T) {
	_, err := UnmarshalRequest([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}

	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected errors.Is(err, ErrMalformedRequest); got %v", err)
	}
}

func TestUnmarshalResponse_RejectsInvalidJSON(t *testing.T) {
	_, err := UnmarshalResponse([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}

	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected errors.Is(err, ErrMalformedResponse); got %v", err)
	}
}

func TestUnmarshalRequest_RejectsMissingFields(t *testing.T) {
	// Valid JSON but missing required connection_id.
	raw := []byte(`{"type":"message","timestamp":"2026-05-21T10:00:00Z"}`)

	_, err := UnmarshalRequest(raw)
	if !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected ErrMalformedRequest, got %v", err)
	}
}

// session-lifecycle D11: a request carrying the legacy session_id field
// instead of connection_id is rejected.
func TestUnmarshalRequest_RejectsLegacySessionID(t *testing.T) {
	raw := []byte(`{"session_id":"s","type":"message","timestamp":"2026-05-21T10:00:00Z"}`)

	if _, err := UnmarshalRequest(raw); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("expected ErrMalformedRequest, got %v", err)
	}
}

// The conversation Session ID and the compaction block round-trip.
func TestResponse_SessionAndCompactionRoundTrip(t *testing.T) {
	in := Response{
		ConnectionID: "c1",
		SessionID:    "0192b7a0-0000-7000-8000-000000000000",
		Status:       ResponseStatusOK,
		ResponseType: ResponseTypeControl,
		Content:      ControlCompacted,
		Compaction:   &CompactionInfo{TokensBefore: 142000, TokensAfter: 31000},
	}

	data, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, err := UnmarshalResponse(data)
	if err != nil {
		t.Fatalf("UnmarshalResponse: %v", err)
	}

	if got.SessionID != in.SessionID || got.ConnectionID != in.ConnectionID || got.Compaction == nil || *got.Compaction != *in.Compaction {
		t.Fatalf("round trip = %+v, want %+v", got, in)
	}

	if !strings.Contains(string(data), `"connection_id":"c1"`) || !strings.Contains(string(data), `"tokens_before":142000`) {
		t.Fatalf("unexpected wire form: %s", data)
	}
}

// session_id is omitted when there is no conversation Session to report.
func TestResponse_SessionIDOmittedWhenEmpty(t *testing.T) {
	data, err := Marshal(Response{ConnectionID: "c", Status: ResponseStatusOK})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if strings.Contains(string(data), "session_id") || strings.Contains(string(data), "compaction") {
		t.Fatalf("expected no session_id/compaction, got %s", data)
	}
}

// AgentDir carriage (per-user-install-agent-isolation): the authenticated
// handshake's daemon-authoritative directory round-trips; challenges, auth
// failures and non-handshake responses omit it.
func TestResponseAgentDir(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		resp := Response{ConnectionID: "c1", SessionID: "s", Status: ResponseStatusOK, AgentDir: "~/agents/casa"}
		data, err := Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		got, uerr := UnmarshalResponse(data)
		if uerr != nil {
			t.Fatal(uerr)
		}
		if got.AgentDir != "~/agents/casa" {
			t.Errorf("AgentDir = %q, want preserved", got.AgentDir)
		}
	})

	t.Run("absent stays absent", func(t *testing.T) {
		data, err := Marshal(Response{ConnectionID: "c", Status: ResponseStatusOK, ErrorCode: "challenge"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "agent_dir") {
			t.Errorf("challenge response carried agent_dir: %s", data)
		}
		got, uerr := UnmarshalResponse(data)
		if uerr != nil {
			t.Fatal(uerr)
		}
		if got.AgentDir != "" {
			t.Errorf("AgentDir = %q, want empty", got.AgentDir)
		}
	})
}

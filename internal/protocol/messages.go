// Package protocol defines the JSON wire-protocol messages exchanged between
// the home-agent daemon ("serve") and the CLI client ("chat").
//
// This package is transport-agnostic: it only describes the message structs,
// their validation rules, and a JSON codec that rejects unknown fields. The
// framing layer (length-prefixed bytes over ZMQ) is the responsibility of the
// transport packages that will be added in a follow-up change.
//
// The package depends only on the Go standard library by design — a clean
// import graph keeps the protocol surface stable and lets every transport
// or test harness reuse it without pulling in unrelated dependencies.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RequestType is the discriminator for Request messages. Only the values
// declared as constants below are accepted by Validate.
type RequestType string

// Allowed RequestType values. session_info and context_usage (tui-usage-
// observability) are TUI-reserved control requests: answered daemon-side,
// never routed to the model. ping (change zmq-session-liveness) is the client
// liveness heartbeat: the TUI sends it periodically while a session is active
// and the daemon answers ok without touching the agent or history — it
// supersedes the earlier decision A1 ("kernel TCP keepalive is sufficient"),
// which held only for graceful exits and left killed clients holding the
// session lock until the idle watchdog.
const (
	RequestTypeSessionStart RequestType = "session_start"
	RequestTypeSessionEnd   RequestType = "session_end"
	RequestTypeMessage      RequestType = "message"
	RequestTypeExec         RequestType = "exec"
	RequestTypeSessionInfo  RequestType = "session_info"
	RequestTypeContextUsage RequestType = "context_usage"
	RequestTypePing         RequestType = "ping"
)

// isAllowedRequestType reports whether t is one of the seven allowed
// RequestType values. A switch keeps the lookup table off the package globals
// and lets the compiler check exhaustiveness at the call site.
func isAllowedRequestType(t RequestType) bool {
	switch t {
	case RequestTypeSessionStart, RequestTypeSessionEnd, RequestTypeMessage,
		RequestTypeExec, RequestTypeSessionInfo, RequestTypeContextUsage,
		RequestTypePing:
		return true
	default:
		return false
	}
}

// ResponseStatus is the discriminator for Response messages.
type ResponseStatus string

// Allowed ResponseStatus values.
const (
	ResponseStatusOK       ResponseStatus = "ok"
	ResponseStatusError    ResponseStatus = "error"
	ResponseStatusBusy     ResponseStatus = "busy"
	ResponseStatusAuthFail ResponseStatus = "auth_fail"
	ResponseStatusShutdown ResponseStatus = "shutdown"
)

// ResponseType discriminates server-to-client messages allowing the DEALER
// client to distinguish replies from unsolicited push notifications.
type ResponseType string

// Allowed ResponseType values.
const (
	// ResponseTypeReply is the final answer to a client request.
	ResponseTypeReply ResponseType = "reply"
	// ResponseTypeProgress is an intermediate notification pushed during processing.
	ResponseTypeProgress ResponseType = "progress"
	// ResponseTypeControl is used for auth handshake and session lifecycle messages.
	ResponseTypeControl ResponseType = "control"
)

// ModelState is the daemon-global LLM model status reported in the
// authenticated handshake and pushed on orchestrator transitions
// (llm-status-visibility). It encodes priority, not identity: green default,
// yellow fallback, red unavailable are the client-side rendering contract.
type ModelState string

// Allowed ModelState values.
const (
	// ModelStateDefault: the configured llm.default candidate is active.
	ModelStateDefault ModelState = "default"
	// ModelStateFallback: a failover candidate is active.
	ModelStateFallback ModelState = "fallback"
	// ModelStateUnavailable: no candidate is healthy; the chain is exhausted.
	ModelStateUnavailable ModelState = "unavailable"
)

// isAllowedModelState reports whether s is one of the three allowed
// ModelState values. The empty value is valid (the field is optional and
// omitted by daemons that have nothing to report).
func isAllowedModelState(s ModelState) bool {
	switch s {
	case "", ModelStateDefault, ModelStateFallback, ModelStateUnavailable:
		return true
	default:
		return false
	}
}

// ErrorCodeLLMUnavailable is the error_code of a final reply whose LLM
// candidate chain was exhausted (the daemon matches errors.Is(err,
// llm.ErrChainExhausted) before using it). It represents a COMPLETED request
// that failed, not a session-lifecycle failure: the session stays open and
// the applicable terminal message travels in Content.
const ErrorCodeLLMUnavailable = "llm_unavailable"

// isAllowedResponseStatus reports whether s is one of the five allowed
// ResponseStatus values.
func isAllowedResponseStatus(s ResponseStatus) bool {
	switch s {
	case ResponseStatusOK, ResponseStatusError, ResponseStatusBusy, ResponseStatusAuthFail, ResponseStatusShutdown:
		return true
	default:
		return false
	}
}

// Sentinel errors returned by validation and unmarshal helpers. Callers should
// use errors.Is to test against these — concrete error sites wrap them with
// fmt.Errorf("...: %w", ...) to carry the human-readable cause.
var (
	// ErrMalformedRequest indicates that a Request value failed structural or
	// semantic validation (missing field, unknown enum value, malformed
	// timestamp, unknown JSON field, invalid JSON, …).
	ErrMalformedRequest = errors.New("malformed request")
	// ErrMalformedResponse indicates that a Response value failed structural
	// or semantic validation.
	ErrMalformedResponse = errors.New("malformed response")
)

// Request is the daemon-bound message sent by the CLI client.
//
// JSON field order and tag spelling are part of the wire contract — do not
// rename without bumping the protocol version. omitempty on AuthToken and
// Content keeps the JSON compact when those fields are not relevant for a
// given RequestType.
type Request struct {
	// ConnectionID is the client-minted Connection ID (one per handshake);
	// it keys the single-active lock and reply routing. It is NOT the
	// conversation Session ID (session-lifecycle D11).
	ConnectionID string      `json:"connection_id"`
	Type         RequestType `json:"type"`
	Timestamp    string      `json:"timestamp"`
	RequestID    string      `json:"request_id,omitempty"`
	AuthToken    string      `json:"auth_token,omitempty"`
	Content      string      `json:"content,omitempty"`
}

// UsageInfo carries a session's cumulative LLM token usage (the architecture decision records):
// every ReAct iteration, failover attempt, and auxiliary consumer call since
// daemon start, plus the completed-turn count. CachedTokens is omitted when
// the provider reports none (Ollama never does) — absence is information,
// zero would be a lie.
type UsageInfo struct {
	ReadTokens    int `json:"read_tokens"`
	WrittenTokens int `json:"written_tokens"`
	CachedTokens  int `json:"cached_tokens,omitempty"`
	// ContextOccupancyPct is computed daemon-side at reply-attach time (last
	// turn's real prompt tokens ÷ the active candidate's configured context
	// window, 0–100). Omitted when the window is not configured or no turn
	// has yet reported prompt tokens — the TUI renders it only when present.
	ContextOccupancyPct float64 `json:"context_occupancy_pct,omitempty"`
	TurnCount           int     `json:"turn_count"`
	LLMCalls            int     `json:"llm_calls"`
	AuxCalls            int     `json:"aux_calls"`
}

// Response is the client-bound message sent by the daemon.
//
// ModelRef and ModelState are optional model-status metadata: the successful
// authenticated handshake carries the current values, and unsolicited
// control events carry updated ones on every orchestrator promotion or
// transition to unavailable. Both are omitted when there is nothing to
// report (omitempty), so pre-llm-status-visibility payloads remain valid.
//
// Usage is the optional usage block (tui-usage-observability): attached to
// the final reply of every turn (success AND failure — a failed attempt
// still consumed tokens) and to the authenticated handshake as the
// reconnect seed. A progress notification never carries it. A nil pointer
// keeps pre-usage payloads valid (omitempty).
type Response struct {
	// ConnectionID echoes the request's Connection ID.
	ConnectionID string `json:"connection_id"`
	// SessionID is the UUIDv7 of the conversation Session the response
	// belongs to: present on the authenticated handshake ok response and on
	// session_changed events (session-lifecycle).
	SessionID    string         `json:"session_id,omitempty"`
	Status       ResponseStatus `json:"status"`
	RequestID    string         `json:"request_id,omitempty"`
	ResponseType ResponseType   `json:"response_type,omitempty"`
	Content      string         `json:"content,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
	ModelRef     string         `json:"model_ref,omitempty"`
	ModelState   ModelState     `json:"model_state,omitempty"`
	Usage        *UsageInfo     `json:"usage,omitempty"`
	// LivenessTimeout (change zmq-session-liveness) rides the authenticated
	// handshake ok response only: the client derives its ping interval
	// (timeout / 3) from it without a second config knob. Empty on every
	// other response (omitempty keeps pre-liveness payloads valid).
	LivenessTimeout string `json:"liveness_timeout,omitempty"`
	// Compaction rides the unsolicited `compacted` control event only
	// (session-lifecycle): the token estimates before and after.
	Compaction *CompactionInfo `json:"compaction,omitempty"`
}

// CompactionInfo carries a Compaction's estimated prompt size before and
// after, in tokens.
type CompactionInfo struct {
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
}

// Content values of the unsolicited Session lifecycle control events
// (Status=ok, ResponseType=control, no error_code). Neither consumes a
// pending reply slot.
const (
	// ControlSessionChanged announces a new conversation Session (carries
	// SessionID, no Usage).
	ControlSessionChanged = "session_changed"
	// ControlCompacted announces a completed Compaction (carries Compaction).
	ControlCompacted = "compacted"
)

// Validate checks structural and semantic invariants on a Request. It returns
// nil when r is well-formed, and an error wrapping ErrMalformedRequest
// otherwise.
//
// Rules enforced here (matching the wire-protocol spec):
//   - ConnectionID must be non-empty for every RequestType, including
//     session_start (the client mints the UUID before sending the first frame).
//   - Type must be one of the seven allowed values.
//   - Timestamp must be parseable as RFC3339.
func (r Request) Validate() error {
	if r.ConnectionID == "" {
		return fmt.Errorf("%w: connection_id is required", ErrMalformedRequest)
	}

	if r.Type == "" {
		return fmt.Errorf("%w: type is required", ErrMalformedRequest)
	}

	if !isAllowedRequestType(r.Type) {
		return fmt.Errorf("%w: unknown type %q", ErrMalformedRequest, string(r.Type))
	}

	if r.Timestamp == "" {
		return fmt.Errorf("%w: timestamp is required", ErrMalformedRequest)
	}

	if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
		return fmt.Errorf("%w: timestamp %q is not valid RFC3339: %w", ErrMalformedRequest, r.Timestamp, err)
	}

	return nil
}

// Validate checks structural and semantic invariants on a Response. It returns
// nil when r is well-formed, and an error wrapping ErrMalformedResponse
// otherwise.
func (r Response) Validate() error {
	if r.Status == "" {
		return fmt.Errorf("%w: status is required", ErrMalformedResponse)
	}

	if !isAllowedResponseStatus(r.Status) {
		return fmt.Errorf("%w: unknown status %q", ErrMalformedResponse, string(r.Status))
	}

	if !isAllowedModelState(r.ModelState) {
		return fmt.Errorf("%w: unknown model_state %q", ErrMalformedResponse, string(r.ModelState))
	}

	return nil
}

// Marshal encodes any wire-protocol message to JSON bytes. It is a thin
// wrapper around encoding/json.Marshal kept here so callers depend on this
// package's surface only.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("protocol marshal: %w", err)
	}

	return b, nil
}

// UnmarshalRequest decodes data into a Request, rejecting unknown JSON fields
// and calling Validate on the result. Any failure returns an error that
// satisfies errors.Is(err, ErrMalformedRequest).
func UnmarshalRequest(data []byte) (Request, error) {
	var req Request

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrMalformedRequest, err)
	}

	if err := req.Validate(); err != nil {
		return Request{}, err
	}

	return req, nil
}

// UnmarshalResponse decodes data into a Response, rejecting unknown JSON fields
// and calling Validate on the result. Any failure returns an error that
// satisfies errors.Is(err, ErrMalformedResponse).
func UnmarshalResponse(data []byte) (Response, error) {
	var resp Response

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}

	if err := resp.Validate(); err != nil {
		return Response{}, err
	}

	return resp, nil
}

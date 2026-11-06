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
	"io"
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
	RequestTypeSessionStart  RequestType = "session_start"
	RequestTypeSessionEnd    RequestType = "session_end"
	RequestTypeMessage       RequestType = "message"
	RequestTypeExec          RequestType = "exec"
	RequestTypeSessionInfo   RequestType = "session_info"
	RequestTypeContextUsage  RequestType = "context_usage"
	RequestTypePing          RequestType = "ping"
	RequestTypeSecretEntry   RequestType = "secret_entry"
	RequestTypeOAuthLogin    RequestType = "oauth_login"
	RequestTypeRuntimeReload RequestType = "runtime_reload"
)

// isAllowedRequestType reports whether t is one of the ten allowed
// RequestType values. A switch keeps the lookup table off the package globals
// and lets the compiler check exhaustiveness at the call site.
func isAllowedRequestType(t RequestType) bool {
	switch t {
	case RequestTypeSessionStart, RequestTypeSessionEnd, RequestTypeMessage,
		RequestTypeExec, RequestTypeSessionInfo, RequestTypeContextUsage,
		RequestTypePing, RequestTypeSecretEntry, RequestTypeOAuthLogin,
		RequestTypeRuntimeReload:
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

// ErrorCodeProtocolMismatch is the error_code of a session_start response
// whose declared protocol_version differs from the daemon's (guided-init
// D11a). No challenge, nonce, HMAC or session state is created for that
// identity; the client closes and prints both versions.
const ErrorCodeProtocolMismatch = "protocol_mismatch"

// Stable secret-entry vocabulary. These values are deliberately independent
// of human-facing content so clients can render safe guidance without parsing
// daemon prose.
const (
	ErrorCodeMalformedRequest   = "malformed_request"
	ErrorCodeNoActiveSession    = "no_active_session"
	ErrorCodeLoopbackRequired   = "loopback_required"
	ErrorCodeProfileRequired    = "profile_required"
	ErrorCodeNoPendingSecrets   = "no_pending_secrets" //nolint:gosec // stable protocol error code, not a credential
	ErrorCodeRequestInFlight    = "request_in_flight"
	ErrorCodeSecretEntryActive  = "secret_entry_active"
	ErrorCodeSecretEntryExpired = "secret_entry_expired"
	ErrorCodeInvalidValue       = "invalid_value" // a value rule failed; the rule code rides SecretEntryResponse.RuleCode
	ErrorCodeUnknownName        = "unknown_name"  // the Secret name is neither referenced nor stored
	ErrorCodeNameInUse          = "name_in_use"   // delete refused: a configuration field still references the name
	ErrorCodeWriteNotCommitted  = "write_not_committed"
)

type SecretEntryAction string

const (
	SecretEntryActionList   SecretEntryAction = "list"
	SecretEntryActionBegin  SecretEntryAction = "begin"
	SecretEntryActionSubmit SecretEntryAction = "submit"
	SecretEntryActionDelete SecretEntryAction = "delete"
	SecretEntryActionEnd    SecretEntryAction = "end"
)

type SecretEntryOutcome string

const (
	SecretEntryOutcomeListed    SecretEntryOutcome = "listed"
	SecretEntryOutcomeActive    SecretEntryOutcome = "active"
	SecretEntryOutcomeCommitted SecretEntryOutcome = "committed"
	SecretEntryOutcomeDeleted   SecretEntryOutcome = "deleted"
	SecretEntryOutcomeReleased  SecretEntryOutcome = "released"
	SecretEntryOutcomeRefused   SecretEntryOutcome = "refused"
)

type OAuthLoginAction string

const (
	OAuthLoginActionBegin  OAuthLoginAction = "begin"
	OAuthLoginActionSubmit OAuthLoginAction = "submit"
	OAuthLoginActionCancel OAuthLoginAction = "cancel"
)

type OAuthLoginOutcome string

const (
	OAuthLoginOutcomeWaiting     OAuthLoginOutcome = "waiting"
	OAuthLoginOutcomeCommitted   OAuthLoginOutcome = "committed"
	OAuthLoginOutcomeNotRequired OAuthLoginOutcome = "not_required"
	OAuthLoginOutcomeCancelled   OAuthLoginOutcome = "cancelled"
	OAuthLoginOutcomeRefused     OAuthLoginOutcome = "refused"
)

// Version is the ZMQ wire-protocol version (guided-init D11a). It is an
// integer independent of the software release: a release that changes no
// wire contract keeps this value (mixed releases still connect); a wire
// change a strict previous-version peer would reject or misinterpret bumps
// it. It applies to the ZMQ transport only; no other channel negotiates.
const Version = 3

// PredatesVersioningGuess is the client's message when a handshake response
// cannot carry a protocol version at all — an unmarshal refusal or an error
// reply without the field, i.e. a daemon from before protocol versioning.
const PredatesVersioningHint = "daemon predates protocol versioning"

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
	// ProtocolVersion rides BOTH session_start rounds and nothing else
	// (guided-init D11a): the daemon requires exact equality before any
	// authentication work. Zero here means a peer that predates protocol
	// versioning — a mismatch by definition.
	ProtocolVersion int                 `json:"protocol_version,omitempty"`
	SecretEntry     *SecretEntryRequest `json:"secret_entry,omitempty"`
	OAuthLogin      *OAuthLoginRequest  `json:"oauth_login,omitempty"`
}

// OAuthLoginRequest is the strict, dedicated OAuth login control payload
// coordinated over ZMQ for an interactive loopback client.
type OAuthLoginRequest struct {
	Action      OAuthLoginAction `json:"action"`
	OperationID string           `json:"operation_id,omitempty"`
	Target      string           `json:"target,omitempty"`
	Value       *string          `json:"value,omitempty"`
}

// OAuthLoginResponse carries the daemon's response or unsolicited control
// result for an interactive OAuth login operation.
type OAuthLoginResponse struct {
	Action        OAuthLoginAction  `json:"action,omitempty"`
	OperationID   string            `json:"operation_id,omitempty"`
	Target        string            `json:"target,omitempty"`
	Outcome       OAuthLoginOutcome `json:"outcome"`
	AuthURL       string            `json:"auth_url,omitempty"`
	AcceptsManual bool              `json:"accepts_manual,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
}

// SecretEntryRequest is the strict, dedicated credential-entry control
// payload, addressed by Secret name. Presence of Value is significant: an
// empty value is still a submitted value and is refused by the value rules.
type SecretEntryRequest struct {
	Action      SecretEntryAction `json:"action"`
	OperationID string            `json:"operation_id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Value       *string           `json:"value,omitempty"`
}

// SecretState is the daemon-derived state of one Secret name.
type SecretState string

const (
	SecretStatePending  SecretState = "pending"
	SecretStateSet      SecretState = "set"
	SecretStateUnused   SecretState = "unused"
	SecretStateExternal SecretState = "pending_external"
)

// PendingSecret is one Secret name with the configuration fields that
// reference it and its derived state. It never carries a value.
type PendingSecret struct {
	Name   string      `json:"name"`
	Fields []string    `json:"fields,omitempty"`
	State  SecretState `json:"state"`
	// Rule is the violated value rule when a present value is non-conforming.
	Rule string `json:"rule,omitempty"`
}

// CredentialInfo summarizes one login-managed OAuth Credential.
type CredentialInfo struct {
	Target    string `json:"target"`
	State     string `json:"state"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type SecretEntryResponse struct {
	Action      SecretEntryAction  `json:"action"`
	OperationID string             `json:"operation_id,omitempty"`
	Outcome     SecretEntryOutcome `json:"outcome"`
	Warnings    []string           `json:"warnings,omitempty"`
	// RuleCode names the violated value rule on invalid_value.
	RuleCode string `json:"rule_code,omitempty"`
	// Secrets and Credentials answer a list action: every Secret name and
	// every Credential, never a value.
	Secrets     []PendingSecret  `json:"secrets,omitempty"`
	Credentials []CredentialInfo `json:"credentials,omitempty"`
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
	// AgentDir rides the authenticated handshake ok response only
	// (per-user-install-agent-isolation): the daemon-authoritative agent
	// directory for the interactive startup block, home-prefix sanitised
	// daemon-side (a leading ~ denotes the daemon user's home). Challenges,
	// auth failures and unauthenticated responses omit it; clients never
	// infer it from local config/cwd.
	AgentDir string `json:"agent_dir,omitempty"`
	// ProtocolVersion rides every response to a session_start (challenge,
	// ok, auth_fail, busy and protocol_mismatch) and is absent elsewhere
	// (guided-init D11a). A successful handshake fixes the version for the
	// whole connection. The client checks it before any HMAC work.
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// AgentID and AgentName ride the authenticated handshake ok response
	// only: the daemon's agent identity, shown by the TUI.
	AgentID        string               `json:"agent_id,omitempty"`
	AgentName      string               `json:"agent_name,omitempty"`
	PendingSecrets []PendingSecret      `json:"pending_secrets,omitempty"`
	Secrets        []PendingSecret      `json:"secrets,omitempty"`
	Credentials    []CredentialInfo     `json:"credentials,omitempty"`
	SecretEntry    *SecretEntryResponse `json:"secret_entry,omitempty"`
	OAuthLogin     *OAuthLoginResponse  `json:"oauth_login,omitempty"`
	ConfigSaved    bool                 `json:"config_saved,omitempty"`
}

// CompactionInfo carries a Compaction's estimated prompt size before and
// after, in tokens.
type CompactionInfo struct {
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
}

// Content values of the unsolicited Session lifecycle and post-save activation
// control events (Status=ok, ResponseType=control, no error_code). None of
// them consumes a pending reply slot.
const (
	// ControlSessionChanged announces a new conversation Session (carries
	// SessionID, no Usage).
	ControlSessionChanged = "session_changed"
	// ControlCompacted announces a completed Compaction (carries Compaction).
	ControlCompacted = "compacted"
	// ControlOAuthLoginResult announces completion of an interactive OAuth
	// login callback on the daemon (carries OAuthLogin).
	ControlOAuthLoginResult = "oauth_login_result"
	// ControlConfigReloaded announces completion of an in-process runtime
	// reload triggered outside the connected TUI.
	ControlConfigReloaded = "config_reloaded"
)

// Validate checks structural and semantic invariants on a Request. It returns
// nil when r is well-formed, and an error wrapping ErrMalformedRequest
// otherwise.
//
// Rules enforced here (matching the wire protocol):
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
		return fmt.Errorf("%w: unknown request type", ErrMalformedRequest)
	}

	if r.Timestamp == "" {
		return fmt.Errorf("%w: timestamp is required", ErrMalformedRequest)
	}

	if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
		return fmt.Errorf("%w: invalid timestamp", ErrMalformedRequest)
	}

	// Missing and differing session_start versions reach the daemon gate,
	// which returns protocol_mismatch with the daemon's version before auth.
	if r.Type != RequestTypeSessionStart && r.ProtocolVersion != 0 {
		return fmt.Errorf("%w: protocol_version is carried on session_start only", ErrMalformedRequest)
	}
	if r.Type == RequestTypeSecretEntry {
		if r.RequestID == "" || r.Content != "" || r.AuthToken != "" || r.SecretEntry == nil {
			return fmt.Errorf("%w: secret_entry requires request_id, empty content, no auth_token and a payload", ErrMalformedRequest)
		}
		if err := r.SecretEntry.validate(); err != nil {
			return err
		}
	} else if r.SecretEntry != nil {
		return fmt.Errorf("%w: secret_entry is allowed only for secret_entry requests", ErrMalformedRequest)
	}

	if r.Type == RequestTypeOAuthLogin {
		if r.RequestID == "" || r.Content != "" || r.AuthToken != "" || r.OAuthLogin == nil {
			return fmt.Errorf("%w: oauth_login requires request_id, empty content, no auth_token and a payload", ErrMalformedRequest)
		}
		if err := r.OAuthLogin.validate(); err != nil {
			return err
		}
	} else if r.OAuthLogin != nil {
		return fmt.Errorf("%w: oauth_login is allowed only for oauth_login requests", ErrMalformedRequest)
	}

	if r.Type == RequestTypeRuntimeReload {
		if r.RequestID == "" || r.Content != "" || r.AuthToken != "" {
			return fmt.Errorf("%w: runtime_reload requires request_id, empty content and no auth_token", ErrMalformedRequest)
		}
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
	if r.SecretEntry != nil {
		if !validSecretAction(r.SecretEntry.Action) || !validSecretOutcome(r.SecretEntry.Outcome) {
			return fmt.Errorf("%w: invalid secret_entry response", ErrMalformedResponse)
		}
	}
	if r.OAuthLogin != nil {
		if (r.OAuthLogin.Action != "" && !validOAuthLoginAction(r.OAuthLogin.Action)) || !validOAuthLoginOutcome(r.OAuthLogin.Outcome) {
			return fmt.Errorf("%w: invalid oauth_login response", ErrMalformedResponse)
		}
	}

	return nil
}

func validOAuthLoginAction(a OAuthLoginAction) bool {
	switch a {
	case OAuthLoginActionBegin, OAuthLoginActionSubmit, OAuthLoginActionCancel:
		return true
	default:
		return false
	}
}

func validOAuthLoginOutcome(outcome OAuthLoginOutcome) bool {
	switch outcome {
	case OAuthLoginOutcomeWaiting, OAuthLoginOutcomeCommitted, OAuthLoginOutcomeNotRequired, OAuthLoginOutcomeCancelled, OAuthLoginOutcomeRefused:
		return true
	default:
		return false
	}
}

func validOAuthTarget(target string) bool {
	if len(target) <= 4 {
		return false
	}
	prefix := target[:4]

	return prefix == "llm:" || prefix == "mcp:"
}

func (o OAuthLoginRequest) validate() error {
	if !validOAuthLoginAction(o.Action) {
		return fmt.Errorf("%w: unknown oauth_login action", ErrMalformedRequest)
	}
	if !validOAuthTarget(o.Target) {
		return fmt.Errorf("%w: oauth_login requires a valid llm:<name> or mcp:<name> target", ErrMalformedRequest)
	}
	has := func(v string) bool { return v != "" }
	switch o.Action {
	case OAuthLoginActionBegin:
		if has(o.OperationID) || o.Value != nil {
			return fmt.Errorf("%w: oauth_login begin accepts target only", ErrMalformedRequest)
		}
	case OAuthLoginActionSubmit:
		if !has(o.OperationID) || o.Value == nil {
			return fmt.Errorf("%w: oauth_login submit requires operation_id, target and value", ErrMalformedRequest)
		}
	case OAuthLoginActionCancel:
		if !has(o.OperationID) || o.Value != nil {
			return fmt.Errorf("%w: oauth_login cancel requires operation_id and target only", ErrMalformedRequest)
		}
	}

	return nil
}

func validSecretAction(a SecretEntryAction) bool {
	switch a {
	case SecretEntryActionList, SecretEntryActionBegin, SecretEntryActionSubmit, SecretEntryActionDelete, SecretEntryActionEnd:
		return true
	default:
		return false
	}
}

func validSecretOutcome(outcome SecretEntryOutcome) bool {
	switch outcome {
	case SecretEntryOutcomeListed, SecretEntryOutcomeActive, SecretEntryOutcomeCommitted, SecretEntryOutcomeDeleted, SecretEntryOutcomeReleased, SecretEntryOutcomeRefused:
		return true
	default:
		return false
	}
}

func (s SecretEntryRequest) validate() error {
	if !validSecretAction(s.Action) {
		return fmt.Errorf("%w: unknown secret_entry action", ErrMalformedRequest)
	}
	has := func(v string) bool { return v != "" }
	switch s.Action {
	case SecretEntryActionList:
		if has(s.OperationID) || has(s.Name) || s.Value != nil {
			return fmt.Errorf("%w: list accepts action only", ErrMalformedRequest)
		}
	case SecretEntryActionBegin:
		// An optional name selects one Secret for rotation.
		if has(s.OperationID) || s.Value != nil {
			return fmt.Errorf("%w: begin accepts an optional name only", ErrMalformedRequest)
		}
	case SecretEntryActionSubmit:
		if !has(s.OperationID) || !has(s.Name) || s.Value == nil {
			return fmt.Errorf("%w: submit requires operation_id, name and value", ErrMalformedRequest)
		}
	case SecretEntryActionDelete:
		if !has(s.Name) || has(s.OperationID) || s.Value != nil {
			return fmt.Errorf("%w: delete requires name only", ErrMalformedRequest)
		}
	case SecretEntryActionEnd:
		if !has(s.OperationID) || has(s.Name) || s.Value != nil {
			return fmt.Errorf("%w: end requires operation_id only", ErrMalformedRequest)
		}
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

// MarshalJSON distinguishes an authoritative empty set from metadata that is
// unavailable (omitted): a non-nil empty pending_secrets, secrets or
// credentials slice is emitted as [], a nil one is omitted.
func (r Response) MarshalJSON() ([]byte, error) {
	type wire Response
	out := struct {
		wire
		PendingSecrets *[]PendingSecret  `json:"pending_secrets,omitempty"`
		Secrets        *[]PendingSecret  `json:"secrets,omitempty"`
		Credentials    *[]CredentialInfo `json:"credentials,omitempty"`
	}{wire: wire(r)}
	if r.PendingSecrets != nil {
		out.PendingSecrets = &r.PendingSecrets
	}
	if r.Secrets != nil {
		out.Secrets = &r.Secrets
	}
	if r.Credentials != nil {
		out.Credentials = &r.Credentials
	}

	return json.Marshal(out)
}

// UnmarshalRequest decodes data into a Request, rejecting unknown JSON fields
// and calling Validate on the result. Any failure returns an error that
// satisfies errors.Is(err, ErrMalformedRequest).
func UnmarshalRequest(data []byte) (Request, error) {
	var req Request

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("%w: invalid request encoding", ErrMalformedRequest)
	}
	if dec.Decode(new(any)) != io.EOF {
		return Request{}, ErrMalformedRequest
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
		return Response{}, fmt.Errorf("%w: invalid response encoding", ErrMalformedResponse)
	}
	if dec.Decode(new(any)) != io.EOF {
		return Response{}, ErrMalformedResponse
	}

	if err := resp.Validate(); err != nil {
		return Response{}, err
	}

	return resp, nil
}

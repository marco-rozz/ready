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
	ErrorCodeInvalidValue       = "invalid_value" // a value rule failed; the rule code rides SecretEntryResponse.Rule
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
//
// Version 5 (subagent-runtime, BREAKING): the unsolicited `subagents`
// snapshot control event (Subagents) joins the wire; same-repo co-release,
// exact-equality handshake unchanged.
// Version 6 (subagent-panel-observability, BREAKING): HandbackNotice gains
// the optional `elapsed_ms` field (the dispatch's wall-clock duration,
// computed daemon-side); a strict protocol-5 peer would reject the unknown
// field, so the version bumps. Same-repo co-release, exact-equality
// handshake unchanged.
// Version 7 (turn-steering, BREAKING): a `message` sent while another is
// unanswered is admitted (no more blanket request_in_flight); every
// `message` carries a required request_id; the `turn` response object and the
// message_accepted / turn_started / steer_injected control events join the
// wire. Same-repo co-release, exact-equality handshake unchanged.
// Version 8 (say-mid-turn-notice, BREAKING): the `renders_notices` client
// capability on session_start, the `notice` response object and the `notice`
// control event (Mid-turn Notices) join the wire. Same-repo co-release,
// exact-equality handshake unchanged.
const Version = 8

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
	ProtocolVersion int `json:"protocol_version,omitempty"`
	// RendersNotices rides session_start only (say-mid-turn-notice): the
	// client declares it renders Mid-turn Notices. Absent means false.
	RendersNotices bool                `json:"renders_notices,omitempty"`
	SecretEntry    *SecretEntryRequest `json:"secret_entry,omitempty"`
	OAuthLogin     *OAuthLoginRequest  `json:"oauth_login,omitempty"`
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
	Fields [][]string  `json:"fields"`
	State  SecretState `json:"state"`
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
	Name        string             `json:"name,omitempty"`
	Field       []string           `json:"field,omitempty"`
	Rule        string             `json:"rule,omitempty"`
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

// Activity describes one request-correlated tool activity transition.
type Activity struct {
	State           string `json:"state"`
	Sequence        uint64 `json:"sequence"`
	ToolName        string `json:"tool_name,omitempty"`
	AdditionalTools *int   `json:"additional_tools,omitempty"`
}

// UnmarshalJSON rejects tool fields on thinking events even when explicitly empty.
func (a *Activity) UnmarshalJSON(data []byte) error {
	type wire Activity
	var value wire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value.State == "thinking" {
		_, hasName := fields["tool_name"]
		_, hasCount := fields["additional_tools"]
		if hasName || hasCount {
			return fmt.Errorf("invalid activity fields")
		}
	}
	*a = Activity(value)

	return nil
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
	// Subagents rides the unsolicited `subagents` control event only
	// (subagent-runtime): the fleet snapshot and the pending-handback
	// count.
	Subagents *SubagentsInfo `json:"subagents,omitempty"`
	// Handback rides the unsolicited `subagent_handback` control event
	// only (subagent-runtime): the just-delivered dispatch result's name
	// and status, so a connected idle TUI learns the Subagent finished
	// without waiting for the owner's next turn.
	Handback *HandbackNotice `json:"handback,omitempty"`
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
	AgentID     string               `json:"agent_id,omitempty"`
	AgentName   string               `json:"agent_name,omitempty"`
	Secrets     []PendingSecret      `json:"secrets,omitempty"`
	Credentials []CredentialInfo     `json:"credentials,omitempty"`
	SecretEntry *SecretEntryResponse `json:"secret_entry,omitempty"`
	OAuthLogin  *OAuthLoginResponse  `json:"oauth_login,omitempty"`
	ConfigSaved bool                 `json:"config_saved,omitempty"`
	Activity    *Activity            `json:"activity,omitempty"`
	// Turn carries the Turn correlation of message_accepted, turn_started,
	// steer_injected and the final reply (turn-steering); see TurnInfo for
	// the combination each response may carry.
	Turn *TurnInfo `json:"turn,omitempty"`
	// Notice rides the unsolicited `notice` control event only
	// (say-mid-turn-notice): the Mid-turn Notice text, redacted daemon-side.
	Notice *NoticeInfo `json:"notice,omitempty"`
}

// NoticeInfo is the `notice` response object (protocol 8): the text of one
// Mid-turn Notice after daemon-side redaction.
type NoticeInfo struct {
	Text string `json:"text"`
}

// Disposition values of message_accepted (TurnInfo.Disposition).
const (
	// DispositionQueued: the message will be (or already is) processed as
	// its own item.
	DispositionQueued = "queued"
	// DispositionSteer: the message is a steering candidate of the running
	// Turn.
	DispositionSteer = "steer"
	// DispositionStop: the message is the /stop command.
	DispositionStop = "stop"
)

// TurnInfo is the `turn` response object (protocol 7). TurnID is the
// daemon-minted Turn ID (never the formatter turn nonce). Which fields may
// appear depends on the response:
//
//	message_accepted: turn_id (when known), disposition (required)
//	turn_started:     turn_id, request_ids (required), deferred
//	steer_injected:   turn_id, request_ids (required), superseded_tool_calls
//	final reply:      turn_id, request_ids, dropped_request_ids
//
// SupersededToolCalls is a pointer so an explicit zero is distinguishable
// from absent.
type TurnInfo struct {
	TurnID              string   `json:"turn_id,omitempty"`
	Disposition         string   `json:"disposition,omitempty"`
	RequestIDs          []string `json:"request_ids,omitempty"`
	Deferred            bool     `json:"deferred,omitempty"`
	SupersededToolCalls *int     `json:"superseded_tool_calls,omitempty"`
	DroppedRequestIDs   []string `json:"dropped_request_ids,omitempty"`
}

// CompactionInfo carries a Compaction's estimated prompt size before and
// after, in tokens.
type CompactionInfo struct {
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
}

// SubagentsAgentView is one row of the `subagents` snapshot's fleet.
type SubagentsAgentView struct {
	SubagentID string `json:"subagent_id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at"` // RFC3339
	Activity   string `json:"activity,omitempty"`
	QueuedMail int    `json:"queued_mail"`
	Parent     string `json:"parent,omitempty"` // depth-2 only
}

// SubagentsInfo is the payload of the `subagents` control event: the full
// fleet snapshot and the pending-handback count (the TUI badge).
type SubagentsInfo struct {
	Agents           []SubagentsAgentView `json:"agents"`
	PendingHandbacks int                  `json:"pending_handbacks"`
}

// HandbackNotice is the payload of the `subagent_handback` control event:
// the just-delivered dispatch result's identity and outcome class, plus —
// since protocol version 6 (subagent-panel-observability D5) — the
// dispatch's wall-clock duration in milliseconds (absent/zero start ⇒ the
// field is omitted; an old daemon renders the plain two-segment line).
type HandbackNotice struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
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
	// ControlSubagents is the full Subagent fleet snapshot (subagent-
	// runtime D11): fleet rows plus the pending-handback count. Emitted on
	// fleet changes coalesced to at most 4 per second, once at Connection
	// attach with subagent history, and once empty after a cascade close.
	ControlSubagents = "subagents"
	// ControlSubagentHandback announces a delivered dispatch result
	// (subagent-runtime): the Subagent's name and the handback status.
	// Emitted on every handback delivery to the owner's connection; the
	// result's content itself still lands at the head of the owner's next
	// turn — this event is a visibility signal, never a delivery.
	ControlSubagentHandback = "subagent_handback"
	// ControlMessageAccepted acknowledges every admitted `message` request
	// (turn-steering): carries Turn.Disposition.
	ControlMessageAccepted = "message_accepted"
	// ControlTurnStarted precedes every Turn delivered to the Connection
	// (turn-steering): carries Turn.TurnID/RequestIDs/Deferred.
	ControlTurnStarted = "turn_started"
	// ControlSteerInjected reports candidates claimed into a Turn
	// (turn-steering): carries Turn.TurnID/RequestIDs/SupersededToolCalls.
	ControlSteerInjected = "steer_injected"
	// ControlNotice delivers one Mid-turn Notice (say-mid-turn-notice):
	// carries Notice.Text and Turn.TurnID; never a reply, never touches the
	// pending slot or the unanswered set.
	ControlNotice = "notice"
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
	if r.Type == RequestTypeMessage && r.RequestID == "" {
		return fmt.Errorf("%w: message requires a request_id", ErrMalformedRequest)
	}
	if r.Type != RequestTypeSessionStart && r.ProtocolVersion != 0 {
		return fmt.Errorf("%w: protocol_version is carried on session_start only", ErrMalformedRequest)
	}
	if r.Type != RequestTypeSessionStart && r.RendersNotices {
		return fmt.Errorf("%w: renders_notices is carried on session_start only", ErrMalformedRequest)
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
	if a := r.Activity; a != nil {
		if r.ConnectionID == "" || r.RequestID == "" || r.Status != ResponseStatusOK || r.ResponseType != ResponseTypeControl || r.Content != "" || r.ErrorCode != "" || a.Sequence == 0 {
			return fmt.Errorf("%w: invalid activity response", ErrMalformedResponse)
		}
		switch a.State {
		case "thinking":
			if a.ToolName != "" || a.AdditionalTools != nil {
				return fmt.Errorf("%w: invalid activity fields", ErrMalformedResponse)
			}
		case "using_tool":
			if a.ToolName == "" || a.AdditionalTools == nil || *a.AdditionalTools < 0 {
				return fmt.Errorf("%w: invalid activity fields", ErrMalformedResponse)
			}
		default:
			return fmt.Errorf("%w: invalid activity state", ErrMalformedResponse)
		}
	}
	if err := r.validateTurn(); err != nil {
		return err
	}
	if err := r.validateNotice(); err != nil {
		return err
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

// validateTurn enforces the per-response combinations of the `turn` object.
func (r Response) validateTurn() error {
	t := r.Turn
	if t == nil {
		return nil
	}

	bad := func(msg string) error { return fmt.Errorf("%w: invalid turn object: %s", ErrMalformedResponse, msg) }

	switch {
	case r.ResponseType == ResponseTypeControl && r.Content == ControlMessageAccepted:
		return validateAcceptedTurn(t, bad)
	case r.ResponseType == ResponseTypeControl && r.Content == ControlTurnStarted:
		return validateStartedTurn(t, bad)
	case r.ResponseType == ResponseTypeControl && r.Content == ControlSteerInjected:
		return validateInjectedTurn(t, bad)
	case r.ResponseType == ResponseTypeControl && r.Content == ControlNotice:
		return validateNoticeTurn(t, bad)
	case r.ResponseType == ResponseTypeReply:
		if t.Disposition != "" || t.Deferred || t.SupersededToolCalls != nil {
			return bad("a final reply carries only turn_id, request_ids and dropped_request_ids")
		}

		return nil
	default:
		return bad("not allowed on this response")
	}
}

func validateAcceptedTurn(t *TurnInfo, bad func(string) error) error {
	switch t.Disposition {
	case DispositionQueued, DispositionSteer, DispositionStop:
	default:
		return bad("message_accepted requires a valid disposition")
	}

	if len(t.RequestIDs) != 0 || t.Deferred || t.SupersededToolCalls != nil || len(t.DroppedRequestIDs) != 0 {
		return bad("message_accepted carries only turn_id and disposition")
	}

	return nil
}

func validateStartedTurn(t *TurnInfo, bad func(string) error) error {
	if t.TurnID == "" || len(t.RequestIDs) == 0 {
		return bad("turn_started requires turn_id and request_ids")
	}

	if t.Disposition != "" || t.SupersededToolCalls != nil || len(t.DroppedRequestIDs) != 0 {
		return bad("turn_started carries only turn_id, request_ids and deferred")
	}

	return nil
}

func validateInjectedTurn(t *TurnInfo, bad func(string) error) error {
	if t.TurnID == "" || len(t.RequestIDs) == 0 {
		return bad("steer_injected requires turn_id and request_ids")
	}

	if t.Disposition != "" || t.Deferred || len(t.DroppedRequestIDs) != 0 {
		return bad("steer_injected carries only turn_id, request_ids and superseded_tool_calls")
	}

	if t.SupersededToolCalls != nil && *t.SupersededToolCalls < 0 {
		return bad("superseded_tool_calls must not be negative")
	}

	return nil
}

func validateNoticeTurn(t *TurnInfo, bad func(string) error) error {
	if t.TurnID == "" {
		return bad("notice requires turn_id")
	}

	if t.Disposition != "" || len(t.RequestIDs) != 0 || t.Deferred || t.SupersededToolCalls != nil || len(t.DroppedRequestIDs) != 0 {
		return bad("notice carries only turn_id")
	}

	return nil
}

// validateNotice enforces the `notice` object / `notice` event pairing.
func (r Response) validateNotice() error {
	isEvent := r.ResponseType == ResponseTypeControl && r.Content == ControlNotice
	if r.Notice == nil {
		if isEvent {
			return fmt.Errorf("%w: notice event requires the notice object", ErrMalformedResponse)
		}

		return nil
	}

	if !isEvent {
		return fmt.Errorf("%w: notice object is allowed only on the notice event", ErrMalformedResponse)
	}

	if r.Notice.Text == "" {
		return fmt.Errorf("%w: notice.text is required", ErrMalformedResponse)
	}

	if r.Status != ResponseStatusOK || r.ErrorCode != "" || r.RequestID != "" || r.Usage != nil {
		return fmt.Errorf("%w: invalid notice event", ErrMalformedResponse)
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
// unavailable (omitted): a non-nil empty secrets or
// credentials slice is emitted as [], a nil one is omitted.
func (r Response) MarshalJSON() ([]byte, error) {
	type wire Response
	out := struct {
		wire
		Secrets     *[]PendingSecret  `json:"secrets,omitempty"`
		Credentials *[]CredentialInfo `json:"credentials,omitempty"`
	}{wire: wire(r)}
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

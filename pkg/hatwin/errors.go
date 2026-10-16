package hatwin

import "errors"

var (
	// ErrNotReady is returned by the read API when the initial sync has not
	// yet completed.
	ErrNotReady = errors.New("hatwin: digital twin not ready")

	// ErrAuthInvalid is returned when HA rejects the access token.
	// The reconnect loop treats this as a permanent failure and stops retrying.
	ErrAuthInvalid = errors.New("hatwin: authentication invalid")

	// ErrClosed is returned when operations are attempted on a stopped twin.
	ErrClosed = errors.New("hatwin: twin closed")

	// Write-barrier sentinels (twin-native-actuation D2). CallService and
	// ExecuteCommand fail immediately — never waiting for a reconnect — with
	// one of these when the write barrier refuses the command.

	// ErrNotConnected is returned when no WS session handle is published:
	// the twin never connected, is reconnecting, or has been closed.
	ErrNotConnected = errors.New("hatwin: not connected (no live HA session)")

	// ErrSessionNotReady is returned when a live session exists but its
	// bootstrap (registries + states) has not completed yet.
	ErrSessionNotReady = errors.New("hatwin: session bootstrap not complete")

	// ErrStale is returned when the session is live but the last inbound WS
	// frame is older than Options.HardStaleThreshold.
	ErrStale = errors.New("hatwin: connection stale beyond the hard stale threshold")

	// ErrSessionClosed is delivered to every pending command waiter when the
	// WS session ends (read-loop error, zombie force-close, shutdown). It
	// wraps the read-loop cause when one is available.
	ErrSessionClosed = errors.New("hatwin: HA session closed while awaiting result")

	// Command-refusal sentinels (twin-native-actuation D7). The ha_command
	// adapter maps them to errdigest severities: everything below is
	// Permanent except the barrier sentinels above (Transient).

	// ErrOutOfVocabulary is returned for a domain or action outside the
	// active command vocabulary (including lock/alarm_control_panel, which
	// are never in the table, and siren when not explicitly activated).
	ErrOutOfVocabulary = errors.New("hatwin: action outside the active command vocabulary")

	// ErrInvalidParams is returned when a parametric action carries invalid,
	// out-of-range, or unknown parameters for its domain.
	ErrInvalidParams = errors.New("hatwin: invalid command parameters")

	// ErrDomainRequired is returned when a target selector omits the
	// mandatory domain.
	ErrDomainRequired = errors.New("hatwin: target selector requires a domain")

	// ErrNoMatch is returned when a selector matches zero entities.
	ErrNoMatch = errors.New("hatwin: no entity matches the selector")

	// ErrUnknownEntity is returned when an explicit entity_id is not known
	// to the twin (HA silently ignores unknown ids, so the twin validates).
	ErrUnknownEntity = errors.New("hatwin: unknown entity_id")

	// ErrOverCap is returned when a selector resolves to more targets than
	// the configured max_targets cap.
	ErrOverCap = errors.New("hatwin: selector matches exceed max_targets")

	// ErrMultiTargetToggle is returned when a toggle resolves to more than
	// one target.
	ErrMultiTargetToggle = errors.New("hatwin: toggle requires exactly one target")

	// ErrUnknownCallID is returned by VerifyCommand for an unknown or expired
	// logical call_id.
	ErrUnknownCallID = errors.New("hatwin: unknown or expired call_id")

	// ErrInvalidFilter is returned by the read API for a malformed filter
	// criterion (unsupported comparison form, unknown sort key).
	ErrInvalidFilter = errors.New("hatwin: invalid filter")
)

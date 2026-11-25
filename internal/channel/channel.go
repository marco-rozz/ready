// Package channel defines the common interface and types for all I/O channels.
package channel

import "context"

// Channel represents a bidirectional communication endpoint.
// Each channel implementation runs in its own goroutine and
// communicates with the agent via messages.
type Channel interface {
	// Name returns the channel identifier (e.g., "cli", "telegram").
	Name() string

	// Receive blocks until a message is available from the user.
	// It should respect context cancellation.
	Receive(ctx context.Context) (Message, error)

	// Send delivers a message to the user.
	Send(ctx context.Context, message Message) error

	// Close shuts down the channel gracefully.
	Close() error
}

// Origin says who produced an inbound Message. It affects steering
// eligibility only: an item of any origin, including OriginUnspecified, is
// still processed as an ordinary Turn or command.
type Origin uint8

const (
	// OriginUnspecified is the zero value: a producer that did not set the
	// field. It is never steerable (fail-closed).
	OriginUnspecified Origin = iota
	// OriginHuman is a message typed by a person on an attended channel.
	OriginHuman
	// OriginHarness is an item produced by the daemon itself (triggers,
	// alerts, wakes, synthetic Turns).
	OriginHarness
	// OriginScripted is a one-shot scripted request (ZMQ exec).
	OriginScripted
)

// String returns the origin's log/diagnostic name.
func (o Origin) String() string {
	switch o {
	case OriginHuman:
		return "human"
	case OriginHarness:
		return "harness"
	case OriginScripted:
		return "scripted"
	default:
		return "unspecified"
	}
}

// Message represents a single communication unit exchanged between
// a channel and the agent.
type Message struct {
	// Content is the text payload of the message.
	Content string

	// ChannelName identifies which channel originated or should receive this message.
	ChannelName string

	// Metadata holds optional per-channel key-value data
	// (e.g., Telegram chat_id).
	Metadata map[string]string

	// Origin identifies who produced the message (set by every producer).
	Origin Origin
}

// ConnectionInfo is what a Connection start hook reports back to a
// connection-oriented channel for its handshake response.
type ConnectionInfo struct {
	// SessionID is the effective conversation Session ID of the address
	// (the pending one when the hook just decided a reset).
	SessionID string
	// ResetPending reports that a Session reset was decided at this
	// Connection start and awaits application: the handshake then carries
	// no usage seed.
	ResetPending bool
}

// ConnectionStartHook is invoked by a connection-oriented channel inside a
// successful Connection start, before the handshake response is composed.
// msg carries the channel name and the Connection's metadata so the hook can
// derive the Conversation address. It must never block on the address's
// message worker.
type ConnectionStartHook func(ctx context.Context, msg Message) ConnectionInfo

// ConnectionOriented is implemented by channels whose clients attach through
// explicit Connections (the ZMQ CLI; serial in phase 4): Session reset rules
// are evaluated only when a new Connection begins, silently, and never while
// a Connection is live (the session-lifecycle D6).
type ConnectionOriented interface {
	Channel
	// OnConnectionStart wires the hook invoked at every successful
	// Connection start.
	OnConnectionStart(hook ConnectionStartHook)
}

// NoticeOutcome is the result class of one Mid-turn Notice delivery
// (say-mid-turn-notice). A failure before any part was accepted is not an
// outcome: SendNotice returns an error instead.
type NoticeOutcome string

const (
	// NoticeDelivered: the channel transport accepted the whole Notice for
	// display (never "read by the human").
	NoticeDelivered NoticeOutcome = "delivered"
	// NoticePartial: parts 1..DeliveredParts of Parts were accepted, then the
	// transport failed.
	NoticePartial NoticeOutcome = "partial"
	// NoticeNotShown: no client can show the Notice; Reason says why.
	NoticeNotShown NoticeOutcome = "not_shown"
)

// Reasons of a NoticeNotShown outcome.
const (
	// NoticeReasonClientDoesNotRender: the active Connection did not declare
	// the Notice rendering capability (--exec, --raw, piped line reader).
	NoticeReasonClientDoesNotRender = "client_does_not_render"
	// NoticeReasonNoConnection: no client is attached to the address.
	NoticeReasonNoConnection = "no_connection"
	// NoticeReasonChannelUnsupported: the channel implements no NoticeSender.
	NoticeReasonChannelUnsupported = "channel_unsupported"
	// NoticeReasonNoChannel: the Turn context carries no deliverer (probes,
	// auxiliary consumers).
	NoticeReasonNoChannel = "no_channel"
)

// NoticeResult is the outcome of NoticeSender.SendNotice.
type NoticeResult struct {
	Outcome NoticeOutcome
	// Parts and DeliveredParts are set for NoticePartial (and may be set for
	// NoticeDelivered by multi-part transports).
	Parts          int
	DeliveredParts int
	// Reason is set for NoticeNotShown only.
	Reason string
}

// NoticeSender is the optional capability of a channel that can show a
// Mid-turn Notice: a new, self-contained act (never a preview, never an edit
// of a streamed message). A channel without it yields
// NoticeNotShown/channel_unsupported.
type NoticeSender interface {
	// SendNotice shows text on the conversation identified by meta. A
	// non-nil error means the transport failed before any part was accepted;
	// a failure after at least one accepted part is reported as NoticePartial
	// with a nil error.
	SendNotice(ctx context.Context, meta map[string]string, text string) (NoticeResult, error)
}

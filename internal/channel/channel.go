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

// Message represents a single communication unit exchanged between
// a channel and the agent.
type Message struct {
	// Content is the text payload of the message.
	Content string

	// ChannelName identifies which channel originated or should receive this message.
	ChannelName string

	// Metadata holds optional per-channel key-value data
	// (e.g., Telegram chat_id, WhatsApp sender number).
	Metadata map[string]string
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

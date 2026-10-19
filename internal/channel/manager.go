package channel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// ErrUnknownChannel is returned when attempting to send to a channel that is not registered.
var ErrUnknownChannel = errors.New("unknown channel")

// Manager orchestrates the lifecycle of all enabled channels.
// It starts each channel in its own goroutine, collects incoming
// messages into a single stream, and routes outgoing messages
// to the appropriate channel.
type Manager struct {
	channels map[string]Channel
	incoming chan Message
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	logger   zerolog.Logger

	// stopOnce makes Stop idempotent (criticalities-remediation LOW
	// lifecycle): a second call must be a no-op —
	// close(m.incoming) would otherwise panic on the second Stop.
	stopOnce sync.Once
}

// NewManager creates a Manager with the given set of channels.
func NewManager(logger zerolog.Logger, channels ...Channel) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	m := &Manager{
		channels: make(map[string]Channel, len(channels)),
		incoming: make(chan Message, 64),
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger.With().Str("component", "channel-manager").Logger(),
	}

	for _, ch := range channels {
		m.channels[ch.Name()] = ch
	}

	return m
}

// Start launches a receive loop for each registered channel.
func (m *Manager) Start() error {
	for _, ch := range m.channels {
		m.wg.Add(1)

		go m.receiveLoop(ch)
	}

	return nil
}

// receiveLoop continuously reads from a channel and forwards messages to the incoming queue.
// A TRANSIENT receive error (network blip, long-poll hiccup) is logged and the
// loop continues with a short backoff — a single transient error must not
// leave a channel permanently deaf. The loop exits only on context
// cancellation (graceful shutdown).
func (m *Manager) receiveLoop(ch Channel) {
	defer m.wg.Done()

	for {
		msg, err := ch.Receive(m.ctx)
		if err != nil {
			// Context cancelled means graceful shutdown.
			if errors.Is(err, context.Canceled) || m.ctx.Err() != nil {
				return
			}

			// Transient error: log, back off, keep receiving — the channel
			// must not go permanently deaf.
			m.logger.Warn().Str("channel", ch.Name()).Err(err).Msg("transient receive error — retrying")

			select {
			case <-time.After(time.Second):
			case <-m.ctx.Done():
				return
			}

			continue
		}

		msg.ChannelName = ch.Name()

		select {
		case m.incoming <- msg:
		case <-m.ctx.Done():
			return
		}
	}
}

// Incoming returns the channel that delivers all received messages.
func (m *Manager) Incoming() <-chan Message {
	return m.incoming
}

// Channel returns the underlying Channel registered under name, or nil when
// no such channel exists.  This is used by the main loop to access
// channel-specific capabilities (e.g. Telegram typing indicator).
func (m *Manager) Channel(name string) Channel {
	return m.channels[name]
}

// SendTo delivers a message to a specific channel identified by name.
func (m *Manager) SendTo(ctx context.Context, channelName string, msg Message) error {
	ch, ok := m.channels[channelName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownChannel, channelName)
	}

	if err := ch.Send(ctx, msg); err != nil {
		return fmt.Errorf("failed to send to channel %s: %w", channelName, err)
	}

	return nil
}

// Stop cancels all channel goroutines, waits for them to finish,
// and closes each channel.
func (m *Manager) Stop() error {
	var errs []error

	// criticalities-remediation (LOW lifecycle): Stop is idempotent — the
	// second call is a no-op instead of panicking on the double channel
	// close.
	m.stopOnce.Do(func() {
		m.cancel()
		m.wg.Wait()

		for _, ch := range m.channels {
			if err := ch.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing %s: %w", ch.Name(), err))
			}
		}

		close(m.incoming)
	})

	return nil
}

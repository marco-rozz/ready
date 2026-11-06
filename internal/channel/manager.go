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

type managedChannel struct {
	ch     Channel
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager orchestrates the lifecycle of all enabled channels.
// It starts each channel in its own goroutine, collects incoming
// messages into a single stream, and routes outgoing messages
// to the appropriate channel.
type Manager struct {
	mu       sync.RWMutex
	channels map[string]Channel
	running  map[string]*managedChannel
	started  bool

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
		running:  make(map[string]*managedChannel, len(channels)),
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
	m.mu.Lock()
	defer m.mu.Unlock()

	m.started = true
	for name, ch := range m.channels {
		if _, exists := m.running[name]; !exists {
			m.startChannelLocked(ch)
		}
	}

	return nil
}

func (m *Manager) startChannelLocked(ch Channel) {
	chCtx, chCancel := context.WithCancel(m.ctx)
	entry := &managedChannel{
		ch:     ch,
		ctx:    chCtx,
		cancel: chCancel,
		done:   make(chan struct{}),
	}
	m.running[ch.Name()] = entry
	m.wg.Add(1)

	go m.receiveLoop(entry)
}

// AddOrReplace registers ch under its Name() and, if the Manager has already
// been started, launches its receive loop. If a different Channel instance was
// already registered under the same name, it is stopped and closed first.
func (m *Manager) AddOrReplace(ch Channel) {
	name := ch.Name()
	m.mu.Lock()
	oldCh := m.channels[name]
	if oldCh == ch {
		if m.started && m.running[name] == nil {
			m.startChannelLocked(ch)
		}
		m.mu.Unlock()

		return
	}
	oldEntry := m.running[name]
	delete(m.running, name)
	delete(m.channels, name)
	m.mu.Unlock()

	if oldEntry != nil {
		oldEntry.cancel()
		<-oldEntry.done
	}
	if oldCh != nil {
		_ = oldCh.Close()
	}

	m.mu.Lock()
	m.channels[name] = ch
	if m.started && m.ctx.Err() == nil {
		m.startChannelLocked(ch)
	}
	m.mu.Unlock()
}

// StopChannel stops the receive loop for name and closes the underlying
// Channel without closing the Manager's shared incoming stream.
func (m *Manager) StopChannel(name string) error {
	m.mu.Lock()
	ch, ok := m.channels[name]
	entry := m.running[name]
	delete(m.channels, name)
	delete(m.running, name)
	m.mu.Unlock()

	if entry != nil {
		entry.cancel()
		<-entry.done
	}
	if ok && ch != nil {
		return ch.Close()
	}

	return nil
}

// receiveLoop continuously reads from a channel and forwards messages to the incoming queue.
// A TRANSIENT receive error (network blip, long-poll hiccup) is logged and the
// loop continues with a short backoff — a single transient error must not
// leave a channel permanently deaf. The loop exits only on context
// cancellation (graceful shutdown).
func (m *Manager) receiveLoop(entry *managedChannel) {
	defer m.wg.Done()
	defer close(entry.done)

	ch := entry.ch
	for {
		msg, err := ch.Receive(entry.ctx)
		if err != nil {
			// Context cancelled means graceful shutdown or per-channel stop.
			if errors.Is(err, context.Canceled) || entry.ctx.Err() != nil || m.ctx.Err() != nil {
				return
			}

			// Transient error: log, back off, keep receiving — the channel
			// must not go permanently deaf.
			m.logger.Warn().Str("channel", ch.Name()).Err(err).Msg("transient receive error — retrying")

			select {
			case <-time.After(time.Second):
			case <-entry.ctx.Done():
				return
			}

			continue
		}

		msg.ChannelName = ch.Name()

		select {
		case m.incoming <- msg:
		case <-entry.ctx.Done():
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
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.channels[name]
}

// SendTo delivers a message to a specific channel identified by name.
func (m *Manager) SendTo(ctx context.Context, channelName string, msg Message) error {
	m.mu.RLock()
	ch, ok := m.channels[channelName]
	m.mu.RUnlock()
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
		m.mu.RLock()
		for _, entry := range m.running {
			entry.cancel()
		}
		m.mu.RUnlock()
		m.wg.Wait()

		m.mu.Lock()
		channels := make([]Channel, 0, len(m.channels))
		for _, ch := range m.channels {
			channels = append(channels, ch)
		}
		m.channels = make(map[string]Channel)
		m.running = make(map[string]*managedChannel)
		m.mu.Unlock()

		for _, ch := range channels {
			if err := ch.Close(); err != nil {
				errs = append(errs, fmt.Errorf("closing %s: %w", ch.Name(), err))
			}
		}

		close(m.incoming)
	})

	return nil
}

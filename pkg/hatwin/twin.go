// Package hatwin provides a real-time in-memory digital twin of a Home
// Assistant instance via its WebSocket API.
//
// The twin maintains an entity-state map and area/device/entity registries
// updated by subscribing to all state_changed events. It is designed as a
// detachable library: it imports no project-internal packages and no agent
// framework dependencies.
package hatwin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Twin is the HA WebSocket digital twin. It holds an always-current in-memory
// mirror of all entity states and registries, updated in real time.
//
// Create with New, then call Start to begin the background connection loop.
// Close stops the loop and releases resources.
type Twin struct {
	opts  Options
	log   zerolog.Logger
	token string

	// mu guards states, areas, devices, entities, and haConfig.
	mu       sync.RWMutex
	states   map[string]map[string]any // entity_id → raw state object
	areas    map[string]map[string]any // area_id   → area registry entry
	devices  map[string]map[string]any // device_id → device registry entry
	entities map[string]map[string]any // entity_id → entity registry entry (display)
	haConfig map[string]any

	// ready is set to true after the first successful initial sync.
	// It stays true across reconnects: stale-but-available data is served.
	ready     atomic.Bool
	readyCh   chan struct{} // closed exactly once on first successful sync
	readyOnce sync.Once

	// topologyCh carries debounced topology-change signals.
	topologyCh    chan TopologyChangedSignal
	debounceMu    sync.Mutex
	debounceTimer *time.Timer
	// topologyDirty is set on every registry event (before the debounce timer
	// fires) and consumed via CompareAndSwap by the Snapshotter's run loop, so
	// a burst of events during a slow generate() cycle is not silently
	// dropped by the size-1 topologyCh.
	topologyDirty atomic.Bool

	// session is the handle to the live WebSocket session, published after
	// the bootstrap commands are written and cleared when the session ends
	// (twin-native-actuation D1). Command traffic (CallService) flows through
	// it; nil means never connected, reconnecting, or closed.
	session atomic.Pointer[wsSession]

	// lastWSActivity is updated (UnixNano) on every inbound WS frame — pings,
	// results, and events alike. It is the WS-connection health signal.
	lastWSActivity atomic.Int64
	// lastStateEvent is updated (UnixNano) only on state_changed events. It is
	// diagnostic only: distinguishes "the house has been quiet" from
	// "the WS connection is dead".
	lastStateEvent atomic.Int64
	// outstandingPings counts unacknowledged pings sent since the last pong.
	// Incremented on ping send, decremented on matching pong receipt.
	outstandingPings atomic.Int64
	// pingTracker records in-flight ping IDs so a pong can be matched and
	// removed exactly once.
	pingTracker sync.Map // int64 ping id -> struct{}

	// Command-evidence machinery (twin-native-actuation D3). cmdMu guards
	// activeEvidence (armed, collecting) and the evidenceStore/order (all
	// entries for mode=verify lookups, TTL- and cap-bounded).
	cmdMu          sync.Mutex
	activeEvidence []*evidenceEntry
	evidenceStore  map[string]*evidenceEntry
	evidenceOrder  []string

	cancel context.CancelFunc
	done   chan struct{}
}

// New validates opts and constructs a Twin. It does not connect; call Start.
func New(opts Options) (*Twin, error) {
	opts.applyDefaults()

	if opts.URL == "" {
		return nil, fmt.Errorf("hatwin: URL is required")
	}

	tok, err := resolveToken(opts)
	if err != nil {
		return nil, err
	}

	log := opts.Logger.With().Str("component", "hatwin").Logger()

	return &Twin{
		opts:          opts,
		log:           log,
		token:         tok,
		states:        make(map[string]map[string]any),
		areas:         make(map[string]map[string]any),
		devices:       make(map[string]map[string]any),
		entities:      make(map[string]map[string]any),
		haConfig:      make(map[string]any),
		evidenceStore: make(map[string]*evidenceEntry),
		readyCh:       make(chan struct{}),
		topologyCh:    make(chan TopologyChangedSignal, 1),
		done:          make(chan struct{}),
	}, nil
}

// Start launches the background reconnect/session loop. It is non-blocking.
// The twin's lifetime is bound to ctx; cancel it to initiate shutdown.
func (t *Twin) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	go t.run(ctx)
}

// Close stops the twin and waits for the background goroutine to exit.
// Close is safe to call only after Start has been called.
func (t *Twin) Close() error {
	if t.cancel != nil {
		t.cancel()
		<-t.done
	}

	return nil
}

// WaitReady blocks until the initial sync completes or ctx expires/is cancelled.
// Returns ErrNotReady if ctx times out before the twin becomes ready.
func (t *Twin) WaitReady(ctx context.Context) error {
	select {
	case <-t.readyCh:
		return nil
	case <-ctx.Done():
		return ErrNotReady
	}
}

// Ready reports whether at least one initial sync has completed.
// The twin may hold stale data while reconnecting, but Ready stays true.
func (t *Twin) Ready() bool {
	return t.ready.Load()
}

// Config returns the HA global configuration. Returns a zero value if not yet synced.
func (t *Twin) Config() HAConfig {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return extractHAConfig(t.haConfig)
}

// StateCount returns the current number of entities in the twin.
func (t *Twin) StateCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return len(t.states)
}

// LastWSActivity returns the time of the last inbound WS frame (any type).
// Returns the zero Time if no frame has been received yet.
func (t *Twin) LastWSActivity() time.Time {
	v := t.lastWSActivity.Load()
	if v == 0 {
		return time.Time{}
	}

	return time.Unix(0, v)
}

// LastStateEvent returns the time of the last state_changed event. Returns
// the zero Time if no state_changed event has been received yet.
func (t *Twin) LastStateEvent() time.Time {
	v := t.lastStateEvent.Load()
	if v == 0 {
		return time.Time{}
	}

	return time.Unix(0, v)
}

// OutstandingPings returns the current count of unacknowledged pings.
func (t *Twin) OutstandingPings() int {
	return int(t.outstandingPings.Load())
}

// ConsumeTopologyDirty performs a CompareAndSwap(true, false) on the
// topology-dirty flag, returning true if it was set. The Snapshotter's run
// loop calls this after each generate() cycle and loops again if the flag was
// set again during the cycle, guaranteeing at-least-once regeneration with
// respect to distinct registry-event windows even when events arrive faster
// than a single write cycle.
func (t *Twin) ConsumeTopologyDirty() bool {
	return t.topologyDirty.CompareAndSwap(true, false)
}

// StopSessionsForTest clears the published session handle. TEST-ONLY hook:
// internal packages use it to exercise the write barrier without killing a
// live connection.
func (t *Twin) StopSessionsForTest() {
	t.session.Store(nil)
}

// TopologyChanges returns a channel that receives a debounced signal whenever
// HA emits a registry-update event (area/device/entity_registry_updated).
// The channel is buffered (size 1); a slow reader will not miss a burst.
func (t *Twin) TopologyChanges() <-chan TopologyChangedSignal {
	return t.topologyCh
}

// Areas returns a shallow copy of the area registry keyed by area_id.
func (t *Twin) Areas() map[string]map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]map[string]any, len(t.areas))
	maps.Copy(out, t.areas)

	return out
}

// Devices returns a shallow copy of the device registry keyed by device_id.
func (t *Twin) Devices() map[string]map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]map[string]any, len(t.devices))
	maps.Copy(out, t.devices)

	return out
}

// Entities returns a shallow copy of the entity registry keyed by entity_id.
func (t *Twin) Entities() map[string]map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]map[string]any, len(t.entities))
	maps.Copy(out, t.entities)

	return out
}

// run is the outer reconnect loop. It calls runSession in a loop with
// exponential backoff until ctx is canceled, MaxRetries is exhausted, or an
// unrecoverable error (ErrAuthInvalid) is encountered.
func (t *Twin) run(ctx context.Context) {
	defer close(t.done)

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		err := t.runSession(ctx)
		if ctx.Err() != nil {
			return
		}

		if errors.Is(err, ErrAuthInvalid) {
			t.log.Error().Msg("HA WS authentication invalid, stopping reconnect")

			return
		}

		t.log.Warn().Err(err).Msg("HA WS session ended")

		attempt++
		if t.opts.MaxRetries > 0 && attempt >= t.opts.MaxRetries {
			t.log.Error().Int("attempts", attempt).Msg("HA WS reconnect exhausted")

			return
		}

		backoff := computeBackoff(t.opts, attempt)
		t.log.Info().Dur("backoff", backoff).Int("attempt", attempt).Msg("HA WS reconnecting")

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// computeBackoff returns the delay for a given attempt (1-based), clamped to
// [InitialBackoff, MaxBackoff].
func computeBackoff(opts Options, attempt int) time.Duration {
	if attempt <= 0 {
		return opts.InitialBackoff
	}
	// Clamp in the float64 domain BEFORE converting to time.Duration: for
	// large attempts float64(initial)*factor exceeds int64 and the float→int
	// conversion is architecture-dependent (arm64 saturates to MaxInt64 —
	// harmless, the clamp below would catch it anyway — while amd64 yields
	// the indefinite value MinInt64, which turned into a reset to
	// InitialBackoff instead of clamping to MaxBackoff).
	factor := math.Pow(opts.BackoffFactor, float64(attempt-1))
	delayF := float64(opts.InitialBackoff) * factor
	maxF := float64(opts.MaxBackoff)
	if delayF >= maxF || math.IsInf(delayF, 1) {
		return opts.MaxBackoff
	}
	if delayF <= float64(opts.InitialBackoff) {
		return opts.InitialBackoff
	}

	return time.Duration(delayF)
}

// onRegistryEvent schedules a debounced topology-change signal.
func (t *Twin) onRegistryEvent() {
	t.debounceMu.Lock()
	defer t.debounceMu.Unlock()

	t.topologyDirty.Store(true)

	if t.debounceTimer != nil {
		t.debounceTimer.Stop()
	}
	t.debounceTimer = time.AfterFunc(t.opts.TopologyDebounce, func() {
		select {
		case t.topologyCh <- TopologyChangedSignal{}:
		default:
		}
	})
}

// resolveToken returns the token from opts.Token or opts.TokenFile.
func resolveToken(opts Options) (string, error) {
	if opts.Token != "" {
		return opts.Token, nil
	}
	if opts.TokenFile != "" {
		data, err := os.ReadFile(opts.TokenFile)
		if err != nil {
			return "", fmt.Errorf("hatwin: read token file: %w", err)
		}

		return strings.TrimSpace(string(data)), nil
	}

	return "", fmt.Errorf("hatwin: Token or TokenFile must be set")
}

// extractHAConfig projects the raw haConfig map onto a HAConfig struct,
// intentionally omitting sensitive fields (latitude, longitude, elevation).
func extractHAConfig(raw map[string]any) HAConfig {
	return HAConfig{
		Name:         pickStr(raw, "name"),
		LocationName: pickStr(raw, "location_name"),
		TimeZone:     pickStr(raw, "time_zone"),
		Version:      pickStr(raw, "version"),
	}
}

// compile-time assertions: Twin must not import internal/ (enforced by build).
// This var ensures the zerolog.Logger field is used without the global logger.
var _ zerolog.Logger = zerolog.Logger{}

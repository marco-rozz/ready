package hatwin

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// writeDeadline bounds every WebSocket write (M4): a half-open TCP connection
// cannot wedge a write for minutes — the deadline expires first.
const writeDeadline = 10 * time.Second

const (
	bootAreaRegistryID   = 2
	bootDeviceRegistryID = 3
	bootEntityRegistryID = 4
	bootGetStatesID      = 5
	bootSubscribeAllID   = 6
	bootGetConfigID      = 7
)

const (
	keyEntityID    = "entity_id"
	msgTypeKey     = "type"
	msgTypePing    = "ping"
	msgTypePong    = "pong"
	msgTypeAuth    = "auth"
	msgTypeAuthReq = "auth_required"
	msgTypeAuthOK  = "auth_ok"
	msgTypeAuthInv = "auth_invalid"
	msgTypeResult  = "result"
	msgTypeEvent   = "event"
)

// bootstrapIDs lists the command responses that must all arrive before the
// twin is considered ready.
func bootstrapIDs() []int {
	return []int{bootAreaRegistryID, bootDeviceRegistryID, bootEntityRegistryID, bootGetStatesID}
}

// runSession dials HA, completes the auth handshake, sends the bootstrap
// commands, and processes messages until the connection is lost or ctx is
// canceled. Readiness is signaled once all four bootstrap responses arrive.
// The per-session handle is published for command traffic after the bootstrap
// commands are written and cleared when the session ends; pending command
// waiters fail with ErrSessionClosed on teardown (twin-native-actuation D1).
func (t *Twin) runSession(ctx context.Context) (err error) {
	conn, err := t.dial(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	// criticalities-remediation (H1): every goroutine bound to this connection
	// runs on a PER-SESSION context cancelled when the session ends (read loop
	// error, zombie force-close, or reconnect) — a twin-level context here
	// leaked one blocked goroutine per reconnection, holding the old
	// *websocket.Conn until daemon shutdown.
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	go closeOnCancel(sessionCtx, conn)

	sess := newWSSession(conn)
	defer func() {
		// Clear the twin-level handle only if it still points at THIS session
		// (a newer session may already have replaced it) and fail every
		// pending command waiter with the typed connection error.
		t.session.CompareAndSwap(sess, nil)
		sess.close(err)
	}()

	if err = t.handshake(conn, sess.sendRaw); err != nil {
		return err
	}

	if err = t.sendBootstrap(sess.sendRaw); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// Publish the session handle for command traffic: the bootstrap commands
	// are on the wire. The write barrier additionally requires
	// bootstrapDone (all bootstrap responses received).
	t.session.Store(sess)

	// Reset ping-health state for the new session: pings left unacknowledged
	// by a previous, now-dead session must not carry over and trip the
	// zombie-detection threshold moments after a fresh, healthy reconnect.
	t.outstandingPings.Store(0)
	t.pingTracker.Clear()

	pingCtx, cancelPing := context.WithCancel(sessionCtx)
	defer cancelPing()
	go t.pingLoop(pingCtx, sess)

	return t.readLoop(conn, t.newBootstrapTracker(sess.markBootstrapDone))
}

// dial establishes the WebSocket connection to HA and applies the read limit.
func (t *Twin) dial(ctx context.Context) (*websocket.Conn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: t.opts.HandshakeTimeout}

	conn, resp, err := dialer.DialContext(ctx, t.opts.URL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(t.opts.ReadLimit)

	return conn, nil
}

// closeOnCancel sends a normal-closure frame and closes conn when ctx is done.
func closeOnCancel(ctx context.Context, conn *websocket.Conn) {
	<-ctx.Done()
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"),
		time.Now().Add(2*time.Second),
	)
	_ = conn.Close()
}

// newJSONSender is retained for callers outside a full session lifecycle
// (tests). Production traffic goes through wsSession, whose id allocation and
// write share one mutex (D1).
// criticalities-remediation (M4): every write carries a deadline — a TCP
// half-open connection cannot block a WriteJSON indefinitely; the zombie
// force-close machinery observes the failure promptly instead of waiting for
// OS keepalive.
func newJSONSender(conn *websocket.Conn) func(any) error {
	var mu sync.Mutex

	return func(v any) error {
		mu.Lock()
		defer mu.Unlock()

		_ = conn.SetWriteDeadline(time.Now().Add(writeDeadline))

		return conn.WriteJSON(v)
	}
}

// pingLoop keeps the WS alive over idle periods and detects a zombie (TCP
// half-open) connection via unacknowledged pings. It returns when ctx is done,
// when a ping cannot be sent, or when the zombie threshold is exceeded.
// Ping ids come from the session's shared, strictly increasing id counter
// (twin-native-actuation D1) — a ping after a command is guaranteed a
// greater id, so HA never answers with id_reuse.
func (t *Twin) pingLoop(ctx context.Context, sess *wsSession) {
	ticker := time.NewTicker(t.opts.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.done():
			return
		case <-ticker.C:
			if _, ok := t.sendPing(sess); !ok {
				return
			}
			if t.zombieDetected() {
				_ = sess.conn.Close()

				return
			}
		}
	}
}

// sendPing allocates an id from the session counter, records and emits a
// single ping. It reports whether the write succeeded and returns the
// allocated id (0 on failure) so tests and pongs can correlate it.
func (t *Twin) sendPing(sess *wsSession) (int64, bool) {
	id, err := sess.send(map[string]any{msgTypeKey: msgTypePing})
	if err != nil {
		t.log.Warn().Err(err).Msg("HA WS ping failed")

		return 0, false
	}

	t.pingTracker.Store(id, struct{}{})
	t.outstandingPings.Add(1)

	return id, true
}

// zombieDetected reports whether unacknowledged pings exceeded the configured
// threshold, logging the decision.
func (t *Twin) zombieDetected() bool {
	outstanding := t.outstandingPings.Load()
	if outstanding <= int64(t.opts.OutstandingPingsThreshold) {
		return false
	}

	t.log.Warn().
		Int64("outstanding_pings", outstanding).
		Int("threshold", t.opts.OutstandingPingsThreshold).
		Msg("HA WS zombie connection detected (unacknowledged pings exceeded threshold) — forcing reconnect")

	return true
}

// newBootstrapTracker returns a session-local callback that marks a bootstrap
// response as received and flips the twin to ready once all of them arrived.
// onComplete, when non-nil, runs exactly once at completion — runSession uses
// it to mark the per-session bootstrap done for the command write barrier.
// The returned function is safe for concurrent use and idempotent after
// readiness has been signaled.
func (t *Twin) newBootstrapTracker(onComplete func()) func(int) {
	needed := bootstrapIDs()

	var (
		mu   sync.Mutex
		got  = make(map[int]bool, len(needed))
		done bool
	)

	return func(id int) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		got[id] = true
		for _, need := range needed {
			if !got[need] {
				return
			}
		}
		done = true
		t.markReady()
		if onComplete != nil {
			onComplete()
		}
	}
}

// markReady flips the twin to ready and closes readyCh exactly once.
func (t *Twin) markReady() {
	t.log.Info().
		Int("states", t.StateCount()).
		Int("areas", len(t.Areas())).
		Int("devices", len(t.Devices())).
		Msg("HA digital twin ready")
	t.ready.Store(true)
	t.readyOnce.Do(func() { close(t.readyCh) })
}

// readLoop consumes WS frames until the connection fails. Per-message handling
// errors are logged and do not terminate the session.
func (t *Twin) readLoop(conn *websocket.Conn, checkBootstrap func(int)) error {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		t.lastWSActivity.Store(time.Now().UnixNano())

		if err := t.handleRaw(data, checkBootstrap); err != nil {
			t.log.Warn().Err(err).Msg("HA WS message error")
		}
	}
}

// handshake completes the HA auth_required → auth → auth_ok sequence.
// criticalities-remediation (M9): the phase is bounded by
// Options.HandshakeTimeout (shared with the HTTP upgrade dialer) — a server
// that accepts the upgrade but stalls before completing the handshake causes
// a session teardown and reconnect instead of blocking the reconnect loop
// forever.
func (t *Twin) handshake(conn *websocket.Conn, send func(any) error) error {
	deadline := time.Now().Add(t.opts.HandshakeTimeout)
	_ = conn.SetReadDeadline(deadline)

	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read auth_required: %w", err)
	}
	if pickStr(msg, msgTypeKey) != msgTypeAuthReq {
		return fmt.Errorf("expected auth_required, got %q", pickStr(msg, msgTypeKey))
	}

	if err := send(map[string]any{msgTypeKey: msgTypeAuth, "access_token": t.token}); err != nil {
		return fmt.Errorf("send auth: %w", err)
	}

	if err := conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read auth reply: %w", err)
	}

	// Handshake complete — lift the deadline so the steady-state read loop
	// is not time-bounded.
	_ = conn.SetReadDeadline(time.Time{})

	switch pickStr(msg, msgTypeKey) {
	case msgTypeAuthOK:
		t.log.Info().Msg("HA WS authenticated")

		return nil
	case msgTypeAuthInv:
		return fmt.Errorf("%w: %s", ErrAuthInvalid, pickStr(msg, "message"))
	default:
		return fmt.Errorf("unexpected auth reply type %q", pickStr(msg, msgTypeKey))
	}
}

// sendBootstrap sends all initial commands to HA after authentication.
func (t *Twin) sendBootstrap(send func(any) error) error {
	cmds := []map[string]any{
		{"id": bootAreaRegistryID, msgTypeKey: "config/area_registry/list"},
		{"id": bootDeviceRegistryID, msgTypeKey: "config/device_registry/list"},
		{"id": bootEntityRegistryID, msgTypeKey: "config/entity_registry/list_for_display"},
		{"id": bootGetStatesID, msgTypeKey: "get_states"},
		{"id": bootSubscribeAllID, msgTypeKey: "subscribe_events"},
		{"id": bootGetConfigID, msgTypeKey: "get_config"},
	}
	for _, cmd := range cmds {
		if err := send(cmd); err != nil {
			return err
		}
	}

	return nil
}

// handleRaw dispatches a raw WS message. Supports the batched-array form used
// by some HA versions.
func (t *Twin) handleRaw(data []byte, checkBootstrap func(int)) error {
	data = trimSpace(data)
	if len(data) == 0 {
		return nil
	}

	if data[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		for _, item := range arr {
			if err := t.handleRaw(item, checkBootstrap); err != nil {
				t.log.Warn().Err(err).Msg("HA WS batched item error")
			}
		}

		return nil
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	switch pickStr(msg, msgTypeKey) {
	case msgTypeResult:
		return t.handleResult(msg, checkBootstrap)
	case msgTypeEvent:
		return t.handleEvent(msg)
	case msgTypePong:
		return t.handlePong(msg)
	}

	return nil
}

// handleResult processes responses to our commands. A result whose id has a
// registered command waiter is routed to it BEFORE any bootstrap handling
// (twin-native-actuation D1); a failed command reaches its caller as a typed
// value, never as a generic bootstrap warning.
func (t *Twin) handleResult(msg map[string]any, checkBootstrap func(int)) error {
	success, _ := msg["success"].(bool)
	id := asInt(msg["id"])

	if sess := t.session.Load(); sess != nil && sess.routeResult(int64(id), msg) {
		return nil
	}

	if !success {
		t.log.Warn().Int("id", id).Interface("error", msg["error"]).Msg("HA WS command failed")

		return fmt.Errorf("command id=%d failed: %v", id, msg["error"])
	}

	switch id {
	case bootAreaRegistryID:
		t.indexAreas(msg["result"])
		t.log.Debug().Int("count", len(t.Areas())).Msg("HA area registry indexed")
		checkBootstrap(bootAreaRegistryID)

	case bootDeviceRegistryID:
		t.indexDevices(msg["result"])
		t.log.Debug().Int("count", len(t.Devices())).Msg("HA device registry indexed")
		checkBootstrap(bootDeviceRegistryID)

	case bootEntityRegistryID:
		t.indexEntities(msg["result"])
		t.log.Debug().Int("count", len(t.Entities())).Msg("HA entity registry indexed")
		checkBootstrap(bootEntityRegistryID)

	case bootGetStatesID:
		t.indexStates(msg["result"])
		t.log.Debug().Int("count", t.StateCount()).Msg("HA states indexed")
		checkBootstrap(bootGetStatesID)

	case bootSubscribeAllID:
		t.log.Info().Msg("HA WS all-events subscription active")

	case bootGetConfigID:
		t.indexConfig(msg["result"])
		t.log.Debug().Msg("HA config indexed")
	}

	return nil
}

// handlePong matches an inbound pong to its outstanding ping by id, removing
// it from the tracker and decrementing the outstanding-ping counter exactly
// once per ping. Unknown or duplicate ids are ignored.
func (t *Twin) handlePong(msg map[string]any) error {
	id := int64(asInt(msg["id"]))

	if _, ok := t.pingTracker.LoadAndDelete(id); ok {
		t.outstandingPings.Add(-1)
	}

	return nil
}

// handleEvent processes incoming HA events.
func (t *Twin) handleEvent(msg map[string]any) error {
	ev := asMap(msg["event"])
	eventType := pickStr(ev, "event_type")

	switch eventType {
	case "state_changed":
		return t.handleStateChanged(ev)
	case "area_registry_updated", "device_registry_updated", "entity_registry_updated":
		t.log.Debug().Str("event", eventType).Msg("HA registry updated")
		t.onRegistryEvent()
	}

	return nil
}

// handleStateChanged updates the twin state map from a state_changed event.
func (t *Twin) handleStateChanged(ev map[string]any) error {
	data := asMap(ev["data"])
	entityID := pickStr(data, "entity_id")
	if entityID == "" {
		return nil
	}

	newState := asMap(data["new_state"])

	t.mu.Lock()
	if len(newState) == 0 {
		delete(t.states, entityID)
	} else {
		t.states[entityID] = newState
	}
	t.mu.Unlock()

	t.lastStateEvent.Store(time.Now().UnixNano())

	// Evidence collectors observe AFTER the mirror was updated (D3): a read
	// issued right after an outcome is reported sees the new state.
	if len(newState) > 0 {
		t.offerStateChanged(entityID, newState)
	}

	t.log.Trace().
		Str("entity_id", entityID).
		Str("state", pickStr(newState, "state")).
		Msg("state_changed")

	return nil
}

// trimSpace strips leading and trailing ASCII whitespace from a byte slice.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isWS(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isWS(b[end-1]) {
		end--
	}

	return b[start:end]
}

func isWS(c byte) bool { return c == ' ' || c == '\n' || c == '\r' || c == '\t' }

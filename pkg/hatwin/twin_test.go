//nolint:goconst
package hatwin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// mockServer wraps httptest.Server and delivers each new WS connection on connCh.
type mockServer struct {
	*httptest.Server
	connCh chan *websocket.Conn
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ms := &mockServer{connCh: make(chan *websocket.Conn, 8)}
	ms.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("ws upgrade: %v", err)

			return
		}
		ms.connCh <- conn
	}))

	return ms
}

func (ms *mockServer) wsURL() string {
	return "ws" + strings.TrimPrefix(ms.URL, "http")
}

// serveBootstrap performs the minimal HA WS handshake and sends all initial
// sync responses. The connection is NOT closed; callers may continue sending events.
func serveBootstrap(t *testing.T, conn *websocket.Conn, areas, devices, entities, states []any, config map[string]any) {
	t.Helper()

	wsSend(t, conn, map[string]any{"type": "auth_required", "ha_version": "2025.1"})

	var msg map[string]any
	wsRecv(t, conn, &msg)

	wsSend(t, conn, map[string]any{"type": "auth_ok"})

	// Consume the 6 bootstrap commands.
	for range 6 { // for i := 0; i < 6; i++
		wsRecv(t, conn, &msg)
	}

	// Send results in the expected order.
	// NOTE: config (bootGetConfigID) is sent before states (bootGetStatesID) so
	// that indexConfig runs before checkBootstrap marks the twin ready and closes
	// readyCh. This avoids a race where WaitReady returns before the config is
	// indexed.
	wsSend(t, conn, map[string]any{"type": "result", "id": bootAreaRegistryID, "success": true, "result": areas})
	wsSend(t, conn, map[string]any{"type": "result", "id": bootDeviceRegistryID, "success": true, "result": devices})
	wsSend(t, conn, map[string]any{"type": "result", "id": bootEntityRegistryID, "success": true, "result": map[string]any{"entities": entities}})
	if config != nil {
		wsSend(t, conn, map[string]any{"type": "result", "id": bootGetConfigID, "success": true, "result": config})
	}
	wsSend(t, conn, map[string]any{"type": "result", "id": bootGetStatesID, "success": true, "result": states})
	wsSend(t, conn, map[string]any{"type": "result", "id": bootSubscribeAllID, "success": true, "result": nil})
}

func wsSend(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Logf("ws send: %v", err)
	}
}

func wsRecv(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.ReadJSON(v); err != nil {
		t.Logf("ws recv: %v", err)
	}
}

func sendEvent(t *testing.T, conn *websocket.Conn, eventType string, data map[string]any) {
	t.Helper()
	wsSend(t, conn, map[string]any{
		"type": "event",
		"id":   bootSubscribeAllID,
		"event": map[string]any{
			"event_type": eventType,
			"data":       data,
		},
	})
}

// newReadyTwin builds a Twin connected to ms, waits for readiness, and returns it.
func newReadyTwin(t *testing.T, ms *mockServer, areas, devices, entities, states []any, config map[string]any) (*Twin, context.CancelFunc, *websocket.Conn) {
	t.Helper()

	connReady := make(chan *websocket.Conn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, areas, devices, entities, states, config)
		connReady <- conn
	}()

	twin, err := New(Options{URL: ms.wsURL(), Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	twin.Start(ctx)

	if err := twin.WaitReady(ctx); err != nil {
		cancel()
		t.Fatalf("WaitReady: %v", err)
	}

	conn := <-connReady

	return twin, cancel, conn
}

// --- tests ---

func TestTwin_InitialSync(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	areas := []any{map[string]any{"area_id": "cucina", "name": "Cucina"}}
	devices := []any{map[string]any{"id": "dev1", "name": "Frigo", "area_id": "cucina"}}
	entities := []any{map[string]any{"ei": "sensor.temperature", "di": "dev1"}}
	states := []any{
		map[string]any{
			"entity_id":    "sensor.temperature",
			"state":        "21.5",
			"attributes":   map[string]any{"device_class": "temperature", "unit_of_measurement": "°C"},
			"last_changed": "2025-01-01T00:00:00Z",
		},
	}
	config := map[string]any{"name": "Casa", "time_zone": "Europe/Rome"}

	twin, cancel, _ := newReadyTwin(t, ms, areas, devices, entities, states, config)
	defer cancel()
	defer twin.Close()

	if twin.StateCount() != 1 {
		t.Errorf("expected 1 state, got %d", twin.StateCount())
	}

	cfg := twin.Config()
	if cfg.Name != "Casa" {
		t.Errorf("config.Name: got %q, want %q", cfg.Name, "Casa")
	}
	if cfg.TimeZone != "Europe/Rome" {
		t.Errorf("config.TimeZone: got %q, want %q", cfg.TimeZone, "Europe/Rome")
	}

	snap, ok, err := twin.GetState("sensor.temperature")
	if err != nil || !ok {
		t.Fatalf("GetState: err=%v ok=%v", err, ok)
	}
	if snap.State != "21.5" {
		t.Errorf("State: got %q, want %q", snap.State, "21.5")
	}
	if snap.Domain != "sensor" {
		t.Errorf("Domain: got %q, want %q", snap.Domain, "sensor")
	}
	if snap.DeviceClass != "temperature" {
		t.Errorf("DeviceClass: got %q, want %q", snap.DeviceClass, "temperature")
	}
}

func TestTwin_StateChanged(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.humidity", "state": "55", "attributes": map[string]any{}},
	}

	twin, cancel, conn := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	sendEvent(t, conn, "state_changed", map[string]any{
		"entity_id": "sensor.humidity",
		"old_state": map[string]any{"entity_id": "sensor.humidity", "state": "55"},
		"new_state": map[string]any{"entity_id": "sensor.humidity", "state": "60", "attributes": map[string]any{}},
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap, ok, err := twin.GetState("sensor.humidity")
		if err == nil && ok && snap.State == "60" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("state did not update to 60 within 1s")
}

func TestTwin_StateChanged_EntityRemoved(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.gone", "state": "on", "attributes": map[string]any{}},
	}

	twin, cancel, conn := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	// new_state null means entity removed.
	sendEvent(t, conn, "state_changed", map[string]any{
		"entity_id": "sensor.gone",
		"old_state": map[string]any{"entity_id": "sensor.gone", "state": "on"},
		"new_state": nil,
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, ok, err := twin.GetState("sensor.gone")
		if err == nil && !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("entity was not removed from twin within 1s")
}

func TestTwin_Filter_ByDomain(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.temp", "state": "22", "attributes": map[string]any{"device_class": "temperature"}},
		map[string]any{"entity_id": "light.lamp", "state": "on", "attributes": map[string]any{}},
		map[string]any{"entity_id": "sensor.humid", "state": "55", "attributes": map[string]any{"device_class": "humidity"}},
	}

	twin, cancel, _ := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	snaps, err := twin.Filter(FilterQuery{Domain: "sensor"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("expected 2 sensor entities, got %d", len(snaps))
	}
}

func TestTwin_Filter_ByArea(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	areas := []any{map[string]any{"area_id": "sala", "name": "Sala"}}
	entities := []any{
		map[string]any{"ei": "sensor.temp_sala", "ai": "sala"},
		map[string]any{"ei": "light.lamp_sala", "ai": "sala"},
		map[string]any{"ei": "sensor.temp_cucina"},
	}
	states := []any{
		map[string]any{"entity_id": "sensor.temp_sala", "state": "22", "attributes": map[string]any{}},
		map[string]any{"entity_id": "light.lamp_sala", "state": "on", "attributes": map[string]any{}},
		map[string]any{"entity_id": "sensor.temp_cucina", "state": "23", "attributes": map[string]any{}},
	}

	twin, cancel, _ := newReadyTwin(t, ms, areas, nil, entities, states, nil)
	defer cancel()
	defer twin.Close()

	// Filter by area_id.
	snaps, _ := twin.Filter(FilterQuery{Area: "sala"})
	if len(snaps) != 2 {
		t.Errorf("by area_id: expected 2, got %d", len(snaps))
	}

	// Filter by area name (case-insensitive substring).
	snaps, _ = twin.Filter(FilterQuery{Area: "ala"})
	if len(snaps) != 2 {
		t.Errorf("by area name substring: expected 2, got %d", len(snaps))
	}
}

func TestTwin_Filter_ByDeviceClass(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.temp", "state": "22", "attributes": map[string]any{"device_class": "temperature"}},
		map[string]any{"entity_id": "sensor.moist", "state": "dry", "attributes": map[string]any{"device_class": "moisture"}},
	}

	twin, cancel, _ := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	snaps, _ := twin.Filter(FilterQuery{DeviceClass: "temperature"})
	if len(snaps) != 1 || snaps[0].EntityID != "sensor.temp" {
		t.Errorf("unexpected filter result: %v", snaps)
	}
}

func TestTwin_Filter_ByEntityIDGlob(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.temp_sala", "state": "22", "attributes": map[string]any{}},
		map[string]any{"entity_id": "sensor.temp_cucina", "state": "23", "attributes": map[string]any{}},
		map[string]any{"entity_id": "light.lamp", "state": "on", "attributes": map[string]any{}},
	}

	twin, cancel, _ := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	snaps, _ := twin.Filter(FilterQuery{EntityID: "sensor.*"})
	if len(snaps) != 2 {
		t.Errorf("glob sensor.*: expected 2, got %d", len(snaps))
	}

	snaps, _ = twin.Filter(FilterQuery{EntityID: "*sala*"})
	if len(snaps) != 1 {
		t.Errorf("glob *sala*: expected 1, got %d", len(snaps))
	}
}

func TestTwin_Filter_OnlyProblem(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.ok", "state": "22", "attributes": map[string]any{}},
		map[string]any{"entity_id": "sensor.bad", "state": "unavailable", "attributes": map[string]any{}},
		map[string]any{"entity_id": "sensor.unk", "state": "unknown", "attributes": map[string]any{}},
	}

	twin, cancel, _ := newReadyTwin(t, ms, nil, nil, nil, states, nil)
	defer cancel()
	defer twin.Close()

	snaps, _ := twin.Filter(FilterQuery{OnlyProblem: true})
	if len(snaps) != 2 {
		t.Errorf("OnlyProblem: expected 2, got %d", len(snaps))
	}
}

func TestTwin_DegradedReadiness(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	// Server completes auth but never sends bootstrap responses.
	go func() {
		conn := <-ms.connCh
		defer conn.Close()
		wsSend(t, conn, map[string]any{"type": "auth_required"})
		var msg map[string]any
		wsRecv(t, conn, &msg)
		wsSend(t, conn, map[string]any{"type": "auth_ok"})
		time.Sleep(10 * time.Second)
	}()

	twin, err := New(Options{URL: ms.wsURL(), Token: "test-token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	twin.Start(ctx)
	defer twin.Close()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer waitCancel()

	if err = twin.WaitReady(waitCtx); !errors.Is(err, ErrNotReady) {
		t.Errorf("expected ErrNotReady, got %v", err)
	}
	if twin.Ready() {
		t.Error("expected twin not ready")
	}

	_, err = twin.Filter(FilterQuery{})
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("Filter: expected ErrNotReady, got %v", err)
	}
}

func TestTwin_Reconnect(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	states := []any{
		map[string]any{"entity_id": "sensor.x", "state": "1", "attributes": map[string]any{}},
	}

	// First connection: serve bootstrap then close (triggering reconnect).
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, states, nil)
		conn.Close()
	}()

	// Second connection: serve bootstrap again (reconnect succeeds).
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, states, nil)
		// Keep alive for the rest of the test.
		time.Sleep(5 * time.Second)
		conn.Close()
	}()

	twin, err := New(Options{
		URL:            ms.wsURL(),
		Token:          "test-token",
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	twin.Start(ctx)
	defer twin.Close()

	if err := twin.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	// Allow time for the first connection to close and the second to establish.
	time.Sleep(600 * time.Millisecond)

	if !twin.Ready() {
		t.Error("expected twin still ready after reconnect")
	}
	if twin.StateCount() == 0 {
		t.Error("expected states to be populated after reconnect")
	}
}

func TestTwin_RegistryUpdateSignal(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	twin, cancel, conn := newReadyTwin(t, ms, nil, nil, nil, nil, nil)
	defer cancel()
	defer twin.Close()

	// Use a short debounce for the test.
	twin.opts.TopologyDebounce = 50 * time.Millisecond

	sendEvent(t, conn, "area_registry_updated", map[string]any{
		"action":  "created",
		"area_id": "new_room",
	})

	select {
	case <-twin.TopologyChanges():
		// received debounced signal
	case <-time.After(time.Second):
		t.Error("expected topology-change signal within 1s")
	}
}

func TestTwin_Freshness_PongClearsOutstanding(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	connReady := make(chan *websocket.Conn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		connReady <- conn
	}()

	twin, err := New(Options{
		URL:          ms.wsURL(),
		Token:        "test-token",
		PingInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	twin.Start(ctx)
	defer twin.Close()

	if err := twin.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	conn := <-connReady

	// Read the ping sent by the twin and reply with a matching pong.
	var ping map[string]any
	wsRecv(t, conn, &ping)
	if ping["type"] != "ping" {
		t.Fatalf("expected ping, got %v", ping)
	}
	wsSend(t, conn, map[string]any{"type": "pong", "id": ping["id"]})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if twin.OutstandingPings() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := twin.OutstandingPings(); got != 0 {
		t.Errorf("expected outstanding pings to clear to 0 after pong, got %d", got)
	}

	if twin.LastWSActivity().IsZero() {
		t.Error("expected LastWSActivity to be set after receiving frames")
	}
}

func TestTwin_ForceReconnectOnMissedPongs(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	// First connection: never acknowledges pings, forcing the twin to close
	// and reconnect once outstandingPings exceeds the threshold.
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
	}()

	secondConnEstablished := make(chan struct{})
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		close(secondConnEstablished)
		time.Sleep(2 * time.Second)
		conn.Close()
	}()

	twin, err := New(Options{
		URL:                       ms.wsURL(),
		Token:                     "test-token",
		PingInterval:              30 * time.Millisecond,
		OutstandingPingsThreshold: 2,
		InitialBackoff:            10 * time.Millisecond,
		MaxBackoff:                50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	twin.Start(ctx)
	defer twin.Close()

	if err := twin.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	select {
	case <-secondConnEstablished:
		// force-reconnect happened as expected
	case <-time.After(3 * time.Second):
		t.Error("expected the twin to force-reconnect after missed pongs")
	}
}

func TestTwin_ConsumeTopologyDirty_CoalescesBurst(t *testing.T) {
	ms := newMockServer(t)
	defer ms.Close()

	twin, cancel, conn := newReadyTwin(t, ms, nil, nil, nil, nil, nil)
	defer cancel()
	defer twin.Close()

	twin.opts.TopologyDebounce = 10 * time.Millisecond

	for range 5 { // i := 0; i < 5; i++
		sendEvent(t, conn, "area_registry_updated", map[string]any{
			"action":  "created",
			"area_id": "room",
		})
	}

	select {
	case <-twin.TopologyChanges():
	case <-time.After(time.Second):
		t.Fatal("expected a debounced topology-change signal")
	}

	if !twin.ConsumeTopologyDirty() {
		t.Fatal("expected topologyDirty to be set after a burst of registry events")
	}

	if twin.ConsumeTopologyDirty() {
		t.Error("expected topologyDirty to be false once consumed — a burst coalesces to a single flag, not one per event")
	}
}

func TestComputeBackoff(t *testing.T) {
	opts := Options{
		InitialBackoff: time.Second,
		MaxBackoff:     60 * time.Second,
		BackoffFactor:  2.0,
	}

	if got := computeBackoff(opts, 1); got != time.Second {
		t.Errorf("attempt 1: got %v, want 1s", got)
	}
	if got := computeBackoff(opts, 2); got != 2*time.Second {
		t.Errorf("attempt 2: got %v, want 2s", got)
	}
	if got := computeBackoff(opts, 3); got != 4*time.Second {
		t.Errorf("attempt 3: got %v, want 4s", got)
	}

	// Clamp to MaxBackoff.
	if got := computeBackoff(opts, 100); got != 60*time.Second {
		t.Errorf("attempt 100: got %v, want 60s (clamped)", got)
	}
}

//nolint:goconst
package twin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/marco-rozz/ready/pkg/hatwin"
)

const (
	wsMsgTypeKey    = "type"
	wsMsgTypeResult = "result"
)

// bootstrapResult builds a minimal HA WS "result" frame for the given
// bootstrap command id.
func bootstrapResult(id int, result any) map[string]any {
	return map[string]any{wsMsgTypeKey: wsMsgTypeResult, "id": id, "success": true, "result": result}
}

// newTestTwinReady spins a minimal HA WS mock, completes the bootstrap
// handshake, and returns a ready *hatwin.Twin plus a closer.
func newTestTwinReady(t *testing.T) (*hatwin.Twin, func()) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		_ = conn.WriteJSON(map[string]any{wsMsgTypeKey: "auth_required"})

		var msg map[string]any
		_ = conn.ReadJSON(&msg)
		_ = conn.WriteJSON(map[string]any{wsMsgTypeKey: "auth_ok"})

		// Drain the 6 bootstrap commands.
		for i := 0; i < 6; i++ {
			_ = conn.ReadJSON(&msg)
		}

		_ = conn.WriteJSON(bootstrapResult(2, []any{}))
		_ = conn.WriteJSON(bootstrapResult(3, []any{}))
		_ = conn.WriteJSON(bootstrapResult(4, map[string]any{"entities": []any{}}))
		_ = conn.WriteJSON(bootstrapResult(7, map[string]any{}))
		_ = conn.WriteJSON(bootstrapResult(5, []any{
			map[string]any{"entity_id": "sensor.test", "state": "on", "attributes": map[string]any{}},
		}))
		_ = conn.WriteJSON(bootstrapResult(6, nil))

		// Keep the connection open so the read loop (and ping goroutine) stay
		// alive for the duration of the test.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	tw, err := hatwin.New(hatwin.Options{URL: wsURL, Token: "test-token", PingInterval: time.Hour})
	if err != nil {
		t.Fatalf("hatwin.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tw.Start(ctx)

	if err := tw.WaitReady(ctx); err != nil {
		cancel()
		t.Fatalf("WaitReady: %v", err)
	}

	return tw, func() {
		cancel()
		_ = tw.Close()
	}
}

func TestQueryTool_FreshPayload(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	qt := NewQueryTool(tw, time.Minute, 5*time.Minute)

	out, err := qt.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp queryResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, out)
	}

	if resp.Stale {
		t.Error("expected stale=false for a fresh twin")
	}
	if !resp.TwinConnected {
		t.Error("expected twin_connected=true")
	}
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(resp.Results))
	}
}

func TestQueryTool_SoftStale(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	// staleThreshold shorter than the time we're about to wait; hard
	// threshold much longer so data is still served.
	qt := NewQueryTool(tw, 20*time.Millisecond, time.Minute)

	time.Sleep(50 * time.Millisecond)

	out, err := qt.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var resp queryResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (raw: %s)", err, out)
	}

	if !resp.Stale {
		t.Error("expected stale=true after exceeding stale_threshold_states")
	}
	if len(resp.Results) != 1 {
		t.Errorf("expected data still returned on soft-stale, got %d results", len(resp.Results))
	}
}

func TestQueryTool_HardStale(t *testing.T) {
	tw, closer := newTestTwinReady(t)
	defer closer()

	qt := NewQueryTool(tw, 10*time.Millisecond, 20*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	out, err := qt.InvokableRun(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected an error for hard-stale data")
	}

	var errResp staleErrorResponse
	if jsonErr := json.Unmarshal([]byte(out), &errResp); jsonErr != nil {
		t.Fatalf("unmarshal stale response: %v (raw: %s)", jsonErr, out)
	}

	if errResp.Error != "twin_stale" {
		t.Errorf("expected error=twin_stale, got %q", errResp.Error)
	}
}

func TestQueryTool_DisabledTwin(t *testing.T) {
	qt := NewQueryTool(nil, time.Minute, 5*time.Minute)

	out, err := qt.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "disabled") {
		t.Errorf("expected a disabled message, got: %s", out)
	}
}

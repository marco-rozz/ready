//nolint:goconst
package hatwin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCallServiceCorrelatesResult(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		ready <- &websocketConn{conn}
	}()

	twin, cancel := newReadyTwinFrom(t, ms)
	defer cancel()
	defer twin.Close()

	serverConn := <-ready

	resultCh := make(chan CommandResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := twin.CallService(context.Background(), "light", "turn_on", []string{"light.lampada"}, nil)
		resultCh <- res
		errCh <- err
	}()

	msg := serverConn.readCmd(t)
	if msg["type"] != "call_service" || msg["domain"] != "light" || msg["service"] != "turn_on" {
		t.Fatalf("unexpected call_service frame: %v", msg)
	}
	target, _ := msg["target"].(map[string]any)
	ids, _ := target["entity_id"].([]any)
	if len(ids) != 1 || ids[0] != "light.lampada" {
		t.Fatalf("unexpected target: %v", target)
	}

	id := int64(asInt(msg["id"]))
	serverConn.writeCmd(t, map[string]any{
		"type": "result", "id": id, "success": true,
		"result": map[string]any{"context": map[string]any{"id": "ctx-01"}},
	})

	var res CommandResult
	select {
	case res = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("CallService did not return")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("CallService: %v", err)
	}
	if !res.Success {
		t.Fatalf("Success = false, want true (%v)", res.Error)
	}
	if res.ContextID != "ctx-01" {
		t.Fatalf("ContextID = %q, want ctx-01", res.ContextID)
	}
}

func TestCallServiceDeliversHAError(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		ready <- &websocketConn{conn}
	}()

	twin, cancel := newReadyTwinFrom(t, ms)
	defer cancel()
	defer twin.Close()
	serverConn := <-ready

	errCh := make(chan error, 1)
	resCh := make(chan CommandResult, 1)
	go func() {
		res, err := twin.CallService(context.Background(), "light", "turn_on", []string{"light.rotta"}, nil)
		resCh <- res
		errCh <- err
	}()

	msg := serverConn.readCmd(t)
	serverConn.writeCmd(t, map[string]any{
		"type": "result", "id": msg["id"], "success": false,
		"error": map[string]any{"code": "service_not_found", "message": "Service light.turn_on not found"},
	})

	select {
	case res := <-resCh:
		if err := <-errCh; err != nil {
			t.Fatalf("success:false must be delivered as a value, not an error: %v", err)
		}
		if res.Success {
			t.Fatal("Success = true, want false")
		}
		if res.Error["code"] != "service_not_found" {
			t.Fatalf("Error = %v, want service_not_found", res.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallService did not return")
	}

	// The failed command must NOT be logged as a generic bootstrap failure:
	// it was routed to the waiter before the bootstrap switch. (Routing is
	// exercised by the delivery above; the bootstrap tracker is untouched.)
}

func TestSessionDropFailsPendingCommand(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		ready <- &websocketConn{conn}
	}()

	twin, cancel := newReadyTwinFrom(t, ms)
	defer cancel()
	defer twin.Close()
	serverConn := <-ready

	errCh := make(chan error, 1)
	go func() {
		_, err := twin.CallService(context.Background(), "light", "turn_on", []string{"light.lampada"}, nil)
		errCh <- err
	}()

	_ = serverConn.readCmd(t) // consume the call_service frame
	_ = serverConn.conn.Close()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("err = %v, want ErrSessionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending command did not fail on session drop")
	}
}

func TestPingAfterCommandHasGreaterID(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		ready <- &websocketConn{conn}
	}()

	twin, cancel := newReadyTwinFrom(t, ms)
	defer cancel()
	defer twin.Close()
	serverConn := <-ready

	commandID := make(chan int64, 1)
	go func() {
		_, _ = twin.CallService(context.Background(), "light", "turn_on", nil, nil)
	}()

	msg := serverConn.readCmd(t)
	if msg["type"] != "call_service" {
		t.Fatalf("expected call_service, got %v", msg["type"])
	}
	commandID <- int64(asInt(msg["id"]))

	// Answer the command, then wait for the next ping and compare ids.
	serverConn.writeCmd(t, map[string]any{
		"type": "result", "id": msg["id"], "success": true,
		"result": map[string]any{"context": map[string]any{"id": "ctx"}},
	})

	cmdID := <-commandID

	// The ping loop fires on the configured interval; wait for its frame.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var ping map[string]any
		if err := serverConn.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if err := serverConn.conn.ReadJSON(&ping); err != nil {
			t.Fatalf("server read ping: %v", err)
		}
		if ping["type"] == "ping" {
			pingID := int64(asInt(ping["id"]))
			if pingID <= cmdID {
				t.Fatalf("ping id %d not greater than command id %d (HA would answer id_reuse)", pingID, cmdID)
			}

			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no ping observed after command")
		}
	}
}

func TestConcurrentSendersKeepWireOrder(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, nil, nil, nil, nil, nil)
		ready <- &websocketConn{conn}
	}()

	twin, cancel := newReadyTwinFrom(t, ms)
	defer cancel()
	defer twin.Close()
	serverConn := <-ready

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = twin.CallService(context.Background(), "light", "turn_on", nil, nil)
		}()
	}
	wg.Wait()

	prev := int64(0)
	for i := 0; i < n; i++ {
		msg := serverConn.readCmd(t)
		if msg["type"] != "call_service" {
			t.Fatalf("frame %d: expected call_service, got %v", i, msg["type"])
		}
		id := int64(asInt(msg["id"]))
		if id <= prev {
			t.Fatalf("ids on the wire not strictly increasing: %d after %d", id, prev)
		}
		prev = id
	}
}

func TestCallServiceBarrierRefusals(t *testing.T) {
	t.Parallel()

	t.Run("no session (reconnecting)", func(t *testing.T) {
		tw := newTestTwin(t, Options{})
		_, err := tw.CallService(context.Background(), "light", "turn_on", nil, nil)
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("err = %v, want ErrNotConnected", err)
		}
	})

	t.Run("bootstrap incomplete", func(t *testing.T) {
		ms := newMockServer(t)
		defer ms.Close()

		ready := make(chan *websocketConn, 1)
		go func() {
			conn := <-ms.connCh
			serveBootstrap(t, conn, nil, nil, nil, nil, nil)
			ready <- &websocketConn{conn}
		}()

		twin, cancel := newReadyTwinFrom(t, ms)
		defer cancel()
		defer twin.Close()

		// Force the barrier's bootstrap flag down on the live session.
		if sess := twin.session.Load(); sess != nil {
			sess.bootstrapDone.Store(false)
		}
		_, err := twin.CallService(context.Background(), "light", "turn_on", nil, nil)
		if !errors.Is(err, ErrSessionNotReady) {
			t.Fatalf("err = %v, want ErrSessionNotReady", err)
		}
	})

	t.Run("silence beyond hard stale threshold", func(t *testing.T) {
		ms := newMockServer(t)
		defer ms.Close()

		ready := make(chan *websocketConn, 1)
		go func() {
			conn := <-ms.connCh
			serveBootstrap(t, conn, nil, nil, nil, nil, nil)
			ready <- &websocketConn{conn}
		}()

		twin, cancel := newReadyTwinFrom(t, ms)
		defer cancel()
		defer twin.Close()

		// Simulate a silent connection: last inbound frame far in the past.
		twin.lastWSActivity.Store(time.Now().Add(-2 * time.Hour).UnixNano())
		_, err := twin.CallService(context.Background(), "light", "turn_on", nil, nil)
		if !errors.Is(err, ErrStale) {
			t.Fatalf("err = %v, want ErrStale", err)
		}
	})
}

// --- tiny wrapper to keep the helpers above readable ---

type websocketConn struct {
	conn interface {
		ReadJSON(any) error
		WriteJSON(any) error
		Close() error
		SetReadDeadline(time.Time) error
	}
}

func (w *websocketConn) readCmd(t *testing.T) map[string]any {
	t.Helper()
	var msg map[string]any
	if err := w.conn.ReadJSON(&msg); err != nil {
		t.Fatalf("server read: %v", err)
	}

	return msg
}

func (w *websocketConn) writeCmd(t *testing.T, msg map[string]any) {
	t.Helper()
	if err := w.conn.WriteJSON(msg); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

// newReadyTwinFrom starts a twin against ms whose server goroutine (already
// running the bootstrap handshake) delivers the server-side connection on
// ready. WaitReady proves the bootstrap completed; the caller receives the
// server conn from ready itself.
func newReadyTwinFrom(t *testing.T, ms *mockServer) (*Twin, context.CancelFunc) {
	t.Helper()

	twin, err := New(Options{URL: ms.wsURL(), Token: "test-token", PingInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	twin.Start(ctx)

	if err := twin.WaitReady(ctx); err != nil {
		cancel()
		t.Fatalf("WaitReady: %v", err)
	}

	return twin, cancel
}

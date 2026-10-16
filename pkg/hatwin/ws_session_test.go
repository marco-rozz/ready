package hatwin

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testToken = "test-token"

// newTestTwin builds a Twin without starting any background loop.
func newTestTwin(t *testing.T, opts Options) *Twin {
	t.Helper()
	if opts.URL == "" {
		opts.URL = "ws://127.0.0.1:1/api/websocket"
	}
	if opts.Token == "" {
		opts.Token = testToken
	}
	tw, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return tw
}

func TestNewBootstrapTrackerReadyOnlyAfterAllIDs(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	check := tw.newBootstrapTracker(nil)
	ids := bootstrapIDs()

	for i, id := range ids[:len(ids)-1] {
		check(id)
		if tw.Ready() {
			t.Fatalf("twin became ready after %d/%d bootstrap responses", i+1, len(ids))
		}
	}

	check(ids[len(ids)-1])
	if !tw.Ready() {
		t.Fatal("twin not ready after all bootstrap responses")
	}

	select {
	case <-tw.readyCh:
	default:
		t.Fatal("readyCh not closed after bootstrap completion")
	}
}

func TestNewBootstrapTrackerIgnoresUnrelatedAndDuplicateIDs(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	check := tw.newBootstrapTracker(nil)

	check(bootSubscribeAllID)
	check(bootGetConfigID)
	check(bootAreaRegistryID)
	check(bootAreaRegistryID)
	if tw.Ready() {
		t.Fatal("twin became ready from unrelated/duplicate ids")
	}

	for _, id := range bootstrapIDs() {
		check(id)
	}
	if !tw.Ready() {
		t.Fatal("twin not ready after all bootstrap responses")
	}

	// Extra calls after completion must not panic (double close of readyCh).
	check(bootAreaRegistryID)
	check(bootGetStatesID)
}

func TestNewBootstrapTrackerIsSessionLocal(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	first := tw.newBootstrapTracker(nil)
	second := tw.newBootstrapTracker(nil)

	// Splitting the responses across two trackers must not complete either.
	first(bootAreaRegistryID)
	first(bootDeviceRegistryID)
	second(bootEntityRegistryID)
	second(bootGetStatesID)

	if tw.Ready() {
		t.Fatal("twin became ready from partial responses on two distinct trackers")
	}
}

func TestNewBootstrapTrackerConcurrent(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	check := tw.newBootstrapTracker(nil)

	var wg sync.WaitGroup
	for range 20 {
		for _, id := range bootstrapIDs() {
			wg.Go(func() {
				check(id)
			})
		}
	}
	wg.Wait()

	if !tw.Ready() {
		t.Fatal("twin not ready after concurrent bootstrap completion")
	}
}

func TestSendPingTracksOutstanding(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	sess := newWSSession(conn)

	id, ok := tw.sendPing(sess)
	if !ok {
		t.Fatal("sendPing reported failure on a successful send")
	}
	if got := tw.OutstandingPings(); got != 1 {
		t.Fatalf("outstanding pings = %d, want 1", got)
	}

	// A matching pong clears the tracker exactly once.
	if err := tw.handlePong(map[string]any{"id": id}); err != nil {
		t.Fatalf("handlePong: %v", err)
	}
	if got := tw.OutstandingPings(); got != 0 {
		t.Fatalf("outstanding pings after pong = %d, want 0", got)
	}
	if err := tw.handlePong(map[string]any{"id": id}); err != nil {
		t.Fatalf("handlePong (duplicate): %v", err)
	}
	if got := tw.OutstandingPings(); got != 0 {
		t.Fatalf("outstanding pings after duplicate pong = %d, want 0", got)
	}
}

func TestSendPingReportsSendFailure(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	ms := newMockServer(t)
	defer ms.Close()

	serverConnReady := make(chan *websocket.Conn, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			serverConnReady <- c
		}
	}))
	defer httpSrv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(httpSrv.URL, "http"), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srvConn := <-serverConnReady
	// Close with SO_LINGER 0 so the client gets an RST and its very next
	// write fails deterministically (a plain FIN would leave the write
	// succeeding into the TCP buffer).
	if tc, ok := srvConn.UnderlyingConn().(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = srvConn.Close()
	time.Sleep(50 * time.Millisecond)

	sess := newWSSession(conn)

	if _, ok := tw.sendPing(sess); ok {
		t.Fatal("sendPing reported success on a failing send")
	}
}

func TestZombieDetectedThreshold(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{OutstandingPingsThreshold: 2})
	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	sess := newWSSession(conn)

	for i := 0; i < 2; i++ {
		tw.sendPing(sess)
		if tw.zombieDetected() {
			t.Fatalf("zombie detected at %d outstanding pings, threshold is 2", tw.OutstandingPings())
		}
	}

	tw.sendPing(sess)
	if !tw.zombieDetected() {
		t.Fatalf("zombie not detected at %d outstanding pings, threshold is 2", tw.OutstandingPings())
	}
}

func TestPingLoopStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{PingInterval: time.Millisecond})
	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	sess := newWSSession(conn)
	go func() {
		defer close(done)
		tw.pingLoop(ctx, sess)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pingLoop did not return after context cancellation")
	}
}

func TestPingLoopClosesZombieConnection(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{PingInterval: time.Millisecond, OutstandingPingsThreshold: 1})
	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	done := make(chan struct{})
	sess := newWSSession(conn)
	go func() {
		defer close(done)
		// Never acknowledged: outstanding pings grow past the threshold.
		tw.pingLoop(context.Background(), sess)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pingLoop did not force-close the zombie connection")
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err == nil {
		t.Fatal("connection still usable after zombie detection")
	}
}

func TestCloseOnCancelClosesConnection(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	go closeOnCancel(ctx, conn)

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("connection still usable after context cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewJSONSenderIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	send := newJSONSender(conn)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := send(map[string]any{"id": i, msgTypeKey: msgTypePing}); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestDialFailsOnUnreachableURL(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{
		URL:              "ws://127.0.0.1:1/api/websocket",
		HandshakeTimeout: 200 * time.Millisecond,
	})

	if _, err := tw.dial(context.Background()); err == nil {
		t.Fatal("dial succeeded against an unreachable URL")
	}
}

func TestReadLoopReturnsOnConnectionClose(t *testing.T) {
	t.Parallel()

	tw := newTestTwin(t, Options{})
	ms := newMockServer(t)
	defer ms.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(ms.wsURL(), nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	srvConn := <-ms.connCh

	errCh := make(chan error, 1)
	go func() {
		errCh <- tw.readLoop(conn, tw.newBootstrapTracker(nil))
	}()

	// A malformed frame is logged, not fatal: the loop keeps running.
	if err := srvConn.WriteMessage(websocket.TextMessage, []byte("{not-json")); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case err := <-errCh:
		t.Fatalf("readLoop returned early on a malformed frame: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if tw.LastWSActivity().IsZero() {
		t.Fatal("readLoop did not record inbound WS activity")
	}

	_ = srvConn.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("readLoop returned nil error on connection close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not return after connection close")
	}
}

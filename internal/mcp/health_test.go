package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// fakeHealthCheck is a controllable HealthCheck used by HealthMonitor tests.
type fakeHealthCheck struct {
	mu        sync.Mutex
	pingErr   error
	pingCount int
	connected bool
	tools     int
}

func (f *fakeHealthCheck) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingCount++

	return f.pingErr
}

func (f *fakeHealthCheck) Connected() bool { return f.connected }
func (f *fakeHealthCheck) ToolCount() int  { return f.tools }

func (f *fakeHealthCheck) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingErr = err
}

func (f *fakeHealthCheck) pings() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.pingCount
}

// fakeReconnector counts how many times Reconnect was called.
type fakeReconnector struct {
	calls   atomic.Int64
	err     error
	started chan struct{}
	once    sync.Once
}

func (f *fakeReconnector) Reconnect(_ context.Context) error {
	f.calls.Add(1)
	f.once.Do(func() {
		if f.started != nil {
			close(f.started)
		}
	})

	return f.err
}

func TestHealthMonitor_PingsOnInterval(t *testing.T) {
	fc := &fakeHealthCheck{connected: true, tools: 3}
	hm := NewHealthMonitor(fc, nil, 20*time.Millisecond)
	hm.logger = discardLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)

	time.Sleep(110 * time.Millisecond)
	cancel()
	hm.Stop()

	if got := fc.pings(); got < 3 {
		t.Errorf("expected at least 3 pings in 110ms, got %d", got)
	}
}

func TestHealthMonitor_TriggersReconnectOnPingFailure(t *testing.T) {
	fc := &fakeHealthCheck{connected: false}
	fc.setErr(errors.New("simulated ping failure"))

	rec := &fakeReconnector{started: make(chan struct{})}

	hm := NewHealthMonitor(fc, rec, 10*time.Millisecond)
	hm.logger = discardLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)

	select {
	case <-rec.started:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Reconnect was not called after ping failure")
	}

	cancel()
	hm.Stop()

	if rec.calls.Load() < 1 {
		t.Errorf("expected at least 1 reconnect call, got %d", rec.calls.Load())
	}
}

func TestHealthMonitor_IncrementsErrorCount(t *testing.T) {
	fc := &fakeHealthCheck{}
	fc.setErr(errors.New("boom"))

	hm := NewHealthMonitor(fc, nil, 5*time.Millisecond)
	hm.logger = discardLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)

	time.Sleep(30 * time.Millisecond)
	cancel()
	hm.Stop()

	status := hm.Status()
	if status.ErrorCount < 1 {
		t.Errorf("expected ErrorCount >= 1, got %d", status.ErrorCount)
	}
}

func TestHealthMonitor_Status(t *testing.T) {
	fc := &fakeHealthCheck{connected: true, tools: 7}
	hm := NewHealthMonitor(fc, nil, 10*time.Millisecond)
	hm.logger = discardLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hm.Start(ctx)

	// Let the loop tick at least once.
	time.Sleep(30 * time.Millisecond)
	cancel()
	hm.Stop()

	status := hm.Status()
	if !status.Connected {
		t.Error("expected Connected=true")
	}
	if status.ToolCount != 7 {
		t.Errorf("expected ToolCount=7, got %d", status.ToolCount)
	}
	if status.LastPingAt.IsZero() {
		t.Error("expected non-zero LastPingAt")
	}
	if status.Uptime <= 0 {
		t.Error("expected positive Uptime")
	}
}

func TestNewHealthMonitorFromConfig_DisabledReturnsNil(t *testing.T) {
	hm := NewHealthMonitorFromConfig(&fakeHealthCheck{}, nil, config.MCPHealthCheckConfig{Enabled: false})
	if hm != nil {
		t.Error("expected nil monitor when health checks disabled")
	}
}

func TestNewHealthMonitorFromConfig_EnabledUsesConfigInterval(t *testing.T) {
	hm := NewHealthMonitorFromConfig(
		&fakeHealthCheck{},
		nil,
		config.MCPHealthCheckConfig{Enabled: true, Interval: 123 * time.Millisecond},
	)
	if hm == nil {
		t.Fatal("expected non-nil monitor")
	}
	if hm.interval != 123*time.Millisecond {
		t.Errorf("expected interval 123ms, got %v", hm.interval)
	}
}

func TestHealthMonitor_StopIsIdempotent(_ *testing.T) {
	hm := NewHealthMonitor(&fakeHealthCheck{}, nil, 10*time.Millisecond)
	hm.logger = discardLogger()

	hm.Start(context.Background())
	hm.Stop()
	hm.Stop() // must not panic or block
}

// TestHealthMonitor_PermanentAuthCondition_DoesNotReconnectAndLogsOnce
// exercises the design doc/task 6.8: on a permanent authorization condition the
// monitor never calls Reconnect (it would immediately short-circuit on the
// same sentinel) and logs the failure once — not once per tick — across
// many ticks while the condition persists.
func TestHealthMonitor_PermanentAuthCondition_DoesNotReconnectAndLogsOnce(t *testing.T) {
	fc := &fakeHealthCheck{connected: true}
	fc.setErr(ErrMCPOAuthLoginRequired)

	rec := &fakeReconnector{}

	var buf syncBuffer

	hm := NewHealthMonitor(fc, rec, 5*time.Millisecond)
	hm.logger = zerolog.New(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)
	time.Sleep(80 * time.Millisecond) // many ticks
	cancel()
	hm.Stop()

	if pings := fc.pings(); pings < 5 {
		t.Fatalf("expected several pings, got %d — the monitor must keep pinging silently", pings)
	}
	if rec.calls.Load() != 0 {
		t.Fatalf("Reconnect was called %d times, want 0 (it would short-circuit on the same sentinel)", rec.calls.Load())
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	count := 0
	for _, l := range lines {
		if strings.Contains(l, "permanent authorization condition") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("logged the permanent-authorization-condition message %d times, want exactly 1 (not once per tick)", count)
	}
}

// TestHealthMonitor_LogsRecoveryOnceAfterAuthCondition exercises task 6.9:
// once a permanent authorization condition clears, the monitor logs the
// recovery transition exactly once, and resumes calling Reconnect on any
// later transient failure (proving it left the permanent-condition state).
func TestHealthMonitor_LogsRecoveryOnceAfterAuthCondition(t *testing.T) {
	fc := &fakeHealthCheck{connected: true}
	fc.setErr(ErrMCPOAuthLoginRequired)

	rec := &fakeReconnector{}

	var buf syncBuffer

	hm := NewHealthMonitor(fc, rec, 5*time.Millisecond)
	hm.logger = zerolog.New(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)
	time.Sleep(30 * time.Millisecond) // a few failing ticks

	// `ready login` runs out of band: the credential is fixed.
	fc.setErr(nil)
	time.Sleep(30 * time.Millisecond) // a few recovered ticks

	// A later, unrelated transient failure must still drive Reconnect —
	// proving the monitor is no longer latched in the auth-condition state.
	fc.setErr(errors.New("simulated transient failure"))
	time.Sleep(30 * time.Millisecond)

	cancel()
	hm.Stop()

	if rec.calls.Load() == 0 {
		t.Fatal("expected Reconnect to be called for the later transient failure")
	}

	recoveryCount := strings.Count(buf.String(), "recovered after a permanent authorization condition")
	if recoveryCount != 1 {
		t.Fatalf("logged the recovery transition %d times, want exactly 1", recoveryCount)
	}
}

func TestClient_Ping_InProcess(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Ping(ctx); err != nil {
		t.Errorf("Ping failed: %v", err)
	}

	if !client.Connected() {
		t.Error("expected Connected=true")
	}

	if client.ToolCount() == 0 {
		t.Error("expected ToolCount > 0")
	}
}

package mcp

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// ---------------------------------------------------------------------------
// Minimal stub that satisfies Runtime for composite tests.
// ---------------------------------------------------------------------------

type stubRuntime struct {
	tools     []tool.BaseTool
	pingErr   error
	connected bool
	closed    bool
}

func (s *stubRuntime) Tools() []tool.BaseTool { return s.tools }

func (s *stubRuntime) Connected() bool { return s.connected }

func (s *stubRuntime) ToolCount() int { return len(s.tools) }

func (s *stubRuntime) Close() error {
	s.closed = true

	return nil
}

func (s *stubRuntime) Ping(_ context.Context) error { return s.pingErr }

// stubMonitor counts how many times Stop is called.
type stubMonitor struct{ stopped bool }

func (m *stubMonitor) Stop()                   { m.stopped = true }
func (m *stubMonitor) Start(_ context.Context) {}

// buildTool builds a named tool returning a fixed display name.
func buildTool(name string) tool.BaseTool {
	return &namedTool{name: name}
}

// namedTool is a minimal tool.BaseTool that returns a fixed name from Info().
type namedTool struct{ name string }

func (n *namedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: n.name}, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCompositeRuntime_ToolsAggregation(t *testing.T) {
	toolA := buildTool("ha__light/turn_on")
	toolB := buildTool("media__play")
	toolC := buildTool("ha__light/turn_off")

	a := &stubRuntime{tools: []tool.BaseTool{toolA, toolC}}
	b := &stubRuntime{tools: []tool.BaseTool{toolB}}

	cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
		{Runtime: a, Tools: a.tools},
		{Runtime: b, Tools: b.tools},
	}, discardLogger())

	got := cr.Tools()
	if len(got) != 3 {
		t.Fatalf("Tools() len = %d, want 3", len(got))
	}
}

func TestCompositeRuntime_ClosePropagatesToAll(t *testing.T) {
	monA := &stubMonitor{}
	monB := &stubMonitor{}
	rtA := &stubRuntime{}
	rtB := &stubRuntime{}

	cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
		{Runtime: rtA, Monitor: (*HealthMonitor)(nil)}, // explicit nil monitor
		{Runtime: rtB, Monitor: nil},
	}, discardLogger())
	// Replace with actual monitor stubs; we can't use stopMonitor directly
	// because HealthMonitor.Stop() uses a channel. So we test with nil monitors
	// and verify runtime Close() is called.
	_ = monA
	_ = monB

	if err := cr.Close(); err != nil {
		t.Errorf("Close() unexpected error: %v", err)
	}

	if !rtA.closed {
		t.Error("runtime A was not closed")
	}

	if !rtB.closed {
		t.Error("runtime B was not closed")
	}
}

func TestCompositeRuntime_PingAnySemantics(t *testing.T) {
	ctx := context.Background()
	errPing := errors.New("ping failed")

	t.Run("at least one succeeds", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
			{Runtime: &stubRuntime{pingErr: errPing}, Tools: nil},
			{Runtime: &stubRuntime{pingErr: nil}, Tools: nil},
		}, discardLogger())

		if err := cr.Ping(ctx); err != nil {
			t.Errorf("Ping() = %v, want nil (any-success semantics)", err)
		}
	})

	t.Run("all fail returns error", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
			{Runtime: &stubRuntime{pingErr: errPing}, Tools: nil},
			{Runtime: &stubRuntime{pingErr: errPing}, Tools: nil},
		}, discardLogger())

		if err := cr.Ping(ctx); err == nil {
			t.Error("Ping() = nil, want error when all runtimes fail")
		}
	})

	t.Run("empty composite returns nil", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), nil, discardLogger())

		if err := cr.Ping(ctx); err != nil {
			t.Errorf("Ping() on empty composite = %v, want nil", err)
		}
	})
}

func TestCompositeRuntime_ConnectedAnySemantics(t *testing.T) {
	t.Run("at least one connected", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
			{Runtime: &stubRuntime{connected: false}, Tools: nil},
			{Runtime: &stubRuntime{connected: true}, Tools: nil},
		}, discardLogger())

		if !cr.Connected() {
			t.Error("Connected() = false, want true (any-success semantics)")
		}
	})

	t.Run("all disconnected", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
			{Runtime: &stubRuntime{connected: false}, Tools: nil},
		}, discardLogger())

		if cr.Connected() {
			t.Error("Connected() = true, want false when all disconnected")
		}
	})

	t.Run("empty composite returns false", func(t *testing.T) {
		cr := NewCompositeRuntime(context.Background(), nil, discardLogger())
		if cr.Connected() {
			t.Error("Connected() on empty composite should be false")
		}
	})
}

func TestCompositeRuntime_ToolCountSum(t *testing.T) {
	toolA := buildTool("a")
	toolB := buildTool("b")
	toolC := buildTool("c")

	a := &stubRuntime{tools: []tool.BaseTool{toolA, toolB}}
	b := &stubRuntime{tools: []tool.BaseTool{toolC}}

	cr := NewCompositeRuntime(context.Background(), []CompositeEntry{
		{Runtime: a, Tools: a.tools},
		{Runtime: b, Tools: b.tools},
	}, discardLogger())

	if got := cr.ToolCount(); got != 3 {
		t.Errorf("ToolCount() = %d, want 3", got)
	}
}

// TestDetectToolNameCollisions verifies task 4.1/4.3:
// - no collision → no warning log
// - duplicate display name → Warn emitted
func TestDetectToolNameCollisions(t *testing.T) {
	t.Run("no collision produces no warning", func(t *testing.T) {
		var buf bytes.Buffer
		logger := zerolog.New(&buf)

		tools := []tool.BaseTool{
			buildTool("ha__light/turn_on"),
			buildTool("media__play"),
		}

		detectToolNameCollisions(context.Background(), tools, logger)

		if buf.Len() != 0 {
			t.Errorf("expected no log output, got: %s", buf.String())
		}
	})

	t.Run("duplicate display name emits warning", func(t *testing.T) {
		var buf bytes.Buffer
		logger := zerolog.New(&buf).Level(zerolog.WarnLevel)

		// Two tools with the same display name (e.g. both have no prefix set).
		tools := []tool.BaseTool{
			buildTool("light/turn_on"),
			buildTool("light/turn_on"),
		}

		detectToolNameCollisions(context.Background(), tools, logger)

		if buf.Len() == 0 {
			t.Error("expected warning log for duplicate tool name, got none")
		}

		logged := buf.String()
		if !contains(logged, "duplicate") {
			t.Errorf("expected 'duplicate' in log output, got: %s", logged)
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsRune(s, substr))
}

func containsRune(s, substr string) bool {
	for i := range s {
		if i+len(substr) <= len(s) && s[i:i+len(substr)] == substr {
			return true
		}
	}

	return false
}

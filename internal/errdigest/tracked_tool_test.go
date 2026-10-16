package errdigest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/errdigest"
	hometools "github.com/marco-rozz/ready/internal/tools"
	"github.com/rs/zerolog"
)

type stubTool struct {
	name   string
	result string
	err    error
}

var _ tool.InvokableTool = (*stubTool)(nil)

func (s *stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return s.result, s.err
}

func sessionFn(_ context.Context) string { return "test-session" }

func TestTrackedTool_SuccessPassesThrough(t *testing.T) {
	tracked := errdigest.NewTrackedTool(&stubTool{name: "ok", result: "data"}, errdigest.NewClassifier(), sessionFn, zerolog.Nop())

	result, err := tracked.InvokableRun(context.Background(), `{}`)
	if err != nil || result != "data" {
		t.Fatalf("got (%q, %v), want (\"data\", nil)", result, err)
	}
}

// A failing inner tool yields a severity-tagged observation, never a Go
// error, so Eino's ToolNode does not abort the ReAct turn.
func TestTrackedTool_ErrorSurfacedAsObservation(t *testing.T) {
	sentinel := errors.New("connection lost")
	classifier := errdigest.NewClassifier().Register(sentinel, errdigest.Transient)
	failing := &stubTool{name: "home_assistant__light_turn_on", err: sentinel}

	result, err := errdigest.NewTrackedTool(failing, classifier, sessionFn, zerolog.Nop()).InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("TrackedTool must not propagate a Go error; got %v", err)
	}

	if result != "[TRANSIENT] connection lost" {
		t.Errorf("observation = %q, want %q", result, "[TRANSIENT] connection lost")
	}
}

func TestTrackedTool_UnclassifiedErrorIsUnknown(t *testing.T) {
	failing := &stubTool{name: "x", err: errors.New("tool returned error result")}

	result, _ := errdigest.NewTrackedTool(failing, errdigest.NewClassifier(), sessionFn, zerolog.Nop()).InvokableRun(context.Background(), `{}`)
	if !strings.HasPrefix(result, "[UNKNOWN] ") || !strings.Contains(result, "tool returned error result") {
		t.Errorf("observation = %q, want [UNKNOWN] prefix and the original message", result)
	}
}

// The deadline-aware wait refusal reaches the model as a transient
// observation (task-plan-resilience: deadline-aware wait tool).
func TestTrackedTool_RefusedWaitIsTransientObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	classifier := errdigest.NewClassifier().Register(hometools.ErrWaitExceedsTurnBudget, errdigest.Transient)

	result, err := errdigest.NewTrackedTool(hometools.NewWaitTool(), classifier, sessionFn, zerolog.Nop()).InvokableRun(ctx, `{"seconds":2}`)
	if err != nil {
		t.Fatalf("refusal must reach Eino as an observation: %v", err)
	}

	if !strings.HasPrefix(result, "[TRANSIENT] ") {
		t.Fatalf("result = %q, want transient observation", result)
	}
}

func TestWrapTools_PassesNonInvokableThrough(t *testing.T) {
	var base tool.BaseTool = baseOnly{}

	out := errdigest.WrapTools([]tool.BaseTool{&stubTool{name: "a"}, base}, errdigest.NewClassifier(), sessionFn, zerolog.Nop())
	if _, ok := out[0].(*errdigest.TrackedTool); !ok {
		t.Errorf("invokable tool not wrapped: %T", out[0])
	}

	if out[1] != base {
		t.Errorf("non-invokable tool must pass through unchanged")
	}
}

type baseOnly struct{}

func (baseOnly) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "base"}, nil
}

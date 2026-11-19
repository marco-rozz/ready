package errdigest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/activity"
	"github.com/marco-rozz/ready/internal/errdigest"
	hometools "github.com/marco-rozz/ready/internal/tools"
	"github.com/rs/zerolog"
)

type stubTool struct {
	name   string
	result string
	err    error
	calls  int
}

var _ tool.InvokableTool = (*stubTool)(nil)

func (s *stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.name}, nil
}

func (s *stubTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	s.calls++

	return s.result, s.err
}

func TestTrackedTool_GuardStopsPermanentDuplicate(t *testing.T) {
	failure := errors.New("invalid")
	inner := &stubTool{name: "test", err: failure}
	tracked := errdigest.NewTrackedTool(inner, errdigest.NewClassifier().Register(failure, errdigest.Permanent), sessionFn, zerolog.Nop())
	ctx := errdigest.WithTurnGuard(context.Background(), 8)
	if _, err := tracked.InvokableRun(ctx, `{"a":1,"b":2}`); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.InvokableRun(ctx, `{"b":2,"a":1}`); !errors.Is(err, errdigest.ErrGuardRepeated) {
		t.Fatalf("duplicate: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("executed %d times", inner.calls)
	}
	if _, err := tracked.InvokableRun(errdigest.WithTurnGuard(context.Background(), 8), `{"a":1,"b":2}`); err != nil {
		t.Fatal(err)
	}
}

type countedFailingTool struct {
	calls atomic.Int32
	err   error
}

func (s *countedFailingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "counted"}, nil
}

func (s *countedFailingTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	s.calls.Add(1)

	return "", s.err
}

func TestTrackedToolGuardParallelAndUnrelatedSuccess(t *testing.T) {
	failure := errors.New("permanent")
	inner := &countedFailingTool{err: failure}
	tracked := errdigest.NewTrackedTool(inner, errdigest.NewClassifier().Register(failure, errdigest.Permanent), sessionFn, zerolog.Nop())
	ctx := errdigest.WithTurnGuard(context.Background(), 32)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tracked.InvokableRun(ctx, `{"x":1}`)
			if err != nil && !errors.Is(err, errdigest.ErrGuardRepeated) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("tool executed %d times", got)
	}
	other := errdigest.NewTrackedTool(&stubTool{name: "unrelated", result: "ok"}, errdigest.NewClassifier(), sessionFn, zerolog.Nop())
	if _, err := other.InvokableRun(ctx, `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.InvokableRun(ctx, `{"x":1}`); !errors.Is(err, errdigest.ErrGuardRepeated) {
		t.Fatalf("unrelated success reset guard: %v", err)
	}
	if _, err := tracked.InvokableRun(ctx, `{"x":2}`); err != nil {
		t.Fatalf("different arguments were suppressed: %v", err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Fatalf("changed invocation count: %d", got)
	}
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

func TestTrackedToolActivityGuardRefusal(t *testing.T) {
	failure := errors.New("permanent")
	inner := &stubTool{name: "tool", err: failure}
	tracked := errdigest.NewTrackedTool(inner, errdigest.NewClassifier().Register(failure, errdigest.Permanent), sessionFn, zerolog.Nop())
	tracker := activity.New()
	defer tracker.Close()
	ctx := activity.WithTracker(errdigest.WithTurnGuard(context.Background(), 8), tracker)
	if _, err := tracked.InvokableRun(ctx, `{}`); err != nil {
		t.Fatal(err)
	}
	if s := <-tracker.Updates(); s.Sequence != 2 || s.ToolName != "" {
		t.Fatalf("unexpected final activity: %+v", s)
	}
	if _, err := tracked.InvokableRun(ctx, `{}`); !errors.Is(err, errdigest.ErrGuardRepeated) {
		t.Fatalf("guard: %v", err)
	}
	select {
	case s := <-tracker.Updates():
		t.Fatalf("guard refusal published: %+v", s)
	default:
	}
}

type panicActivityTool struct{}

func (*panicActivityTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "panic_tool"}, nil
}

func (*panicActivityTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	panic("tool panic")
}

func TestTrackedToolActivityClearsOnPanic(t *testing.T) {
	tracker := activity.New()
	defer tracker.Close()
	tracked := errdigest.NewTrackedTool(&panicActivityTool{}, errdigest.NewClassifier(), sessionFn, zerolog.Nop())
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic")
			}
		}()
		_, _ = tracked.InvokableRun(activity.WithTracker(context.Background(), tracker), `{}`)
	}()
	if s := <-tracker.Updates(); s.Sequence != 2 || s.ToolName != "" {
		t.Fatalf("panic left tool active: %+v", s)
	}
}

func TestTrackedToolActivityExposesNameOnly(t *testing.T) {
	inner := &stubTool{name: "visible", result: "private_result"}
	tracked := errdigest.NewTrackedTool(inner, errdigest.NewClassifier(), sessionFn, zerolog.Nop())
	tracker := activity.New()
	defer tracker.Close()
	result, err := tracked.InvokableRun(activity.WithTracker(context.Background(), tracker), `{"secret":"private_argument"}`)
	if err != nil || result != "private_result" {
		t.Fatalf("tool result %q: %v", result, err)
	}
	snapshot := <-tracker.Updates()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private_") {
		t.Fatalf("activity leaked data: %s", encoded)
	}
}

type dualActivityTool struct{ failure error }

func (*dualActivityTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "dual_tool"}, nil
}

func (t *dualActivityTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "", t.failure
}

func (*dualActivityTool) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	return schema.StreamReaderFromArray([]string{"result"}), nil
}

func TestDualToolPreservesInterfacesAndGuard(t *testing.T) {
	failure := errors.New("permanent failure")
	inner := &dualActivityTool{failure: failure}
	wrapped := errdigest.WrapTools([]tool.BaseTool{inner}, errdigest.NewClassifier().Register(failure, errdigest.Permanent), sessionFn, zerolog.Nop())[0]
	invokable, ok := wrapped.(tool.InvokableTool)
	if !ok {
		t.Fatal("missing invokable interface")
	}
	streamable, ok := wrapped.(tool.StreamableTool)
	if !ok {
		t.Fatal("missing streamable interface")
	}
	tracker := activity.New()
	defer tracker.Close()
	ctx := activity.WithTracker(errdigest.WithTurnGuard(context.Background(), 8), tracker)
	if _, err := invokable.InvokableRun(ctx, `{}`); err != nil {
		t.Fatal(err)
	}
	<-tracker.Updates()
	if _, err := streamable.StreamableRun(ctx, `{}`); !errors.Is(err, errdigest.ErrGuardRepeated) {
		t.Fatalf("stream bypassed guard: %v", err)
	}
	select {
	case got := <-tracker.Updates():
		t.Fatalf("guard refusal emitted activity: %+v", got)
	default:
	}
}

type enhancedActivityTool struct{}

func (*enhancedActivityTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "enhanced_tool"}, nil
}

func (*enhancedActivityTool) InvokableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
	return &schema.ToolResult{}, nil
}

func (*enhancedActivityTool) StreamableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	return schema.StreamReaderFromArray([]*schema.ToolResult{{}}), nil
}

func TestEnhancedToolPreservesInterfacesAndActivity(t *testing.T) {
	wrapped := errdigest.WrapTools([]tool.BaseTool{&enhancedActivityTool{}}, errdigest.NewClassifier(), sessionFn, zerolog.Nop())[0]
	invokable, ok := wrapped.(tool.EnhancedInvokableTool)
	if !ok {
		t.Fatal("missing enhanced invokable interface")
	}
	streamable, ok := wrapped.(tool.EnhancedStreamableTool)
	if !ok {
		t.Fatal("missing enhanced streamable interface")
	}
	tracker := activity.New()
	defer tracker.Close()
	ctx := activity.WithTracker(context.Background(), tracker)
	if _, err := invokable.InvokableRun(ctx, &schema.ToolArgument{Text: `{}`}); err != nil {
		t.Fatal(err)
	}
	if got := <-tracker.Updates(); got.Sequence != 2 || got.ToolName != "" {
		t.Fatalf("invocation activity: %+v", got)
	}
	reader, err := streamable.StreamableRun(ctx, &schema.ToolArgument{Text: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-tracker.Updates(); got.ToolName != "enhanced_tool" {
		t.Fatalf("stream start: %+v", got)
	}
	reader.Close()
	if got := <-tracker.Updates(); got.Sequence != 4 || got.ToolName != "" {
		t.Fatalf("stream end: %+v", got)
	}
}

type failingStreamDual struct{ failure error }

func (*failingStreamDual) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "failing_stream"}, nil
}

func (*failingStreamDual) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "ok", nil
}

func (t *failingStreamDual) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	reader, writer := schema.Pipe[string](0)
	go func() { defer writer.Close(); writer.Send("", t.failure) }()

	return reader, nil
}

func TestDualStreamPermanentFailureFeedsGuard(t *testing.T) {
	failure := errors.New("permanent stream failure")
	wrapped := errdigest.WrapTools([]tool.BaseTool{&failingStreamDual{failure: failure}}, errdigest.NewClassifier().Register(failure, errdigest.Permanent), sessionFn, zerolog.Nop())[0]
	streamable, ok := wrapped.(tool.StreamableTool)
	if !ok {
		t.Fatal("missing streamable interface")
	}
	ctx := errdigest.WithTurnGuard(context.Background(), 8)
	reader, err := streamable.StreamableRun(ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Recv(); !errors.Is(err, failure) {
		t.Fatalf("stream error: %v", err)
	}
	reader.Close()
	if _, err = streamable.StreamableRun(ctx, `{}`); !errors.Is(err, errdigest.ErrGuardRepeated) {
		t.Fatalf("guard did not retain permanent stream error: %v", err)
	}
}

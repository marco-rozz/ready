package agent_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/activity"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/rs/zerolog"
)

var errToolStop = errors.New("tool stop sentinel")

type stoppingEchoTool struct{ echoTool }

func (stoppingEchoTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "", errToolStop
}

func TestToolErrorPropagationBothProtocols(t *testing.T) {
	ctx := context.Background()
	t.Run("chat", func(t *testing.T) {
		m := &scenarioMockModel{responses: []mockResponse{{message: &schema.Message{}, toolCalls: []schema.ToolCall{echoCall("call_1")}}}}
		a, err := agent.New(ctx, agent.Config{ChatModel: m, Tools: []tool.BaseTool{stoppingEchoTool{}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("test")}, nil)
		if !errors.Is(err, errToolStop) {
			t.Fatalf("tool sentinel lost: %v", err)
		}
	})
	t.Run("responses", func(t *testing.T) {
		m := &scenarioAgenticModel{responses: []agenticScenarioResponse{{message: agenticEchoToolCall()}}}
		a, err := agent.NewAgentic(ctx, agent.AgenticConfig{Model: m, Tools: []tool.BaseTool{stoppingEchoTool{}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("test")}, nil)
		if !errors.Is(err, errToolStop) {
			t.Fatalf("tool sentinel lost: %v", err)
		}
	})
}

type countedStoppingTool struct {
	echoTool
	calls atomic.Int32
}

func (s *countedStoppingTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	s.calls.Add(1)

	return "", errToolStop
}

func TestGuardStopsRepeatedCallBothProtocols(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			inner := &countedStoppingTool{}
			tracked := errdigest.NewTrackedTool(inner, errdigest.NewClassifier().Register(errToolStop, errdigest.Permanent), func(context.Context) string { return "test" }, zerolog.Nop())
			tracker := activity.New()
			defer tracker.Close()
			ctx := activity.WithTracker(errdigest.WithTurnGuard(context.Background(), 8), tracker)
			var run func() error
			if protocol == "chat" {
				m := &scenarioMockModel{responses: []mockResponse{{message: &schema.Message{}, toolCalls: []schema.ToolCall{echoCall("call_1")}}}}
				a, err := agent.New(ctx, agent.Config{ChatModel: m, Tools: []tool.BaseTool{tracked}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				run = func() error {
					_, err := a.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("test")}, nil)

					return err
				}
			} else {
				m := &scenarioAgenticModel{responses: []agenticScenarioResponse{{message: agenticEchoToolCall()}}}
				a, err := agent.NewAgentic(ctx, agent.AgenticConfig{Model: m, Tools: []tool.BaseTool{tracked}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				run = func() error {
					_, err := a.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("test")}, nil)

					return err
				}
			}
			if err := run(); !errors.Is(err, errdigest.ErrGuardRepeated) {
				t.Fatalf("guard stop: %v", err)
			}
			if n := inner.calls.Load(); n != 1 {
				t.Fatalf("tool ran %d times", n)
			}
			if snapshot := <-tracker.Updates(); snapshot.Sequence != 2 || snapshot.ToolName != "" {
				t.Fatalf("activity: %+v", snapshot)
			}
		})
	}
}

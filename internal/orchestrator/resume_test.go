package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
)

func staticAssemble(llm.PromptLayout) []*schema.Message {
	return []*schema.Message{schema.UserMessage("hi")}
}

// session-transcript-persistence §D10: with recorded, complete tool-call
// groups the turn is resume-ready — an advanceable failure moves on to the
// next candidate, which re-assembles from the live history. Tool activity of
// the failed attempt stays visible on the final outcome.
func TestRunTurn_ResumeAfterRecordedGroups(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{
		outcome: agent.TurnOutcome{ToolCallStarted: true, ToolCallCompleted: true},
		err:     errors.New("error 401: unauthorized"),
	}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("resumed")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	assembled := 0
	outcome, err := o.RunTurn(context.Background(), TurnRequest{
		SessionKey: testTurnChannel,
		Assemble: func(l llm.PromptLayout) []*schema.Message {
			assembled++

			return staticAssemble(l)
		},
		ResumeReady: func() bool { return true },
	}, func(string) {})
	if err != nil {
		t.Fatalf("expected the fallback to resume the turn, got %v", err)
	}
	if outcome.Reply != "resumed" || r1.callCount() != 1 || assembled != 2 {
		t.Fatalf("reply=%q fallback calls=%d assembles=%d", outcome.Reply, r1.callCount(), assembled)
	}
	if !outcome.ToolCallStarted || !outcome.ToolCallCompleted {
		t.Errorf("expected the earlier attempt's tool activity merged into the outcome, got %+v", outcome)
	}
}

// An open tool-call group (a call recorded without its result) keeps the
// turn unsafe: no retry, no other candidate.
func TestRunTurn_OpenGroupKeepsTurnUnsafe(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{
		outcome: agent.TurnOutcome{ToolCallStarted: true},
		err:     errors.New("error 500: boom"),
	}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("must not run")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	_, err := o.RunTurn(context.Background(), TurnRequest{
		SessionKey:  testTurnChannel,
		Assemble:    staticAssemble,
		ResumeReady: func() bool { return false },
	}, func(string) {})
	if err == nil {
		t.Fatal("expected the original error")
	}
	if r0.callCount() != 1 || r1.callCount() != 0 {
		t.Fatalf("calls = primary %d fallback %d, want 1/0", r0.callCount(), r1.callCount())
	}
}

// Delivered final-answer content blocks every further attempt even when the
// transcript is resume-ready.
func TestRunTurn_DeliveredContentBlocksResume(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{
		outcome: agent.TurnOutcome{ContentDelivered: true},
		err:     errors.New("error 500: boom"),
	}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("must not run")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	_, err := o.RunTurn(context.Background(), TurnRequest{
		SessionKey:  testTurnChannel,
		Assemble:    staticAssemble,
		ResumeReady: func() bool { return true },
	}, func(string) {})
	if err == nil {
		t.Fatal("expected the original error")
	}
	if r1.callCount() != 0 {
		t.Fatal("no candidate may run after content was delivered")
	}
}

// ── Cross-protocol resume with real agents ────────────────────────────────

// scriptedChatModel serves scripted responses; validate, when set, inspects
// every model input first and can reject it (the strict-wire mock).
type scriptedChatModel struct {
	mu       sync.Mutex
	calls    int
	steps    []func() (*schema.Message, error)
	validate func([]*schema.Message) error
	inputs   [][]*schema.Message
}

var _ model.ToolCallingChatModel = (*scriptedChatModel)(nil)

func (m *scriptedChatModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.inputs = append(m.inputs, input)
	if m.validate != nil {
		if err := m.validate(input); err != nil {
			return nil, err
		}
	}

	idx := min(m.calls, len(m.steps)-1)
	m.calls++

	return m.steps[idx]()
}

func (m *scriptedChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *scriptedChatModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// countingTool counts real executions.
type countingTool struct{ runs atomic.Int32 }

func (t *countingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "light_on", Desc: "turns a light on"}, nil
}

func (t *countingTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	t.runs.Add(1)

	return "ok", nil
}

// strictToolPairing mirrors chat_completions / anthropic / responses
// request-shape rules: every tool call has a non-empty id and every tool
// result references a call earlier in the same prompt.
func strictToolPairing(input []*schema.Message) error {
	seen := map[string]bool{}
	for _, m := range input {
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				return errors.New("error 400: invalid_request_error: tool_calls[].id is required")
			}
			seen[tc.ID] = true
		}
		if m.Role == schema.Tool && (m.ToolCallID == "" || !seen[m.ToolCallID]) {
			return errors.New("error 400: invalid_request_error: tool_call_id does not match a tool call")
		}
	}

	return nil
}

// Spec "Resume across protocols": an Ollama candidate records a tool-call
// group with provider-empty ids and then dies; the chat_completions fallback
// receives the recorded group with matching non-empty ids, is not rejected
// for request shape, and continues without re-executing the tool.
func TestRunTurn_OllamaGroupsFailOverToStrictProtocol(t *testing.T) {
	ctx := context.Background()
	lamp := &countingTool{}

	ollama := &scriptedChatModel{steps: []func() (*schema.Message, error){
		func() (*schema.Message, error) {
			return schema.AssistantMessage("", []schema.ToolCall{{
				Type: testToolType, Function: schema.FunctionCall{Name: "light_on", Arguments: `{"entity":"light.x"}`},
			}}), nil
		},
		func() (*schema.Message, error) { return nil, errors.New("error 503: service unavailable") },
	}}
	strict := &scriptedChatModel{
		validate: strictToolPairing,
		steps:    []func() (*schema.Message, error){func() (*schema.Message, error) { return schema.AssistantMessage("the light is on", nil), nil }},
	}

	newRunner := func(m model.ToolCallingChatModel) agent.Runner {
		a, err := agent.New(ctx, agent.Config{ChatModel: m, Tools: []tool.BaseTool{lamp}}, nil)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}

		return a
	}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, newRunner(ollama), nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, newRunner(strict), nil),
	)

	// The recorded transcript stands in for the session history.
	var mu sync.Mutex
	var recorded []*schema.Message
	sinkCtx := agent.WithTranscriptSink(ctx, "ab12cd34", func(m schema.Message) {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, &m)
	})
	assemble := func(llm.PromptLayout) []*schema.Message {
		mu.Lock()
		defer mu.Unlock()

		return append([]*schema.Message{schema.UserMessage("turn on the light")}, recorded...)
	}
	resumeReady := func() bool {
		mu.Lock()
		defer mu.Unlock()

		return strictToolPairing(recorded) == nil && len(recorded)%2 == 0
	}

	outcome, err := o.RunTurn(sinkCtx, TurnRequest{SessionKey: testTurnChannel, Assemble: assemble, ResumeReady: resumeReady}, func(string) {})
	if err != nil {
		t.Fatalf("expected the strict fallback to resume the turn, got %v", err)
	}
	if outcome.Reply != "the light is on" {
		t.Fatalf("reply = %q", outcome.Reply)
	}
	if got := lamp.runs.Load(); got != 1 {
		t.Fatalf("expected the tool to run exactly once (no re-execution on resume), ran %d times", got)
	}

	strict.mu.Lock()
	defer strict.mu.Unlock()
	first := strict.inputs[0]
	var sawCall, sawResult bool
	for _, m := range first {
		for _, tc := range m.ToolCalls {
			sawCall = tc.ID == "call_ab12cd34_1_0"
		}
		if m.Role == schema.Tool {
			sawResult = m.ToolCallID == "call_ab12cd34_1_0"
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("expected the fallback prompt to carry the recorded group with the synthesized id, got %+v", first)
	}
}

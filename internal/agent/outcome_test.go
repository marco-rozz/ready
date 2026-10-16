package agent_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
)

// echoTool is a minimal invokable tool used by the outcome-combination tests
// on both construction paths. It always succeeds, so a tool call issued by
// the mock model always completes and produces a result.
const echoToolName = "echo"

type echoTool struct{}

var _ tool.BaseTool = (*echoTool)(nil)

func (echoTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: echoToolName, Desc: "echoes its arguments back"}, nil
}

func (echoTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	return "echo:" + argumentsInJSON, nil
}

// chatToolCall builds the tool-call response the scenarioMockModel returns to
// drive the ReAct loop into executing echoTool.
func chatToolCall() mockResponse {
	return mockResponse{
		message: &schema.Message{Content: ""},
		toolCalls: []schema.ToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: schema.FunctionCall{
					Name:      echoToolName,
					Arguments: `{"text":"hi"}`,
				},
			},
		},
	}
}

// TestTurnOutcome_FailureBeforeAnyModelOutput covers the spec scenario
// "Failure before any model output": the first model invocation fails, so no
// tool call completed and no content was delivered. This is the combination
// that maps to the temporary-unavailability terminal message.
func TestTurnOutcome_FailureBeforeAnyModelOutput(t *testing.T) {
	mock := &scenarioMockModel{
		responses: []mockResponse{{err: errors.New("connection refused")}},
	}

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    mock,
		SystemPrompt: agent.BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("agent.New() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr == nil {
		t.Fatal("expected error from failed first model invocation")
	}

	if outcome.ToolCallStarted || outcome.ToolCallCompleted {
		t.Error("expected no tool activity before the failure")
	}

	if outcome.ContentDelivered {
		t.Error("expected ContentDelivered=false; no content reached the caller")
	}

	if outcome.Reply != "" {
		t.Errorf("expected empty Reply, got %q", outcome.Reply)
	}
}

// TestTurnOutcome_FailureAfterCompletedToolCall covers the spec scenario
// "Failure after a completed tool call is distinguishable": the tool ran and
// produced a result, then the next model invocation failed before any
// final-answer content. This is the combination that maps to the
// incomplete-response terminal message.
func TestTurnOutcome_FailureAfterCompletedToolCall(t *testing.T) {
	mock := &scenarioMockModel{
		responses: []mockResponse{
			chatToolCall(),
			{err: errors.New("model died after tool call")},
		},
	}

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    mock,
		SystemPrompt: agent.BuildSystemPrompt(),
		Tools:        []tool.BaseTool{echoTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("agent.New() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr == nil {
		t.Fatal("expected error from failure after the tool call")
	}

	if !outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=true; echoTool ran and returned a result")
	}
	if !outcome.ToolCallStarted {
		t.Error("expected ToolCallStarted=true when the model emitted the call")
	}

	if outcome.ContentDelivered {
		t.Error("expected ContentDelivered=false; no final-answer content was produced")
	}

	if outcome.Reply != "" {
		t.Errorf("expected empty Reply, got %q", outcome.Reply)
	}
}

// TestTurnOutcome_SuccessWithToolCall verifies the success combination: the
// tool completed AND the final answer was delivered, on the non-streaming
// path (onChunk == nil) — ContentDelivered must be accurate there too.
func TestTurnOutcome_SuccessWithToolCall(t *testing.T) {
	mock := &scenarioMockModel{
		responses: []mockResponse{
			chatToolCall(),
			{message: &schema.Message{Content: "The light is on."}},
		},
	}

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    mock,
		SystemPrompt: agent.BuildSystemPrompt(),
		Tools:        []tool.BaseTool{echoTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("agent.New() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	if !outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=true; echoTool ran")
	}
	if !outcome.ToolCallStarted {
		t.Error("expected ToolCallStarted=true; echoTool was requested")
	}

	if !outcome.ContentDelivered {
		t.Error("expected ContentDelivered=true; a complete final answer was returned on the non-streaming path")
	}

	if outcome.Reply != "The light is on." {
		t.Errorf("expected final answer, got %q", outcome.Reply)
	}
}

// TestTurnOutcome_SuccessStreamingDelivered verifies ContentDelivered is set
// by the streaming wrapper when onChunk receives non-empty content, with no
// tool call in the turn.
func TestTurnOutcome_SuccessStreamingDelivered(t *testing.T) {
	mock := &scenarioMockModel{
		responses: []mockResponse{
			{message: &schema.Message{Content: "Hello world!"}},
		},
	}

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    mock,
		SystemPrompt: agent.BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("agent.New() error: %v", err)
	}

	var delivered []string

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")},
		func(chunk string) { delivered = append(delivered, chunk) },
	)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	if !outcome.ContentDelivered {
		t.Error("expected ContentDelivered=true; onChunk received non-empty content")
	}

	if outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=false; no tool ran")
	}

	if len(delivered) != 1 || delivered[0] != "Hello world!" {
		t.Errorf("expected one delivered chunk, got %v", delivered)
	}
}

// --- Agentic construction path (api: responses) ---

// agenticScenarioResponse is one step of a scripted agentic-model
// conversation: either a message or an error.
type agenticScenarioResponse struct {
	message *schema.AgenticMessage
	err     error
}

// scenarioAgenticModel implements model.AgenticModel with a scripted
// response sequence, mirroring scenarioMockModel for the agentic path.
type scenarioAgenticModel struct {
	callCount atomic.Int32
	responses []agenticScenarioResponse
	// streamFn, when set, overrides Stream entirely (used for
	// partial-stream-then-failure scenarios).
	streamFn func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error)
}

var _ model.AgenticModel = (*scenarioAgenticModel)(nil)

func (m *scenarioAgenticModel) Generate(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	idx := int(m.callCount.Add(1)) - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}

	resp := m.responses[idx]
	if resp.err != nil {
		return nil, resp.err
	}

	return resp.message, nil
}

func (m *scenarioAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	if m.streamFn != nil {
		return m.streamFn(ctx, input, opts...)
	}

	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func agenticEchoToolCall() *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.FunctionToolCall{
				Name:      echoToolName,
				CallID:    "call_1",
				Arguments: `{"text":"hi"}`,
			}),
		},
	}
}

// TestAgenticTurnOutcome_FailureBeforeAnyModelOutput is the agentic-path
// counterpart of TestTurnOutcome_FailureBeforeAnyModelOutput.
func TestAgenticTurnOutcome_FailureBeforeAnyModelOutput(t *testing.T) {
	mock := &scenarioAgenticModel{
		responses: []agenticScenarioResponse{{err: errors.New("connection refused")}},
	}

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: mock}, nil)
	if err != nil {
		t.Fatalf("agent.NewAgentic() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr == nil {
		t.Fatal("expected error from failed first model invocation")
	}

	if outcome.ToolCallStarted || outcome.ToolCallCompleted || outcome.ContentDelivered || outcome.Reply != "" {
		t.Errorf("expected zero outcome, got %+v", outcome)
	}
}

// TestAgenticTurnOutcome_FailureAfterCompletedToolCall covers the agentic
// construction path for the spec scenario "Failure after a completed tool
// call is distinguishable". Tool results are embedded as FunctionToolResult
// content blocks within assistant messages rather than emitted as separate
// Role=Tool events, so ToolCallCompleted is detected via those blocks.
func TestAgenticTurnOutcome_FailureAfterCompletedToolCall(t *testing.T) {
	mock := &scenarioAgenticModel{
		responses: []agenticScenarioResponse{
			{message: agenticEchoToolCall()},
			{err: errors.New("model died after tool call")},
		},
	}

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{
		Model: mock,
		Tools: []tool.BaseTool{echoTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("agent.NewAgentic() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr == nil {
		t.Fatal("expected error from failure after the tool call")
	}

	if !outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=true; echoTool ran and produced a result block")
	}
	if !outcome.ToolCallStarted {
		t.Error("expected ToolCallStarted=true when the agentic call block was emitted")
	}

	if outcome.ContentDelivered {
		t.Error("expected ContentDelivered=false; no final-answer content was produced")
	}
}

// TestAgenticTurnOutcome_PartialStreamThenFailure covers partial-stream-then-
// failure on the agentic construction path: the stream yields one non-empty
// text chunk and then errors. The turn fails, but ContentDelivered=true and
// Reply carries the partial text.
func TestAgenticTurnOutcome_PartialStreamThenFailure(t *testing.T) {
	partial := "partial answer"

	mock := &scenarioAgenticModel{
		streamFn: func(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
			sr, sw := schema.Pipe[*schema.AgenticMessage](0)
			go func() {
				defer sw.Close()
				sw.Send(&schema.AgenticMessage{
					Role:          schema.AgenticRoleTypeAssistant,
					ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: partial})},
				}, nil)
				sw.Send(nil, errors.New("stream boom"))
			}()

			return sr, nil
		},
	}

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: mock}, nil)
	if err != nil {
		t.Fatalf("agent.NewAgentic() error: %v", err)
	}

	var delivered []string

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")},
		func(chunk string) { delivered = append(delivered, chunk) },
	)
	if runErr == nil {
		t.Fatal("expected error from stream failure after partial content")
	}

	if !outcome.ContentDelivered {
		t.Error("expected ContentDelivered=true after a non-empty chunk was forwarded")
	}

	if outcome.Reply != partial {
		t.Errorf("expected partial Reply %q, got %q", partial, outcome.Reply)
	}

	if len(delivered) != 1 || delivered[0] != partial {
		t.Errorf("expected one delivered chunk %q, got %v", partial, delivered)
	}
}

// TestAgenticTurnOutcome_Success verifies the plain success combination on
// the agentic path with no tool call.
func TestAgenticTurnOutcome_Success(t *testing.T) {
	mock := &scenarioAgenticModel{
		responses: []agenticScenarioResponse{
			{message: assistantAgenticText("All done.")},
		},
	}

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: mock}, nil)
	if err != nil {
		t.Fatalf("agent.NewAgentic() error: %v", err)
	}

	outcome, runErr := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}

	if outcome.Reply != "All done." {
		t.Errorf("expected 'All done.', got %q", outcome.Reply)
	}

	if !outcome.ContentDelivered {
		t.Error("expected ContentDelivered=true on the non-streaming success path")
	}

	if outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=false; no tool ran")
	}
}

// assistantAgenticText builds an assistant AgenticMessage carrying a single
// text block (test helper, mirroring the unexported assistantAgenticMessage
// in agentic_convert.go from the outside).
func assistantAgenticText(text string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: text})},
	}
}

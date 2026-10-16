package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// mockChatModel implements model.ToolCallingChatModel for testing.
type mockChatModel struct {
	generateFunc func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
	streamFunc   func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

var _ model.ToolCallingChatModel = (*mockChatModel)(nil)

func (m *mockChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if m.generateFunc != nil {
		return m.generateFunc(ctx, input, opts...)
	}

	return &schema.Message{
		Role:    schema.Assistant,
		Content: "mock response",
	}, nil
}

func (m *mockChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if m.streamFunc != nil {
		return m.streamFunc(ctx, input, opts...)
	}

	// When no streamFunc is set, delegate to generateFunc so that tests
	// that configure only generateFunc still work with EnableStreaming: true.
	if m.generateFunc != nil {
		msg, err := m.generateFunc(ctx, input, opts...)
		if err != nil {
			return nil, err
		}

		return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
	}

	// Default: a simple stream with one message.
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "mock stream response",
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *mockChatModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	// Return the same mock — tools are not used in these tests.
	return m, nil
}

func TestNew_NilModel(t *testing.T) {
	_, err := New(context.Background(), Config{
		ChatModel: nil,
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil model")
	}

	if !errors.Is(err, ErrNilModel) {
		t.Errorf("expected ErrNilModel, got: %v", err)
	}
}

func TestNew_Success(t *testing.T) {
	mock := &mockChatModel{}
	ag, err := New(context.Background(), Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ag == nil {
		t.Fatal("expected non-nil agent")
	}
}

func TestNew_DefaultMaxIterations(t *testing.T) {
	mock := &mockChatModel{}
	ag, err := New(context.Background(), Config{
		ChatModel:     mock,
		SystemPrompt:  BuildSystemPrompt(),
		MaxIterations: 0, // should default to DefaultMaxIterations
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ag == nil {
		t.Fatal("expected non-nil agent")
	}
}

func TestAgent_Run_Success(t *testing.T) {
	expectedResponse := "The living room light is now on."

	mock := &mockChatModel{
		generateFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			return &schema.Message{
				Role:    schema.Assistant,
				Content: expectedResponse,
			}, nil
		},
	}

	ag, err := New(context.Background(), Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error creating agent: %v", err)
	}

	response, err := ag.Run(context.Background(), "cli", "Turn on the living room light")
	if err != nil {
		t.Fatalf("unexpected error running agent: %v", err)
	}

	if response.Reply != expectedResponse {
		t.Errorf("expected %q, got %q", expectedResponse, response.Reply)
	}
}

func TestAgent_Run_ModelError(t *testing.T) {
	mock := &mockChatModel{
		generateFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			return nil, errors.New("connection refused")
		},
	}

	ag, err := New(context.Background(), Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error creating agent: %v", err)
	}

	_, err = ag.Run(context.Background(), "cli", "hello")
	if err == nil {
		t.Fatal("expected error from model failure")
	}
}

func TestAgent_Run_ContextCancellation(t *testing.T) {
	mock := &mockChatModel{
		generateFunc: func(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				return &schema.Message{
					Role:    schema.Assistant,
					Content: "response",
				}, nil
			}
		},
	}

	ag, err := New(context.Background(), Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error creating agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err = ag.Run(ctx, "cli", "hello")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestBuildSystemPrompt_NoSkills(t *testing.T) {
	prompt := BuildSystemPrompt()
	if prompt == "" {
		t.Fatal("expected non-empty system prompt")
	}

	// Should not contain skills section.
	if strings.Contains(prompt, "## Available Skills") {
		t.Error("should not contain skills section when no metadata provided")
	}

	// Should not contain formatting section.
	if strings.Contains(prompt, "## Response Formatting") {
		t.Error("should not contain formatting section when no formatter prompt provided")
	}
}

func TestBuildSystemPrompt_ContainsBaseIdentity(t *testing.T) {
	prompt := BuildSystemPrompt()

	if !strings.Contains(prompt, "READY.") {
		t.Error("expected agent identity in prompt")
	}

	// Skills metadata and formatter rules are injected as per-turn system
	// messages by the dispatcher — they must NOT appear in the base prompt.
	if strings.Contains(prompt, "## Available Skills") {
		t.Error("base prompt must not contain skills section")
	}

	if strings.Contains(prompt, "## Response Formatting") {
		t.Error("base prompt must not contain formatter section")
	}
}

func TestAgent_RunMessages_SeesPriorHistory(t *testing.T) {
	// Capture the input slice the model receives — this is the real test that
	// per-session history actually reaches the LLM.
	var lastInput []*schema.Message

	mock := &mockChatModel{
		generateFunc: func(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			lastInput = append(lastInput[:0], input...)

			return &schema.Message{
				Role:    schema.Assistant,
				Content: "ack",
			}, nil
		},
	}

	ag, err := New(context.Background(), Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	conversation := []*schema.Message{
		schema.UserMessage("Mi chiamo Marco"),
		schema.AssistantMessage("Ciao Marco!", nil),
		schema.UserMessage("Come mi chiamo?"),
	}

	if _, err := ag.RunMessages(context.Background(), "telegram", conversation); err != nil {
		t.Fatalf("RunMessages() error: %v", err)
	}

	// The model must have received Marco's prior turn plus the new question.
	var sawMarco, sawQuestion bool

	for _, m := range lastInput {
		if m.Role == schema.User && strings.Contains(m.Content, "Mi chiamo Marco") {
			sawMarco = true
		}

		if m.Role == schema.User && strings.Contains(m.Content, "Come mi chiamo?") {
			sawQuestion = true
		}
	}

	if !sawMarco {
		t.Errorf("model never saw the prior user turn — history not threaded through. Input: %+v", lastInput)
	}

	if !sawQuestion {
		t.Errorf("model never saw the current user turn. Input: %+v", lastInput)
	}
}

func TestAgent_RunMessages_EmptySliceErrors(t *testing.T) {
	mock := &mockChatModel{}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	if _, err := ag.RunMessages(context.Background(), "cli", nil); err == nil {
		t.Fatal("expected error for empty messages slice")
	}
}

func TestAgent_Rebuild_UpdatesSystemPrompt(t *testing.T) {
	// Track which system prompt the LLM receives.
	var lastSystemPrompt string

	mock := &mockChatModel{
		generateFunc: func(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			for _, m := range msgs {
				if m.Role == schema.System {
					lastSystemPrompt = m.Content
				}
			}

			return &schema.Message{
				Role:    schema.Assistant,
				Content: "ok",
			}, nil
		},
	}

	ctx := context.Background()

	ag, err := New(ctx, Config{
		ChatModel:    mock,
		SystemPrompt: BuildSystemPrompt(),
	}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// First run with old prompt.
	if _, err := ag.Run(ctx, "cli", "hello"); err != nil {
		t.Fatalf("Run() before rebuild error: %v", err)
	}
	if !strings.Contains(lastSystemPrompt, "READY.") {
		t.Errorf("expected agent identity in system prompt, got: %q", lastSystemPrompt)
	}

	// Rebuild with a custom prompt (e.g. for testing prompt replacement).
	customPrompt := "CUSTOM PROMPT FOR TESTING"
	if err := ag.Rebuild(ctx, customPrompt); err != nil {
		t.Fatalf("Rebuild() error: %v", err)
	}

	// Second run should use the new prompt.
	if _, err := ag.Run(ctx, "cli", "hello again"); err != nil {
		t.Fatalf("Run() after rebuild error: %v", err)
	}
	if !strings.Contains(lastSystemPrompt, customPrompt) {
		t.Errorf("expected custom prompt after Rebuild, got: %q", lastSystemPrompt)
	}
}

// --- Streaming tests ---

func TestAgent_RunMessagesStreaming_ChunksDelivered(t *testing.T) {
	chunks := []string{"Hello", " world", "!"}
	chunkIdx := 0

	mock := &mockChatModel{
		streamFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
			msgs := make([]*schema.Message, len(chunks))
			for i, c := range chunks {
				msgs[i] = &schema.Message{Role: schema.Assistant, Content: c}
			}

			return schema.StreamReaderFromArray(msgs), nil
		},
	}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var delivered []string
	resp, err := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")},
		func(chunk string) { delivered = append(delivered, chunk) },
	)
	if err != nil {
		t.Fatalf("RunMessagesStreaming error: %v", err)
	}

	if resp.Reply != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", resp.Reply)
	}

	if len(delivered) != len(chunks) {
		t.Fatalf("expected %d chunks delivered, got %d", len(chunks), len(delivered))
	}

	for i, c := range chunks {
		if delivered[i] != c {
			t.Errorf("chunk %d: expected %q, got %q", i, c, delivered[i])
		}
	}
	_ = chunkIdx
}

func TestAgent_RunMessagesStreaming_NilCallback_ReturnsFullString(t *testing.T) {
	mock := &mockChatModel{
		streamFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
			return schema.StreamReaderFromArray([]*schema.Message{
				{Role: schema.Assistant, Content: "complete response"},
			}), nil
		},
	}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if err != nil {
		t.Fatalf("RunMessagesStreaming error: %v", err)
	}

	if resp.Reply != "complete response" {
		t.Errorf("expected 'complete response', got %q", resp.Reply)
	}
}

func TestAgent_RunMessagesStreaming_IntermediateToolCallsNotStreamed(t *testing.T) {
	// Test at the readStreamingAssistant level: a stream carrying ToolCalls
	// should NOT invoke onChunk and should return "" (intermediate iteration).
	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{
			Role:    schema.Assistant,
			Content: "",
			ToolCalls: []schema.ToolCall{
				{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "some_tool", Arguments: "{}"}},
			},
		}, nil)
	}()

	var delivered []string
	result, hasToolCalls := readStreamingAssistant(sr, func(chunk string) {
		delivered = append(delivered, chunk)
	}, nil, nil)

	if result != "" {
		t.Errorf("expected empty result for intermediate iteration, got %q", result)
	}
	if !hasToolCalls {
		t.Error("expected tool-call iteration to be reported")
	}

	if len(delivered) != 0 {
		t.Errorf("expected no chunks delivered for tool-call iteration, got %v", delivered)
	}
}

func TestAgent_RunMessagesStreaming_MultipleReActIterations(t *testing.T) {
	// Test readStreamingAssistant with a final-answer stream (Content only, no ToolCalls).
	// This simulates the last ReAct iteration after tool calls have been handled.
	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "The "}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "light "}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "is on."}, nil)
	}()

	var delivered []string
	result, hasToolCalls := readStreamingAssistant(sr, func(chunk string) {
		delivered = append(delivered, chunk)
	}, nil, nil)

	if result != "The light is on." {
		t.Errorf("expected 'The light is on.', got %q", result)
	}
	if hasToolCalls {
		t.Error("final answer unexpectedly reported tool calls")
	}

	if len(delivered) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(delivered))
	}

	expected := []string{"The ", "light ", "is on."}
	for i, exp := range expected {
		if delivered[i] != exp {
			t.Errorf("chunk %d: expected %q, got %q", i, exp, delivered[i])
		}
	}
}

const partialContent = "partial"

func TestAgent_RunMessagesStreaming_StreamError_PartialContentPreserved(t *testing.T) {
	mock := &mockChatModel{
		streamFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
			// Create a stream that yields one chunk then errors.
			sr, sw := schema.Pipe[*schema.Message](0)
			go func() {
				defer sw.Close()
				sw.Send(&schema.Message{Role: schema.Assistant, Content: partialContent}, nil)
				sw.Send(nil, errors.New("stream boom"))
			}()

			return sr, nil
		},
	}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	var delivered []string
	outcome, err := ag.RunMessagesStreaming(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")},
		func(chunk string) { delivered = append(delivered, chunk) },
	)
	// Partial stream then failure (llm-outage-delivery): the turn FAILS — the
	// dispatcher must know the response is incomplete — but the outcome
	// reports the partial reply and ContentDelivered=true so the partial is
	// retained and marked rather than presented as complete.
	if err == nil {
		t.Fatal("expected error from stream failure after partial content")
	}

	if outcome.Reply != partialContent {
		t.Errorf("expected partial content %q, got %q", partialContent, outcome.Reply)
	}

	if !outcome.ContentDelivered {
		t.Error("expected ContentDelivered=true after a non-empty chunk was forwarded")
	}

	if outcome.ToolCallCompleted {
		t.Error("expected ToolCallCompleted=false; no tool ran in this turn")
	}

	if len(delivered) != 1 || delivered[0] != partialContent {
		t.Errorf("expected one delivered chunk %q, got %v", partialContent, delivered)
	}
}

func TestAgent_RunMessagesStreaming_RunMessages_DelegatesWithNil(t *testing.T) {
	mock := &mockChatModel{
		generateFunc: func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
			return &schema.Message{Role: schema.Assistant, Content: "delegated"}, nil
		},
	}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	// RunMessages should delegate to RunMessagesStreaming with nil callback.
	resp, err := ag.RunMessages(context.Background(), "cli",
		[]*schema.Message{schema.UserMessage("hi")})
	if err != nil {
		t.Fatalf("RunMessages error: %v", err)
	}

	if resp.Reply != "delegated" {
		t.Errorf("expected 'delegated', got %q", resp.Reply)
	}
}

func TestAgent_RunMessagesStreaming_ContextCancellation(t *testing.T) {
	// Cancel the context before calling RunMessagesStreaming. The model's
	// Stream function checks context and returns an error immediately.
	mock := &mockChatModel{
		streamFunc: func(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				return schema.StreamReaderFromArray([]*schema.Message{
					{Role: schema.Assistant, Content: "should not reach"},
				}), nil
			}
		},
	}

	ag, err := New(context.Background(), Config{ChatModel: mock, SystemPrompt: BuildSystemPrompt()}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err = ag.RunMessagesStreaming(ctx, "cli",
		[]*schema.Message{schema.UserMessage("hi")}, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

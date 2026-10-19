package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// mockAgenticModel implements model.AgenticModel for testing.
type mockAgenticModel struct {
	generateFunc func(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error)
	lastInput    []*schema.AgenticMessage
}

var _ model.AgenticModel = (*mockAgenticModel)(nil)

func (m *mockAgenticModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.lastInput = input

	if m.generateFunc != nil {
		return m.generateFunc(ctx, input, opts...)
	}

	return assistantAgenticMessage("mock agentic response"), nil
}

func (m *mockAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

// History round-trip test — a conversation restored from the
// checkpoint store (internal/persistence.HistoryMessage's flattened
// {Role, Content} shape, rebuilt as []*schema.Message by the dispatcher's
// toSchemaMessages, exactly as it would be for any session regardless of
// which protocol wrote it) must convert losslessly into the AgenticMessage
// shape and drive the agentic ReAct loop after the operator switches the
// `default` entry's protocol to `responses` across a restart.
func TestAgenticAgent_HistoryRoundTrip_AcrossProtocolSwitch(t *testing.T) {
	// Simulates a conversation that was persisted while the daemon ran on a
	// chat_completions (or any other ToolCallingChatModel) protocol, then
	// restored after a restart where the operator switched `default` to
	// `api: responses`.
	restoredConversation := []*schema.Message{
		schema.SystemMessage("system prompt"),
		{Role: schema.User, Content: "turn 1 user"},
		{Role: schema.Assistant, Content: "turn 1 assistant"},
		{Role: schema.User, Content: "turn 2 user"},
	}

	mock := &mockAgenticModel{
		generateFunc: func(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
			return assistantAgenticMessage("turn 2 assistant"), nil
		},
	}

	ag, err := NewAgentic(context.Background(), AgenticConfig{Model: mock}, nil)
	if err != nil {
		t.Fatalf("unexpected error creating AgenticAgent: %v", err)
	}

	response, err := ag.RunMessagesStreaming(context.Background(), "cli", restoredConversation, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.Reply != "turn 2 assistant" {
		t.Errorf("expected response %q, got %q", "turn 2 assistant", response.Reply)
	}

	// Verify every restored turn survived the conversion losslessly (no
	// schema errors, no dropped turns, no content corruption) — this is the
	// History continuity across protocols.
	if len(mock.lastInput) != len(restoredConversation) {
		t.Fatalf("expected %d converted messages, got %d", len(restoredConversation), len(mock.lastInput))
	}

	wantRoles := []schema.AgenticRoleType{
		schema.AgenticRoleTypeSystem,
		schema.AgenticRoleTypeUser,
		schema.AgenticRoleTypeAssistant,
		schema.AgenticRoleTypeUser,
	}
	wantTexts := []string{"system prompt", "turn 1 user", "turn 1 assistant", "turn 2 user"}

	for i, got := range mock.lastInput {
		if got.Role != wantRoles[i] {
			t.Errorf("message %d: expected role %q, got %q", i, wantRoles[i], got.Role)
		}

		text, _ := agenticMessageText(got)
		if got.Role == schema.AgenticRoleTypeUser || got.Role == schema.AgenticRoleTypeSystem {
			// User/System text lives in UserInputText, not AssistantGenText;
			// agenticMessageText only reads assistant blocks, so fall back to
			// the block's UserInputText for those roles.
			text = userOrSystemBlockText(got)
		}

		if text != wantTexts[i] {
			t.Errorf("message %d: expected text %q, got %q", i, wantTexts[i], text)
		}
	}
}

// userOrSystemBlockText extracts the text of a System/User AgenticMessage's
// first content block, for test assertions only.
func userOrSystemBlockText(msg *schema.AgenticMessage) string {
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		if block.UserInputText != nil {
			return block.UserInputText.Text
		}
	}

	return ""
}

func TestToAgenticMessages_RolesMapped(t *testing.T) {
	messages := []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.User, Content: "usr"},
		{Role: schema.Assistant, Content: "asst"},
	}

	out := toAgenticMessages(messages)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}

	if out[0].Role != schema.AgenticRoleTypeSystem {
		t.Errorf("expected system role, got %q", out[0].Role)
	}
	if out[1].Role != schema.AgenticRoleTypeUser {
		t.Errorf("expected user role, got %q", out[1].Role)
	}
	if out[2].Role != schema.AgenticRoleTypeAssistant {
		t.Errorf("expected assistant role, got %q", out[2].Role)
	}

	text, hasToolCalls := agenticMessageText(out[2])
	if hasToolCalls {
		t.Error("expected no tool calls for a plain assistant message")
	}
	if text != "asst" {
		t.Errorf("expected text %q, got %q", "asst", text)
	}
}

func TestAgenticMessageText_DetectsToolCalls(t *testing.T) {
	msg := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.FunctionToolCall{Name: "test_tool", CallID: "1", Arguments: "{}"}),
		},
	}

	text, hasToolCalls := agenticMessageText(msg)
	if !hasToolCalls {
		t.Error("expected hasToolCalls to be true")
	}
	if text != "" {
		t.Errorf("expected empty text for a tool-call-only message, got %q", text)
	}
}

func TestAsBaseChatModel_Generate(t *testing.T) {
	mock := &mockAgenticModel{
		generateFunc: func(_ context.Context, input []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
			if len(input) == 0 {
				t.Fatal("expected non-empty input")
			}

			return assistantAgenticMessage("adapted response"), nil
		},
	}

	baseModel := AsBaseChatModel(mock)

	resp, err := baseModel.Generate(context.Background(), []*schema.Message{
		schema.SystemMessage("sys"),
		{Role: schema.User, Content: "hello"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Role != schema.Assistant {
		t.Errorf("expected assistant role, got %q", resp.Role)
	}
	if resp.Content != "adapted response" {
		t.Errorf("expected content %q, got %q", "adapted response", resp.Content)
	}
}

func TestAsBaseChatModel_Stream(t *testing.T) {
	mock := &mockAgenticModel{}
	baseModel := AsBaseChatModel(mock)

	reader, err := baseModel.Stream(context.Background(), []*schema.Message{{Role: schema.User, Content: "hi"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer reader.Close()

	var sb strings.Builder
	for {
		chunk, recvErr := reader.Recv()
		if recvErr != nil {
			break
		}
		sb.WriteString(chunk.Content)
	}

	if sb.String() != "mock agentic response" {
		t.Errorf("expected %q, got %q", "mock agentic response", sb.String())
	}
}

// Tool calls and results survive the []*schema.Message → AgenticMessage
// conversion with their call ids (no tool→user degradation), and map back to
// the same transcript shape.
func TestToAgenticMessages_ToolCallRoundTrip(t *testing.T) {
	const rtTool = "echo"
	call := schema.ToolCall{ID: "call_ab12cd34_1_0", Function: schema.FunctionCall{Name: rtTool, Arguments: `{"a":1}`}}
	messages := []*schema.Message{
		schema.UserMessage("hi"),
		schema.AssistantMessage("pre-action", []schema.ToolCall{call}),
		{Role: schema.Tool, Content: "result", ToolCallID: call.ID, ToolName: rtTool},
	}

	out := toAgenticMessages(messages)
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}

	assistant, results := agenticTranscript(out[1])
	if assistant == nil || assistant.Content != "pre-action" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("expected the assistant tool call back, got %+v", assistant)
	}
	if got := assistant.ToolCalls[0]; got.ID != call.ID || got.Function.Name != rtTool || got.Function.Arguments != `{"a":1}` {
		t.Errorf("tool call not preserved: %+v", got)
	}
	if len(results) != 0 {
		t.Errorf("expected no results on the assistant message, got %d", len(results))
	}

	if out[2].Role != schema.AgenticRoleTypeUser {
		t.Fatalf("expected the tool result as a user-role message, got %q", out[2].Role)
	}
	_, results = agenticTranscript(out[2])
	if len(results) != 1 || results[0].ToolCallID != call.ID || results[0].Content != "result" || results[0].ToolName != rtTool {
		t.Fatalf("expected the function result with the same call id, got %+v", results)
	}
}

// agenticChunk wraps one streaming content block in an assistant chunk.
func agenticChunk(block *schema.ContentBlock) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}}
}

// streamAt is the StreamingMeta for the block at index idx.
func streamAt(idx int) *schema.StreamingMeta { return &schema.StreamingMeta{Index: idx} }

// reasoningToolCallChunks is the streamed shape the thought tap consumes:
// reasoning-summary deltas plus encrypted material, then a function call
// whose arguments arrive in two deltas.
func reasoningToolCallChunks() []*schema.AgenticMessage {
	return []*schema.AgenticMessage{
		agenticChunk(schema.NewContentBlockChunk(&schema.Reasoning{Signature: "ENC"}, streamAt(0))),
		agenticChunk(schema.NewContentBlockChunk(&schema.Reasoning{Text: "Checking "}, streamAt(0))),
		agenticChunk(schema.NewContentBlockChunk(&schema.Reasoning{Text: "the light."}, streamAt(0))),
		agenticChunk(schema.NewContentBlockChunk(&schema.FunctionToolCall{CallID: "call-1", Name: "turn_on", Arguments: `{"entity_id":`}, streamAt(1))),
		agenticChunk(schema.NewContentBlockChunk(&schema.FunctionToolCall{Arguments: `"light.kitchen"}`}, streamAt(1))),
	}
}

// A completed stream materializes into one complete message.
func TestReadAgenticStreamFull_MaterializesCompleteMessage(t *testing.T) {
	var lastErr error
	_, full, hasToolCalls, _ := readAgenticStreamFull(schema.StreamReaderFromArray(reasoningToolCallChunks()), nil, &lastErr, nil)
	if lastErr != nil {
		t.Fatalf("unexpected stream error: %v", lastErr)
	}
	if full == nil {
		t.Fatal("expected a materialized message")
	}
	if !hasToolCalls {
		t.Error("expected hasToolCalls=true")
	}

	var reasoning *schema.Reasoning
	var call *schema.FunctionToolCall
	for _, b := range full.ContentBlocks {
		if b.Reasoning != nil {
			reasoning = b.Reasoning
		}
		if b.FunctionToolCall != nil {
			call = b.FunctionToolCall
		}
	}
	if reasoning == nil || reasoning.Text != "Checking the light." || reasoning.Signature != "ENC" {
		t.Errorf("reasoning = %+v", reasoning)
	}
	if call == nil || call.CallID != "call-1" || call.Name != "turn_on" || call.Arguments != `{"entity_id":"light.kitchen"}` {
		t.Errorf("call = %+v", call)
	}
}

// A stream that errors mid-way yields no materialized message.
func TestReadAgenticStreamFull_FailedStreamYieldsNoMessage(t *testing.T) {
	sr, sw := schema.Pipe[*schema.AgenticMessage](len(reasoningToolCallChunks()) + 1)
	for _, c := range reasoningToolCallChunks()[:4] {
		sw.Send(c, nil)
	}
	sw.Send(nil, errors.New("stream boom"))
	sw.Close()

	var lastErr error
	text, full, _, _ := readAgenticStreamFull(sr, nil, &lastErr, nil)
	if text != "" {
		t.Errorf("expected no final text from an intermediate stream, got %q", text)
	}
	if lastErr == nil {
		t.Error("expected the stream error to be recorded")
	}
	if full != nil {
		t.Errorf("expected no materialized message from a failed stream, got %+v", full)
	}
}

// Chunks ConcatAgenticMessages cannot merge (mixed roles) yield no
// materialized message even though the stream reached EOF.
func TestReadAgenticStreamFull_UnmergeableStreamYieldsNoMessage(t *testing.T) {
	chunks := reasoningToolCallChunks()
	chunks[len(chunks)-1].Role = schema.AgenticRoleTypeUser

	var lastErr error
	_, full, hasToolCalls, _ := readAgenticStreamFull(schema.StreamReaderFromArray(chunks), nil, &lastErr, nil)
	if !hasToolCalls {
		t.Error("expected hasToolCalls=true from the streamed call chunks")
	}
	if full != nil {
		t.Errorf("expected no materialized message from unmergeable chunks, got %+v", full)
	}
}

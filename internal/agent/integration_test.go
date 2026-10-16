package agent_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/persistence"
)

const (
	testCallID      = "call_1"
	testCallID2     = "call_2"
	testCallType    = "function"
	testToolTurnOn  = "light/turn_on"
	testToolTurnOff = "light/turn_off"
)

// scenarioMockModel simulates a multi-turn ReAct conversation.
// It tracks call count and responds differently at each step.
type scenarioMockModel struct {
	callCount atomic.Int32
	responses []mockResponse
}

type mockResponse struct {
	message   *schema.Message
	toolCalls []schema.ToolCall
	// err, when non-nil, makes the model fail on this response — used to
	// exercise failed-turn outcome combinations (llm-outage-delivery).
	err error
}

var _ model.ToolCallingChatModel = (*scenarioMockModel)(nil)

func (m *scenarioMockModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	idx := int(m.callCount.Add(1)) - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}

	resp := m.responses[idx]

	if resp.err != nil {
		return nil, resp.err
	}

	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: resp.message.Content,
	}

	if len(resp.toolCalls) > 0 {
		msg.ToolCalls = resp.toolCalls
	}

	return msg, nil
}

func (m *scenarioMockModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	idx := int(m.callCount.Add(1)) - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}

	resp := m.responses[idx]

	if resp.err != nil {
		return nil, resp.err
	}

	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: resp.message.Content,
	}

	if len(resp.toolCalls) > 0 {
		msg.ToolCalls = resp.toolCalls
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *scenarioMockModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// TestIntegration_MCPToolCall tests the full flow:
// user message → agent decides to call MCP tool → tool executes → final answer.
func TestIntegration_MCPToolCall(t *testing.T) {
	ctx := context.Background()

	// 1. Set up mock MCP server.
	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mcpTools := mcpClient.Tools()

	// 2. Create a scenario mock model that:
	//    - First call: decides to call light/turn_off with entity_id
	//    - Second call: produces final answer after observing tool result
	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      testToolTurnOff,
							Arguments: `{"entity_id": "light.kitchen"}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: "Ho spento la luce in cucina."},
			},
		},
	}

	// 3. Create agent with MCP tools.
	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         mcpTools,
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// 4. Run the agent.
	response, err := ag.Run(ctx, "cli", "Spegni la luce in cucina")
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	// 5. Verify response.
	if !strings.Contains(response.Reply, "cucina") {
		t.Errorf("expected response to mention cucina, got: %q", response.Reply)
	}

	// 6. Verify MCP tool was called.
	calls := mockMCP.Calls()
	if len(calls) == 0 {
		t.Fatal("expected MCP tool calls, got none")
	}

	found := false
	for _, call := range calls {
		if call.ToolName == testToolTurnOff {
			if call.Arguments["entity_id"] == "light.kitchen" {
				found = true

				break
			}
		}
	}

	if !found {
		t.Errorf("expected %s call with entity_id=light.kitchen, got: %+v", testToolTurnOff, calls)
	}

	// 7. Verify entity state changed.
	state, ok := mockMCP.GetState("light.kitchen")
	if !ok {
		t.Fatal("expected state for light.kitchen")
	}

	if state.State != "off" {
		t.Errorf("expected state 'off', got %q", state.State)
	}
}

// TestIntegration_SkillLoadThenMCP tests: skill load → use info → MCP call → response.
func TestIntegration_SkillLoadThenMCP(t *testing.T) {
	ctx := context.Background()

	// Set up mock MCP.
	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mcpTools := mcpClient.Tools()

	// Create a simple mock skill tool that returns home config info.
	skillsTool := &mockSkillLoadTool{
		content: "Kitchen: entity_id = light.kitchen\nLiving Room: entity_id = light.living_room",
	}

	allTools := append([]tool.BaseTool{skillsTool}, mcpTools...)

	// Mock model: 1) calls skills-load, 2) calls light/turn_on, 3) final answer.
	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      "skills-load",
							Arguments: `{"skill_name": "home-topology"}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID2,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      testToolTurnOn,
							Arguments: `{"entity_id": "light.kitchen", "brightness": 255}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: "La luce della cucina è accesa al massimo."},
			},
		},
	}

	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         allTools,
		MaxIterations: 10,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	response, err := ag.Run(ctx, "cli", "Accendi la luce in cucina al massimo")
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	if response.Reply == "" {
		t.Fatal("expected non-empty response")
	}

	// Verify MCP was called correctly.
	calls := mockMCP.Calls()

	found := false
	for _, call := range calls {
		if call.ToolName == testToolTurnOn && call.Arguments["entity_id"] == "light.kitchen" {
			found = true

			break
		}
	}

	if !found {
		t.Errorf("expected %s for light.kitchen, calls: %+v", testToolTurnOn, calls)
	}
}

// TestIntegration_ClimateControl tests climate device control flow.
func TestIntegration_ClimateControl(t *testing.T) {
	ctx := context.Background()

	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      "climate/set_temperature",
							Arguments: `{"entity_id": "climate.thermostat", "temperature": 22.5}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: "Temperatura impostata a 22.5 gradi."},
			},
		},
	}

	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         mcpClient.Tools(),
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	response, err := ag.Run(ctx, "cli", "Imposta il termostato a 22.5 gradi")
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	if !strings.Contains(response.Reply, "22.5") {
		t.Errorf("expected response to mention temperature, got: %q", response.Reply)
	}

	// Verify state update.
	state, _ := mockMCP.GetState("climate.thermostat")
	if state.Attributes["temperature"] != float64(22.5) {
		t.Errorf("expected temperature 22.5, got %v", state.Attributes["temperature"])
	}
}

// TestIntegration_GetState tests querying entity state.
func TestIntegration_GetState(t *testing.T) {
	ctx := context.Background()

	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      "homeassistant/get_state",
							Arguments: `{"entity_id": "light.living_room"}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: "La luce del soggiorno è accesa con luminosità 200."},
			},
		},
	}

	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         mcpClient.Tools(),
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	response, err := ag.Run(ctx, "cli", "Qual è lo stato della luce in soggiorno?")
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	if response.Reply == "" {
		t.Fatal("expected non-empty response")
	}
}

// TestIntegration_Persistence tests Session file save/restore with conversation history.
func TestIntegration_Persistence(t *testing.T) {
	ctx := context.Background()
	file := persistence.NewSessionFile(filepath.Join(t.TempDir(), "cli", "session.jsonl"))

	// Create and save a history.
	history := persistence.NewConversationHistory("cli-session", file)
	history.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "Spegni la luce"})
	history.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "Luce spenta."})

	if err := history.Save(ctx); err != nil {
		t.Fatalf("failed to save history: %v", err)
	}

	// Restore in a new instance.
	restored := persistence.NewConversationHistory("cli-session", persistence.NewSessionFile(file.Path()))
	if err := restored.Restore(ctx); err != nil {
		t.Fatalf("failed to restore history: %v", err)
	}

	msgs := restored.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	if msgs[0].Content != "Spegni la luce" {
		t.Errorf("expected 'Spegni la luce', got %q", msgs[0].Content)
	}
}

// TestIntegration_MCPConnectionError tests error handling when MCP is unavailable.
func TestIntegration_MCPConnectionError(t *testing.T) {
	ctx := context.Background()

	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: "Mi dispiace, non riesco a connettermi al sistema domotico."},
			},
		},
	}

	// Agent with no tools (simulating MCP failure).
	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         nil,
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	response, err := ag.Run(ctx, "cli", "Accendi la luce")
	if err != nil {
		t.Fatalf("agent should still respond gracefully: %v", err)
	}

	if response.Reply == "" {
		t.Fatal("expected a response even without tools")
	}
}

// mockSkillLoadTool implements a simple skill load tool for integration testing.
type mockSkillLoadTool struct {
	content string
}

var _ tool.InvokableTool = (*mockSkillLoadTool)(nil)

func (t *mockSkillLoadTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "skills-load",
		Desc: "Load a skill by name",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"skill_name": {
				Type:     schema.String,
				Desc:     "Name of the skill",
				Required: true,
			},
		}),
	}, nil
}

func (t *mockSkillLoadTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args struct {
		SkillName string `json:"skill_name"`
	}

	_ = json.Unmarshal([]byte(argumentsInJSON), &args)

	return t.content, nil
}

const streamFinalAnswer = "Ho acceso la luce del salotto."

// TestIntegration_StreamingWithToolCalls verifies that with EnableStreaming=true,
// only the final answer (not intermediate tool-call iterations) is streamed to onChunk.
func TestIntegration_StreamingWithToolCalls(t *testing.T) {
	ctx := context.Background()

	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mcpTools := mcpClient.Tools()

	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      testToolTurnOn,
							Arguments: `{"entity_id": "light.living_room"}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: streamFinalAnswer},
			},
		},
	}

	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         mcpTools,
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	var delivered []string
	resp, err := ag.RunMessagesStreaming(ctx, "cli",
		[]*schema.Message{schema.UserMessage("Accendi la luce del salotto")},
		func(chunk string) { delivered = append(delivered, chunk) },
	)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	if resp.Reply != streamFinalAnswer {
		t.Errorf("expected final answer, got %q", resp.Reply)
	}

	// Only the final answer should be streamed — the tool-call iteration
	// should NOT produce any chunks.
	if len(delivered) != 1 {
		t.Errorf("expected exactly 1 chunk (final answer only), got %d: %v", len(delivered), delivered)
	}

	if len(delivered) == 1 && delivered[0] != streamFinalAnswer {
		t.Errorf("expected final answer as chunk, got %q", delivered[0])
	}
}

// TestIntegration_StreamingNilCallback verifies that RunMessagesStreaming
// with a nil callback returns the same result as RunMessages.
func TestIntegration_StreamingNilCallback(t *testing.T) {
	ctx := context.Background()

	mockMCP := homemcp.NewMockServer(nil)
	mcpClient, err := mockMCP.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create MCP client: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	mcpTools := mcpClient.Tools()

	mock := &scenarioMockModel{
		responses: []mockResponse{
			{
				message: &schema.Message{Content: ""},
				toolCalls: []schema.ToolCall{
					{
						ID:   testCallID,
						Type: testCallType,
						Function: schema.FunctionCall{
							Name:      testToolTurnOff,
							Arguments: `{"entity_id": "light.kitchen"}`,
						},
					},
				},
			},
			{
				message: &schema.Message{Content: "Luce cucina spenta."},
			},
		},
	}

	ag, err := agent.New(ctx, agent.Config{
		ChatModel:     mock,
		SystemPrompt:  agent.BuildSystemPrompt(),
		Tools:         mcpTools,
		MaxIterations: 5,
	}, nil)
	if err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	resp, err := ag.RunMessagesStreaming(ctx, "cli",
		[]*schema.Message{schema.UserMessage("Spegni la luce della cucina")}, nil)
	if err != nil {
		t.Fatalf("agent run failed: %v", err)
	}

	if resp.Reply != "Luce cucina spenta." {
		t.Errorf("expected 'Luce cucina spenta.', got %q", resp.Reply)
	}
}

package agent_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/agent/toolnames"
)

const (
	deniedCallID   = "call_denied_1"
	deniedToolName = "home_assistant__ha_get_history"
)

// disclosureMockModel serves canned responses and records the tool-result
// messages the ReAct loop feeds back into the model, so the test can assert
// that the disclosure observation reached the loop for the original call id.
type disclosureMockModel struct {
	mu      sync.Mutex
	visible []*schema.Message // messages seen on the final Generate call
	calls   int
}

var _ model.ToolCallingChatModel = (*disclosureMockModel)(nil)

func (m *disclosureMockModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if m.calls == 1 {
		return &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   deniedCallID,
				Type: testCallType,
				Function: schema.FunctionCall{
					Name:      deniedToolName,
					Arguments: `{"entity_id":"sensor.battery"}`,
				},
			}},
		}, nil
	}

	m.visible = input

	return &schema.Message{Content: "The history data is unreachable."}, nil
}

func (m *disclosureMockModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *disclosureMockModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// lastToolMessages returns the tool-role messages observed on the final call.
func (m *disclosureMockModel) lastToolMessages() []schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []schema.Message
	for _, msg := range m.visible {
		if msg != nil && msg.Role == schema.Tool {
			out = append(out, *msg)
		}
	}

	return out
}

func newDisclosureAgent(t *testing.T, m model.ToolCallingChatModel, resolver *toolnames.Resolver, disclose bool, profile string) *agent.Agent {
	t.Helper()

	var wrapped model.ToolCallingChatModel
	if disclose {
		wrapped = toolnames.NewDisclosingChatModel(m, resolver, zerolog.Nop(), profile)
	} else {
		wrapped = toolnames.NewNormalizingChatModel(m, resolver, zerolog.Nop())
	}

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    wrapped,
		SystemPrompt: agent.BuildSystemPrompt(),
		// Mirrors production: registered tools plus, when the knob is on,
		// the policy_block sentinel.
		Tools: func() []tool.BaseTool {
			tools := []tool.BaseTool{echoTool{}}
			if disclose {
				tools = append(tools, toolnames.NewPolicyBlockTool())
			}

			return tools
		}(),
	}, nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	return ag
}

func newDisclosureResolver(disclose bool) *toolnames.Resolver {
	r := toolnames.NewResolver([]string{echoToolName, toolnames.PolicyBlockToolName})
	if disclose {
		r.LoadBlocked([]toolnames.BlockedTool{{Name: deniedToolName, Owner: "home_assistant"}})
	}

	return r
}

// Knob on: a call to a denied tool name is rewritten into the sentinel and
// the turn continues with the disclosure observation as an ordinary tool
// result, recorded in the transcript as a completed tool-call group.
func TestIntegration_DeniedToolDisclosure_KnobOn(t *testing.T) {
	mock := &disclosureMockModel{}
	ag := newDisclosureAgent(t, mock, newDisclosureResolver(true), true, "default")

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "turn01", rec.emit)

	outcome, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("battery history")}, func(string) {})
	if err != nil {
		t.Fatalf("turn must continue past a blocked call, got error: %v", err)
	}
	if outcome.Reply != "The history data is unreachable." {
		t.Errorf("unexpected reply: %q", outcome.Reply)
	}

	// The ReAct loop received the disclosure observation for the original
	// call id.
	toolMsgs := mock.lastToolMessages()
	if len(toolMsgs) != 1 {
		t.Fatalf("expected exactly 1 tool observation, got %d", len(toolMsgs))
	}
	obs := toolMsgs[0]
	if obs.ToolCallID != deniedCallID {
		t.Errorf("observation must carry the original call id, got %q", obs.ToolCallID)
	}
	for _, want := range []string{"[POLICY BLOCK]", deniedToolName, "home_assistant", "default"} {
		if !strings.Contains(obs.Content, want) {
			t.Errorf("observation missing %q: %s", want, obs.Content)
		}
	}

	// Transcript: one ordinary completed tool-call group — the rewritten
	// assistant call (sentinel, original id preserved) followed by its
	// result.
	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("expected 2 transcript emissions (assistant call + tool result), got %d: %+v", len(got), got)
	}
	if got[0].Role != schema.Assistant || len(got[0].ToolCalls) != 1 {
		t.Fatalf("expected an assistant tool-call message, got %+v", got[0])
	}
	call := got[0].ToolCalls[0]
	if call.ID != deniedCallID {
		t.Errorf("recorded call must keep the original id, got %q", call.ID)
	}
	if call.Function.Name != toolnames.PolicyBlockToolName {
		t.Errorf("recorded call must be the sentinel rewrite, got %q", call.Function.Name)
	}
	if got[1].Role != schema.Tool || got[1].ToolCallID != deniedCallID || !strings.Contains(got[1].Content, "[POLICY BLOCK]") {
		t.Errorf("expected the disclosure recorded as an ordinary tool result, got %+v", got[1])
	}
}

// Knob off: the same ghost call keeps today's failure behavior — Eino's
// unregistered-tool error fails the turn and no blocked name is resolvable.
func TestIntegration_DeniedToolDisclosure_KnobOff(t *testing.T) {
	mock := &disclosureMockModel{}
	ag := newDisclosureAgent(t, mock, newDisclosureResolver(false), false, "default")

	_, err := ag.RunMessagesStreaming(context.Background(), "cli", []*schema.Message{schema.UserMessage("battery history")}, func(string) {})
	if err == nil {
		t.Fatal("knob-off must keep the current failure behavior for an unregistered call")
	}
	if !strings.Contains(err.Error(), "not found in toolsNode indexes") {
		t.Errorf("expected Eino's unregistered-tool failure, got: %v", err)
	}

	if toolMsgs := mock.lastToolMessages(); len(toolMsgs) != 0 {
		t.Errorf("no observation must reach the loop with the knob off, got %+v", toolMsgs)
	}
}

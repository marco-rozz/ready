package agent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// `responses` path of the provider-compatibility check (turn-steering S19):
// the steered and stopped sequences convert to well-formed agentic messages —
// every function call answered by a user-role function result with the same
// call id, and the steer / turn_stopped / next-opener text as standalone user
// text messages.
func TestToAgenticMessages_SteerAndStopShapes(t *testing.T) {
	call := func(id, name string) schema.ToolCall {
		return schema.ToolCall{ID: id, Function: schema.FunctionCall{Name: name, Arguments: `{}`}}
	}

	steered := []*schema.Message{
		schema.UserMessage("lights and shutters"),
		schema.UserMessage("<turn_context_x>ctx</turn_context_x>"),
		schema.AssistantMessage("", []schema.ToolCall{call("c1", "light_on"), call("c2", "cover_close")}),
		schema.ToolMessage("[Superseded] …", "c1", schema.WithToolName("light_on")),
		schema.ToolMessage("[Superseded] …", "c2", schema.WithToolName("cover_close")),
		schema.UserMessage("<user_steer_x>\nno, the upstairs ones\n</user_steer_x>"),
		schema.AssistantMessage("done", nil),
	}

	stopped := []*schema.Message{
		schema.UserMessage("start the pump"),
		schema.AssistantMessage("", []schema.ToolCall{call("c1", "pump_on")}),
		schema.ToolMessage("[Stopped] …", "c1", schema.WithToolName("pump_on")),
		schema.UserMessage("[The user stopped this request before it was completed.]"),
		schema.UserMessage("next question"),
	}

	for name, in := range map[string][]*schema.Message{"steered": steered, "stopped": stopped} {
		out := toAgenticMessages(in)
		if len(out) != len(in) {
			t.Fatalf("%s: %d agentic messages for %d inputs", name, len(out), len(in))
		}

		open := map[string]bool{}

		for i, m := range out {
			for _, b := range m.ContentBlocks {
				switch {
				case b.FunctionToolCall != nil:
					if m.Role != schema.AgenticRoleTypeAssistant || b.FunctionToolCall.CallID == "" {
						t.Fatalf("%s message %d: malformed call block", name, i)
					}

					open[b.FunctionToolCall.CallID] = true
				case b.FunctionToolResult != nil:
					if m.Role != schema.AgenticRoleTypeUser || !open[b.FunctionToolResult.CallID] {
						t.Fatalf("%s message %d: result without an open call or not user-role", name, i)
					}

					delete(open, b.FunctionToolResult.CallID)
				}
			}

			if m.Role == schema.AgenticRoleTypeUser && len(open) > 0 && !hasResult(m) {
				t.Fatalf("%s message %d: user text while calls are unanswered", name, i)
			}
		}

		if len(open) != 0 {
			t.Fatalf("%s: calls never answered: %v", name, open)
		}
	}

	// The steer is a plain user text message after the results.
	out := toAgenticMessages(steered)
	steerMsg := out[5]

	if steerMsg.Role != schema.AgenticRoleTypeUser || len(steerMsg.ContentBlocks) != 1 || steerMsg.ContentBlocks[0].UserInputText == nil {
		t.Fatalf("steer message = %+v, want one user text block", steerMsg)
	}
}

func hasResult(m *schema.AgenticMessage) bool {
	for _, b := range m.ContentBlocks {
		if b.FunctionToolResult != nil {
			return true
		}
	}

	return false
}

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/steer"
)

// Provider compatibility (turn-steering S19): a steer produces
// `assistant(tool calls) → tool results → user` and a stop
// `… → user(turn_stopped) → user(next opener)`. The transcript is stored
// protocol-neutrally and projected identically for every wire, so a failover
// between protocols replays the same bytes; this test pins the structural
// rules each wire family imposes on that projection. The live per-provider
// smoke is recorded in tasks/turn-steering-live-smoke.md.

func steeredHistory() []persistence.HistoryMessage {
	now := time.Now()
	call := func(id, name string) persistence.ToolCallRecord {
		return persistence.ToolCallRecord{ID: id, Name: name, Arguments: `{}`}
	}

	return []persistence.HistoryMessage{
		{Role: persistence.RoleUser, Content: "turn on the lights and close the shutters", Timestamp: now},
		{Role: persistence.RoleUser, Kind: persistence.KindTurnContext, Content: "<turn_context_ab12cd34>ctx</turn_context_ab12cd34>", Timestamp: now},
		// A superseded batch: two proposed calls, both closed by append.
		{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{call("c1", "light_on"), call("c2", "cover_close")}, Timestamp: now},
		{Role: persistence.RoleTool, ToolCallID: "c1", Content: persistence.SupersededToolResult, Timestamp: now},
		{Role: persistence.RoleTool, ToolCallID: "c2", Content: persistence.SupersededToolResult, Timestamp: now},
		{Role: persistence.RoleUser, Kind: persistence.KindSteer, Content: steer.RenderSteer("ab12cd34", now, "no, the upstairs ones"), Timestamp: now},
		// A completed group followed by a second steer.
		{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{call("c3", "light_on")}, Timestamp: now},
		{Role: persistence.RoleTool, ToolCallID: "c3", Content: lampOnJSON, Timestamp: now},
		{Role: persistence.RoleUser, Kind: persistence.KindSteer, Content: steer.RenderSteer("ab12cd34", now, "and the garage"), Timestamp: now},
		{Role: persistence.RoleAssistant, Content: "done", Timestamp: now},
	}
}

func stoppedHistory() []persistence.HistoryMessage {
	now := time.Now()

	return []persistence.HistoryMessage{
		{Role: persistence.RoleUser, Content: "start the pump", Timestamp: now},
		{Role: persistence.RoleUser, Kind: persistence.KindTurnContext, Content: "<turn_context_x>ctx</turn_context_x>", Timestamp: now},
		{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{{ID: "c1", Name: "pump_on", Arguments: `{}`}}, Timestamp: now},
		{Role: persistence.RoleTool, ToolCallID: "c1", Content: persistence.StoppedToolResult, Timestamp: now},
		{Role: persistence.RoleUser, Kind: persistence.KindTurnStopped, Content: persistence.TurnStoppedContent, Timestamp: now},
		{Role: persistence.RoleUser, Content: "next question", Timestamp: now},
	}
}

// structuralChecks applies the rules shared by every wire: tool results answer
// an earlier assistant call, and every call is answered before any message
// that is not one of its results.
func structuralChecks(msgs []*schema.Message) error {
	open := map[string]bool{}
	seen := map[string]bool{}

	for i, m := range msgs {
		switch m.Role {
		case schema.Assistant:
			if len(open) > 0 {
				return fmt.Errorf("message %d: assistant before %d tool result(s)", i, len(open))
			}

			for _, tc := range m.ToolCalls {
				if tc.ID == "" || seen[tc.ID] {
					return fmt.Errorf("message %d: call id %q empty or reused", i, tc.ID)
				}

				seen[tc.ID], open[tc.ID] = true, true
			}
		case schema.Tool:
			if !open[m.ToolCallID] {
				return fmt.Errorf("message %d: tool result %q answers no open call", i, m.ToolCallID)
			}

			delete(open, m.ToolCallID)
		default:
			if len(open) > 0 {
				return fmt.Errorf("message %d (%s): %d tool call(s) unanswered", i, m.Role, len(open))
			}
		}
	}

	if len(open) > 0 {
		return fmt.Errorf("%d tool call(s) never answered", len(open))
	}

	return nil
}

// mergedTurns collapses consecutive same-role messages the way the Anthropic
// Messages API does (tool results count as user content) and returns each
// merged message's block kinds.
func mergedTurns(msgs []*schema.Message) [][]string {
	var out [][]string

	lastRole := ""

	for _, m := range msgs {
		if m.Role == schema.System {
			continue
		}

		role, kind := "user", blockKindText

		switch m.Role {
		case schema.Assistant:
			role = "assistant"
			kind = blockKindText
			if len(m.ToolCalls) > 0 {
				kind = blockKindUse
			}
		case schema.Tool:
			kind = blockKindResult
		}

		if role == lastRole {
			out[len(out)-1] = append(out[len(out)-1], kind)

			continue
		}

		out = append(out, []string{kind})
		lastRole = role
	}

	return out
}

// anthropicChecks: after merging, roles alternate, a tool_use turn is followed
// by a user turn that STARTS with its tool_result blocks (text after them is
// fine — that is where a steer lands).
func anthropicChecks(msgs []*schema.Message) error {
	turns := mergedTurns(msgs)

	for i := 1; i < len(turns); i++ {
		if turns[i-1][0] == blockKindUse || (len(turns[i-1]) > 0 && turns[i-1][len(turns[i-1])-1] == blockKindUse) {
			if turns[i][0] != blockKindResult {
				return fmt.Errorf("merged turn %d: tool_use not followed by a user turn starting with tool_result (%v)", i, turns[i])
			}
		}

		// tool_result blocks must precede any text inside their user turn.
		seenText := false

		for _, k := range turns[i] {
			if k == blockKindText {
				seenText = true
			}

			if k == blockKindResult && seenText {
				return fmt.Errorf("merged turn %d: tool_result after text (%v)", i, turns[i])
			}
		}
	}

	return nil
}

// googleChecks: a model turn with N function calls is followed by user
// content carrying exactly N function responses.
func googleChecks(msgs []*schema.Message) error {
	for i, m := range msgs {
		if m.Role != schema.Assistant || len(m.ToolCalls) == 0 {
			continue
		}

		results := 0

		for j := i + 1; j < len(msgs) && msgs[j].Role == schema.Tool; j++ {
			results++
		}

		if results != len(m.ToolCalls) {
			return fmt.Errorf("message %d: %d function calls but %d function responses follow", i, len(m.ToolCalls), results)
		}
	}

	return nil
}

func TestProviderCompatibility_ProjectionWellFormedOnEveryWire(t *testing.T) {
	t.Parallel()

	wires := map[string]func([]*schema.Message) error{
		wireOllama:         structuralChecks,
		"chat_completions": structuralChecks,
		"anthropic":        func(m []*schema.Message) error { return firstErr(structuralChecks(m), anthropicChecks(m)) },
		"google":           func(m []*schema.Message) error { return firstErr(structuralChecks(m), googleChecks(m)) },
	}

	histories := map[string][]persistence.HistoryMessage{"steered": steeredHistory(), "stopped": stoppedHistory()}

	for hname, h := range histories {
		projected := toSchemaMessages(h)

		for wire, check := range wires {
			if err := check(projected); err != nil {
				t.Errorf("%s history on %s: %v", hname, wire, err)
			}
		}
	}
}

// A protocol switch by failover replays the very same bytes: two projections
// of the same restored view are identical, whatever wire reads them.
func TestProviderCompatibility_FailoverReplaysIdenticalBytes(t *testing.T) {
	t.Parallel()

	h := steeredHistory()
	first, second := toSchemaMessages(h), toSchemaMessages(h)

	if len(first) != len(second) {
		t.Fatal("projection length differs between attempts")
	}

	for i := range first {
		if first[i].Role != second[i].Role || first[i].Content != second[i].Content || first[i].ToolCallID != second[i].ToolCallID || len(first[i].ToolCalls) != len(second[i].ToolCalls) {
			t.Fatalf("message %d differs between two projections of the same view", i)
		}
	}

	// The steer text reaches every wire verbatim and inside its envelope.
	var steers int

	for _, m := range first {
		if m.Role == schema.User && len(m.Content) > 12 && m.Content[:12] == "<user_steer_" {
			steers++
		}
	}

	if steers != 2 {
		t.Fatalf("steer envelopes projected = %d, want 2", steers)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}

	return nil
}

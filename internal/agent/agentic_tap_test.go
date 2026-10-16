package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
)

// Runner-level thought-tap coverage for the agentic construction path
// (responses-react-thought-logging change: tasks 3.3/3.4).

// reactRecords parses the tap's JSON log lines.
func reactRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		out = append(out, m)
	}

	return out
}

// kinds renders each record as event_type or event_type/source.
func kinds(records []map[string]any) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		k, _ := r["event_type"].(string)
		if src, ok := r["source"].(string); ok {
			k += "/" + src
		}
		out = append(out, k)
	}

	return out
}

// agenticToolIteration is one ReAct iteration: reasoning summary, pre-action
// text, and an echo function call with the given id.
func agenticToolIteration(callID, thought string) *schema.AgenticMessage {
	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.Reasoning{Text: "summary " + callID, Signature: "SIGNATURE-SECRET"}),
			schema.NewContentBlock(&schema.AssistantGenText{Text: thought}),
			schema.NewContentBlock(&schema.FunctionToolCall{Name: echoToolName, CallID: callID, Arguments: echoArgs}),
		},
	}
}

func newTappedAgentic(t *testing.T, m model.AgenticModel, buf *bytes.Buffer, enabled bool) *agent.AgenticAgent {
	t.Helper()

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{
		Model: m,
		Tools: []tool.BaseTool{echoTool{}},
	}, agent.NewTap(zerolog.New(buf), enabled))
	if err != nil {
		t.Fatalf("agent.NewAgentic: %v", err)
	}

	return ag
}

// Multi-iteration turn: each iteration logs reasoning, then content, then its
// decision; each user-role FunctionToolResult logs one observation; the final
// answer is never logged; event_seq is monotonic under one turn_id; the
// transcript sink and liveness are unaffected.
func TestAgenticTap_MultiIterationTurn(t *testing.T) {
	mock := &scenarioAgenticModel{responses: []agenticScenarioResponse{
		{message: agenticToolIteration(testCallID, "first")},
		{message: agenticToolIteration("call_2", "second")},
		{message: assistantAgenticText("FINAL-ANSWER")},
	}}

	var buf bytes.Buffer
	ag := newTappedAgentic(t, mock, &buf, true)

	rec := &sinkRecorder{}
	livenessAt := -1
	ctx := agent.WithTranscriptSink(context.Background(), "t-tap", rec.emit)
	ctx = agent.WithLiveness(ctx, func() { livenessAt = buf.Len() })

	outcome, err := ag.RunMessagesStreaming(ctx, "telegram-123456789", []*schema.Message{schema.UserMessage("hi")}, func(string) {})
	if err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}
	if outcome.Reply != "FINAL-ANSWER" {
		t.Fatalf("reply = %q", outcome.Reply)
	}

	records := reactRecords(t, &buf)
	want := []string{
		"thought/reasoning", "thought/content", "decision", "observation",
		"thought/reasoning", "thought/content", "decision", "observation",
	}
	if got := kinds(records); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v", got, want)
	}

	turnID, _ := records[0]["turn_id"].(string)
	for i, r := range records {
		if seq, _ := r["event_seq"].(float64); int(seq) != i+1 {
			t.Errorf("record %d: event_seq = %v", i, r["event_seq"])
		}
		if r["turn_id"] != turnID || r["session_key"] != "telegram-123456789" {
			t.Errorf("record %d: correlation = %v/%v", i, r["turn_id"], r["session_key"])
		}
	}
	if records[0]["content"] != "summary call_1" || records[1]["content"] != "first" {
		t.Errorf("iteration 1 thoughts = %v / %v", records[0]["content"], records[1]["content"])
	}
	if records[3]["tool_call_id"] != testCallID || records[3]["tool_name"] != echoToolName || records[3]["result"] == "" {
		t.Errorf("observation = %v", records[3])
	}
	for _, leak := range []string{"FINAL-ANSWER", "SIGNATURE-SECRET"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("records leaked %s", leak)
		}
	}

	if got := rec.all(); len(got) != 4 || got[0].ToolCalls[0].ID != testCallID || got[3].ToolCallID != "call_2" {
		t.Errorf("transcript emissions changed: %+v", got)
	}
	if livenessAt != 0 {
		t.Errorf("liveness signalled after %d bytes of records, want before any", livenessAt)
	}
}

// A stream that fails mid-way, or whose chunks cannot be merged, emits no
// records for that event.
func TestAgenticTap_IncompleteStreamEmitsNothing(t *testing.T) {
	cases := map[string]func(sw *schema.StreamWriter[*schema.AgenticMessage]){
		"failed": func(sw *schema.StreamWriter[*schema.AgenticMessage]) {
			sw.Send(agenticToolIteration(testCallID, "partial"), nil)
			sw.Send(nil, errors.New("stream boom"))
		},
		"unmergeable": func(sw *schema.StreamWriter[*schema.AgenticMessage]) {
			sw.Send(agenticToolIteration(testCallID, "partial"), nil)
			sw.Send(schema.UserAgenticMessage("other role"), nil)
		},
	}

	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			mock := &scenarioAgenticModel{
				streamFn: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
					sr, sw := schema.Pipe[*schema.AgenticMessage](2)
					go func() {
						defer sw.Close()
						send(sw)
					}()

					return sr, nil
				},
			}

			var buf bytes.Buffer
			ag := newTappedAgentic(t, mock, &buf, true)
			_, _ = ag.RunMessagesStreaming(context.Background(), "cli", []*schema.Message{schema.UserMessage("hi")}, func(string) {})

			if buf.Len() != 0 {
				t.Errorf("expected no records from an incomplete stream, got: %s", buf.String())
			}
		})
	}
}

// A disabled tap performs no content-block serialization; an enabled tap on
// the same message does (proving the probe is observable).
func TestAgenticTap_DisabledDoesNoWork(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		calls := 0
		mock := &scenarioAgenticModel{responses: []agenticScenarioResponse{{message: &schema.AgenticMessage{
			Role: schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{
				schema.NewContentBlock(&schema.ServerToolResult{Name: "web_search", CallID: "s1", Content: countingJSON{&calls}}),
				schema.NewContentBlock(&schema.AssistantGenText{Text: "done"}),
			},
		}}}}

		var buf bytes.Buffer
		ag := newTappedAgentic(t, mock, &buf, enabled)
		if _, err := ag.RunMessagesStreaming(context.Background(), "cli", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
			t.Fatalf("enabled=%v: %v", enabled, err)
		}

		if !enabled && (buf.Len() != 0 || calls != 0) {
			t.Errorf("disabled tap did work: %d bytes, %d marshal calls", buf.Len(), calls)
		}
		if enabled && (calls == 0 || !strings.Contains(buf.String(), `"observation"`)) {
			t.Errorf("enabled tap did not render the server result (calls=%d): %s", calls, buf.String())
		}
	}
}

// Rebuild replaces the Eino runner but keeps the injected tap.
func TestAgenticTap_RebuildRetainsTap(t *testing.T) {
	mock := &scenarioAgenticModel{responses: []agenticScenarioResponse{
		{message: agenticToolIteration(testCallID, "go")},
		{message: assistantAgenticText("ok")},
	}}

	var buf bytes.Buffer
	ag := newTappedAgentic(t, mock, &buf, true)
	if err := ag.Rebuild(context.Background(), "rebuilt prompt"); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if _, err := ag.RunMessagesStreaming(context.Background(), "cli", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}

	if got := kinds(reactRecords(t, &buf)); strings.Join(got, ",") != "thought/reasoning,thought/content,decision,observation" {
		t.Errorf("records after Rebuild = %v", got)
	}
}

// Each runner invocation — e.g. an orchestrator retry or failover attempt —
// keeps the caller's session key but starts a fresh turn_id/event_seq batch.
func TestAgenticTap_EachInvocationStartsNewBatch(t *testing.T) {
	mock := &scenarioAgenticModel{responses: []agenticScenarioResponse{
		{message: agenticToolIteration(testCallID, "go")},
		{message: assistantAgenticText("ok")},
		{message: agenticToolIteration("call_2", "again")},
		{message: assistantAgenticText("ok")},
	}}

	var buf bytes.Buffer
	ag := newTappedAgentic(t, mock, &buf, true)
	for range 2 {
		if _, err := ag.RunMessagesStreaming(context.Background(), "telegram-42", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
			t.Fatalf("RunMessagesStreaming: %v", err)
		}
	}

	records := reactRecords(t, &buf)
	if len(records) != 8 {
		t.Fatalf("expected 8 records, got %d", len(records))
	}
	first, second := records[0], records[4]
	if first["turn_id"] == second["turn_id"] {
		t.Error("expected a new turn_id for the second invocation")
	}
	if seq, _ := second["event_seq"].(float64); seq != 1 {
		t.Errorf("second batch event_seq = %v, want 1", second["event_seq"])
	}
	if first["session_key"] != "telegram-42" || second["session_key"] != "telegram-42" {
		t.Errorf("session keys = %v / %v", first["session_key"], second["session_key"])
	}
}

// countingJSON counts MarshalJSON calls.
type countingJSON struct{ calls *int }

func (c countingJSON) MarshalJSON() ([]byte, error) {
	*c.calls++

	return []byte(`"counted"`), nil
}

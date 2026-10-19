//nolint:goconst
package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	schemaopenai "github.com/cloudwego/eino/schema/openai"
	"github.com/rs/zerolog"
)

// makeAssistantEvent constructs a synthetic *adk.AgentEvent carrying an assistant message.
func makeAssistantEvent(msg *schema.Message) *adk.AgentEvent {
	return adk.EventFromMessage(msg, nil, schema.Assistant, "")
}

// makeToolEvent constructs a synthetic *adk.AgentEvent carrying a tool-result message.
func makeToolEvent(msg *schema.Message, toolName string) *adk.AgentEvent {
	return adk.EventFromMessage(msg, nil, schema.Tool, toolName)
}

// parseLinesJSON parses each non-empty line of buf as a JSON object and
// returns a slice of maps for easy field assertion.
func parseLinesJSON(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("failed to parse log line %q: %v", line, err)
		}

		lines = append(lines, m)
	}

	return lines
}

// TestTurnTap_Disabled verifies that a disabled tap emits zero log records.
func TestTurnTap_Disabled(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, false)
	tt := tap.StartTurn("cli")

	// Feed 100 events.
	for range 100 {
		ev := makeAssistantEvent(&schema.Message{
			Role:    schema.Assistant,
			Content: "thinking...",
			ToolCalls: []schema.ToolCall{
				{ID: "c1", Function: schema.FunctionCall{Name: "light/on", Arguments: `{}`}},
			},
		})
		tt.Emit(ev)
	}

	if buf.Len() != 0 {
		t.Errorf("expected zero output from disabled tap, got %d bytes: %s", buf.Len(), buf.String())
	}
}

// TestTurnTap_NilTap verifies that a nil *TurnTap does not panic.
func TestTurnTap_NilTap(_ *testing.T) {
	var tt *TurnTap
	ev := makeAssistantEvent(&schema.Message{Role: schema.Assistant, Content: "hello"})
	tt.Emit(ev) // must not panic
}

// TestTurnTap_NilEvent verifies that a nil *AgentEvent does not panic.
func TestTurnTap_NilEvent(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")
	tt.Emit(nil) // must not panic
	if buf.Len() != 0 {
		t.Errorf("expected zero output for nil event")
	}
}

// TestTurnTap_ErrorEventSkipped verifies that events with non-nil Err are skipped.
func TestTurnTap_ErrorEventSkipped(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	ev := &adk.AgentEvent{Err: errors.New("oops")}
	tt.Emit(ev)

	if buf.Len() != 0 {
		t.Errorf("expected zero output for error event, got: %s", buf.String())
	}
}

// TestTurnTap_Observation verifies the event_type=observation record.
func TestTurnTap_Observation(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	toolMsg := &schema.Message{
		Role:       schema.Tool,
		ToolName:   "light/get_state",
		ToolCallID: "call-abc",
		Content:    `{"state":"on"}`,
	}
	ev := makeToolEvent(toolMsg, "light/get_state")
	tt.Emit(ev)

	lines := parseLinesJSON(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}

	line := lines[0]
	assertField(t, line, "event_type", "observation")
	assertField(t, line, "session_key", "cli")
	assertField(t, line, "tool_name", "light/get_state")
	assertField(t, line, "tool_call_id", "call-abc")
	assertField(t, line, "result", `{"state":"on"}`)

	// event_seq must be 1
	if seq, ok := line["event_seq"].(float64); !ok || seq != 1 {
		t.Errorf("expected event_seq=1, got %v", line["event_seq"])
	}
}

// TestTurnTap_ReasoningContent verifies event_type=thought, source=reasoning.
func TestTurnTap_ReasoningContent(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("telegram-123")

	msg := &schema.Message{
		Role:             schema.Assistant,
		ReasoningContent: "I should check the lights first",
	}
	tt.Emit(makeAssistantEvent(msg))

	lines := parseLinesJSON(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}

	assertField(t, lines[0], "event_type", "thought")
	assertField(t, lines[0], "source", "reasoning")
	assertField(t, lines[0], "session_key", "telegram-123")
	assertField(t, lines[0], "content", "I should check the lights first")
}

// TestTurnTap_ContentWithToolCalls verifies event_type=thought, source=content
// plus one decision per ToolCall.
func TestTurnTap_ContentWithToolCalls(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "I'll turn on the living room light",
		ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "light/turn_on", Arguments: `{"entity_id":"light.living_room"}`}},
			{ID: "c2", Function: schema.FunctionCall{Name: "light/turn_on", Arguments: `{"entity_id":"light.bedroom"}`}},
		},
	}
	tt.Emit(makeAssistantEvent(msg))

	// Expect: 1 thought (content) + 2 decisions = 3 records.
	lines := parseLinesJSON(t, &buf)
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines (1 thought + 2 decisions), got %d", len(lines))
	}

	assertField(t, lines[0], "event_type", "thought")
	assertField(t, lines[0], "source", "content")
	assertField(t, lines[0], "content", "I'll turn on the living room light")

	assertField(t, lines[1], "event_type", "decision")
	assertField(t, lines[1], "tool_name", "light/turn_on")
	assertField(t, lines[1], "tool_call_id", "c1")

	assertField(t, lines[2], "event_type", "decision")
	assertField(t, lines[2], "tool_call_id", "c2")

	// event_seq must be monotonic 1, 2, 3.
	for i, seq := range []float64{1, 2, 3} {
		if got, ok := lines[i]["event_seq"].(float64); !ok || got != seq {
			t.Errorf("line %d: expected event_seq=%.0f, got %v", i+1, seq, lines[i]["event_seq"])
		}
	}
}

// TestTurnTap_FinalAnswerNotLogged verifies that a plain assistant Content
// with no ToolCalls (the final reply) does NOT emit any record.
func TestTurnTap_FinalAnswerNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "The lights are now on.",
	}
	tt.Emit(makeAssistantEvent(msg))

	if buf.Len() != 0 {
		t.Errorf("expected zero output for final-answer event, got: %s", buf.String())
	}
}

// TestTurnTap_ReasoningAndDecision verifies all three sources together.
func TestTurnTap_ReasoningAndDecision(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	msg := &schema.Message{
		Role:             schema.Assistant,
		ReasoningContent: "chain of thought",
		Content:          "pre-action text",
		ToolCalls: []schema.ToolCall{
			{ID: "x1", Function: schema.FunctionCall{Name: "wait", Arguments: `{"seconds":2}`}},
		},
	}
	tt.Emit(makeAssistantEvent(msg))

	// Expect: 1 reasoning + 1 content + 1 decision = 3 records.
	lines := parseLinesJSON(t, &buf)
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %s", len(lines), buf.String())
	}

	assertField(t, lines[0], "event_type", "thought")
	assertField(t, lines[0], "source", "reasoning")

	assertField(t, lines[1], "event_type", "thought")
	assertField(t, lines[1], "source", "content")

	assertField(t, lines[2], "event_type", "decision")
	assertField(t, lines[2], "tool_name", "wait")
}

// TestTurnTap_TurnIDConsistency verifies that all records within a turn share
// the same turn_id and that event_seq is strictly monotonic.
func TestTurnTap_TurnIDConsistency(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	// Emit one thought + one observation.
	tt.Emit(makeAssistantEvent(&schema.Message{
		Role:    schema.Assistant,
		Content: "going to act",
		ToolCalls: []schema.ToolCall{
			{ID: "d1", Function: schema.FunctionCall{Name: "tool_a", Arguments: `{}`}},
		},
	}))
	tt.Emit(makeToolEvent(&schema.Message{
		Role:       schema.Tool,
		ToolName:   "tool_a",
		ToolCallID: "d1",
		Content:    "ok",
	}, "tool_a"))

	lines := parseLinesJSON(t, &buf)
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 log lines, got %d", len(lines))
	}

	// All lines must share the same turn_id.
	turnID, ok := lines[0]["turn_id"].(string)
	if !ok || turnID == "" {
		t.Fatalf("expected non-empty turn_id in first line, got %v", lines[0]["turn_id"])
	}

	prevSeq := 0.0

	for i, line := range lines {
		if tid, _ := line["turn_id"].(string); tid != turnID {
			t.Errorf("line %d: expected turn_id=%q, got %q", i+1, turnID, tid)
		}

		seq, _ := line["event_seq"].(float64)
		if seq <= prevSeq {
			t.Errorf("line %d: event_seq %v is not strictly greater than previous %v", i+1, seq, prevSeq)
		}

		prevSeq = seq
	}
}

// assertField is a helper that checks a string field value in a log line.
func assertField(t *testing.T, line map[string]any, key, want string) {
	t.Helper()

	got, ok := line[key].(string)
	if !ok {
		t.Errorf("field %q missing or not a string in %v", key, line)

		return
	}

	if got != want {
		t.Errorf("field %q: expected %q, got %q", key, want, got)
	}
}

// TestTurnTap_LogLevelAboveDebug verifies task 7.6: when the logger level is
// above DEBUG (e.g. INFO), thought records are NOT emitted even when the tap is
// enabled — zerolog's level filter silently drops Debug() calls.
func TestTurnTap_LogLevelAboveDebug(t *testing.T) {
	var buf bytes.Buffer
	// Level set to Info — Debug() calls will be suppressed by zerolog.
	logger := zerolog.New(&buf).Level(zerolog.InfoLevel)
	tap := NewTap(logger, true) // tap is enabled…
	tt := tap.StartTurn("cli")

	// Emit an event that would normally produce records if level were debug.
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "about to act",
		ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "light/turn_on", Arguments: `{}`}},
		},
	}
	tt.Emit(makeAssistantEvent(msg))

	if buf.Len() != 0 {
		t.Errorf("expected zero output when log level > debug, got: %s", buf.String())
	}
}

// TestTurnTap_FileSinkReceivesThoughtRecords verifies task 7.7: thought records
// are written to both the in-memory writer (stderr analogue) and a temp file
// when io.MultiWriter is used — matching the production file-sink setup in
// internal/logging/logging.go.
func TestTurnTap_FileSinkReceivesThoughtRecords(t *testing.T) {
	// Create a temp file representing the JSON log file.
	f, err := os.CreateTemp(t.TempDir(), "thoughts-*.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	var stderrBuf bytes.Buffer
	combined := io.MultiWriter(&stderrBuf, f)
	// Debug level to allow thought records through.
	logger := zerolog.New(combined).Level(zerolog.DebugLevel)

	tap := NewTap(logger, true)
	tt := tap.StartTurn("cli")

	tt.Emit(makeAssistantEvent(&schema.Message{
		Role:    schema.Assistant,
		Content: "deciding next action",
		ToolCalls: []schema.ToolCall{
			{ID: "d1", Function: schema.FunctionCall{Name: "home_assistant__TurnOn", Arguments: `{}`}},
		},
	}))
	tt.Emit(makeToolEvent(&schema.Message{
		Role:       schema.Tool,
		ToolName:   "home_assistant__TurnOn",
		ToolCallID: "d1",
		Content:    `{"result":"ok"}`,
	}, "home_assistant__TurnOn"))

	// Flush and rewind the file.
	if err = f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	fileBytes, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Both the in-memory buffer and the file must have the same content.
	if !bytes.Equal(stderrBuf.Bytes(), fileBytes) {
		t.Errorf("file content differs from stderr buffer\nstderr: %s\nfile:   %s",
			stderrBuf.String(), string(fileBytes))
	}

	// Parse JSON lines and confirm expected event_type fields.
	var lines []map[string]any
	for _, raw := range bytes.Split(fileBytes, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("failed to parse log line %q: %v", raw, err)
		}
		lines = append(lines, m)
	}

	if len(lines) < 2 {
		t.Fatalf("expected at least 2 log lines in file, got %d", len(lines))
	}

	// Verify no event_type record is a "thoughts/" file path or directory marker.
	for _, line := range lines {
		if et, ok := line["event_type"].(string); ok {
			if strings.Contains(et, "/") || strings.HasPrefix(et, ".ready") {
				t.Errorf("unexpected event_type value suggesting FS write: %q", et)
			}
		}
	}

	// Confirm at least one thought and one observation record.
	counts := map[string]int{}
	for _, line := range lines {
		if et, ok := line["event_type"].(string); ok {
			counts[et]++
		}
	}
	if counts["decision"] == 0 {
		t.Error("expected at least one event_type=decision in file, got zero")
	}
	if counts["observation"] == 0 {
		t.Error("expected at least one event_type=observation in file, got zero")
	}
}

// TestTap_NoFilesystemSideEffects verifies task 7.9 as a code-level invariant:
// the Tap and TurnTap types perform no filesystem operations — they only call
// zerolog methods which are provided by the injected logger. This test confirms
// that enabling the tap with a no-op writer produces no disk I/O by checking
// the temp dir remains empty after Emit calls.
func TestTap_NoFilesystemSideEffects(t *testing.T) {
	tmpDir := t.TempDir()

	// Count files before.
	before := countFiles(t, tmpDir)

	var buf bytes.Buffer
	tap := NewTap(zerolog.New(&buf).Level(zerolog.DebugLevel), true)
	tt := tap.StartTurn("cli")

	for range 10 {
		tt.Emit(makeAssistantEvent(&schema.Message{
			Role:    schema.Assistant,
			Content: "looping",
			ToolCalls: []schema.ToolCall{
				{ID: "x", Function: schema.FunctionCall{Name: "tool", Arguments: `{}`}},
			},
		}))
	}

	after := countFiles(t, tmpDir)
	if after != before {
		t.Errorf("tap created %d unexpected files in temp dir", after-before)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	return len(entries)
}

// ── AgenticMessage adapter (change responses-react-thought-logging) ─────────

// agenticMsg builds an AgenticMessage of the given role from content blocks.
func agenticMsg(role schema.AgenticRoleType, blocks ...*schema.ContentBlock) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: role, ContentBlocks: blocks}
}

func newEnabledTurnTap(buf *bytes.Buffer, sessionKey string) *TurnTap {
	return NewTap(zerolog.New(buf), true).StartTurn(sessionKey)
}

// assertRecords checks each line's event_type (and source, when the want
// entry is "thought/<source>") in order.
func assertRecords(t *testing.T, lines []map[string]any, want ...string) {
	t.Helper()

	if len(lines) != len(want) {
		t.Fatalf("expected %d records %v, got %d: %v", len(want), want, len(lines), lines)
	}

	for i, w := range want {
		eventType, source, _ := strings.Cut(w, "/")
		assertField(t, lines[i], "event_type", eventType)
		if source != "" {
			assertField(t, lines[i], "source", source)
		}
		if seq, ok := lines[i]["event_seq"].(float64); !ok || int(seq) != i+1 {
			t.Errorf("record %d: expected event_seq=%d, got %v", i, i+1, lines[i]["event_seq"])
		}
	}
}

// countingMarshaler counts MarshalJSON calls so tests can prove a disabled
// tap never serializes values.
type countingMarshaler struct{ calls *int }

func (c countingMarshaler) MarshalJSON() ([]byte, error) {
	*c.calls++

	return []byte(`"counted"`), nil
}

// panickingMarshaler panics during encoding.
type panickingMarshaler struct{}

func (panickingMarshaler) MarshalJSON() ([]byte, error) { panic("boom") }

func TestTurnTapAgentic_ReasoningAggregatedOnce(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")

	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.Reasoning{Text: "Checking "}),
		schema.NewContentBlock(&schema.Reasoning{Text: ""}),
		schema.NewContentBlock(&schema.Reasoning{Text: "the light."}),
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "c1", Name: "light/on", Arguments: `{}`}),
	))

	lines := parseLinesJSON(t, &buf)
	assertRecords(t, lines, "thought/reasoning", "decision")
	assertField(t, lines[0], "content", "Checking the light.")
}

func TestTurnTapAgentic_PreActionContentOnlyWithCall(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")

	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.AssistantGenText{Text: "Let me "}),
		schema.NewContentBlock(&schema.Reasoning{Text: "why"}),
		schema.NewContentBlock(&schema.AssistantGenText{Text: "check."}),
		schema.NewContentBlock(&schema.MCPToolCall{CallID: "c1", Name: "search", Arguments: `{"q":1}`}),
	))

	lines := parseLinesJSON(t, &buf)
	assertRecords(t, lines, "thought/reasoning", "thought/content", "decision")
	assertField(t, lines[0], "content", "why")
	assertField(t, lines[1], "content", "Let me check.")
	assertField(t, lines[2], "tool_name", "search")
	assertField(t, lines[2], "tool_call_id", "c1")
	assertField(t, lines[2], "arguments", `{"q":1}`)
}

func TestTurnTapAgentic_FinalAnswerExcluded(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")

	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.AssistantGenText{Text: "Final answer."}),
	))

	if buf.Len() != 0 {
		t.Errorf("expected no records for a final answer, got: %s", buf.String())
	}
}

func TestTurnTapAgentic_NilAndUnsupportedBlocksIgnored(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")

	tt.EmitAgentic(nil)
	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		nil,
		schema.NewContentBlock(&schema.MCPListToolsResult{ServerLabel: "srv"}),
		schema.NewContentBlock(&schema.MCPToolApprovalRequest{ID: "a1", Name: "x"}),
		schema.NewContentBlock(&schema.MCPToolApprovalResponse{ApprovalRequestID: "a1", Approve: true}),
		schema.NewContentBlock(&schema.UserInputText{Text: "user text"}),
		&schema.ContentBlock{Type: "future_block"},
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "c1", Name: "light/on", Arguments: `{}`}),
	))

	assertRecords(t, parseLinesJSON(t, &buf), "decision")
}

func TestTurnTapAgentic_BlockOrderAndCorrelation(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "telegram-123456789")

	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.FunctionToolCall{CallID: "f1", Name: "fn", Arguments: `{"b":2}`}),
		schema.NewContentBlock(&schema.ServerToolCall{CallID: "s1", Name: "web_search", Arguments: map[string]any{"q": "x"}}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s1", Name: "web_search", Content: "found"}),
		schema.NewContentBlock(&schema.MCPToolCall{CallID: "m1", Name: "mcp", Arguments: `{}`}),
		schema.NewContentBlock(&schema.MCPToolResult{CallID: "m1", Name: "mcp", Content: `{"ok":true}`}),
	))
	// Tool results replayed / produced by Eino's tools node sit in a
	// user-role message; observation detection is role-independent.
	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeUser,
		schema.NewContentBlock(&schema.FunctionToolResult{CallID: "f1", Name: "fn", Content: []*schema.FunctionToolResultContentBlock{
			{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "on"}},
		}}),
	))

	lines := parseLinesJSON(t, &buf)
	assertRecords(t, lines, "decision", "decision", "observation", "decision", "observation", "observation")

	wantIDs := []string{"f1", "s1", "s1", "m1", "m1", "f1"}
	turnID, _ := lines[0]["turn_id"].(string)
	if turnID == "" {
		t.Fatal("expected a non-empty turn_id")
	}
	for i, line := range lines {
		assertField(t, line, "component", "agent")
		assertField(t, line, "session_key", "telegram-123456789")
		assertField(t, line, "turn_id", turnID)
		assertField(t, line, "tool_call_id", wantIDs[i])
	}
	assertField(t, lines[1], "arguments", `{"q":"x"}`)
	assertField(t, lines[2], "result", "found")
	assertField(t, lines[4], "result", `{"ok":true}`)
	assertField(t, lines[5], "result", "on")
	assertField(t, lines[5], "tool_name", "fn")
}

func TestTurnTapAgentic_DisabledDoesNoWork(t *testing.T) {
	var buf bytes.Buffer
	calls := 0
	msg := agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.Reasoning{Text: "r"}),
		schema.NewContentBlock(&schema.ServerToolCall{CallID: "s1", Name: "x", Arguments: countingMarshaler{&calls}}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s1", Name: "x", Content: countingMarshaler{&calls}}),
	)

	NewTap(zerolog.New(&buf), false).StartTurn("cli").EmitAgentic(msg)
	var nilTT *TurnTap
	nilTT.EmitAgentic(msg) // must not panic

	if buf.Len() != 0 || calls != 0 {
		t.Errorf("disabled tap did work: %d bytes, %d marshal calls", buf.Len(), calls)
	}
}

// Best-effort rendering of structured and multimodal values.
func TestTurnTapAgentic_ValueRendering(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")
	code := int64(-32000)

	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeUser,
		schema.NewContentBlock(&schema.FunctionToolResult{CallID: "f1", Name: "fn", Content: []*schema.FunctionToolResultContentBlock{
			{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "see "}},
			{Type: schema.FunctionToolResultContentBlockTypeImage, Image: &schema.UserInputImage{URL: "https://img/x.png"}, Extra: map[string]any{"k": "EXTRA-SECRET"}},
		}}),
		schema.NewContentBlock(&schema.MCPToolResult{CallID: "m1", Name: "mcp", Content: "partial", Error: &schema.MCPToolCallError{Code: &code, Message: "failed"}}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s1", Name: "srv", Content: map[string]any{"n": 1}}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s2", Name: "srv", Content: nil}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s3", Name: "srv", Content: make(chan int)}),
		schema.NewContentBlock(&schema.ServerToolResult{CallID: "s4", Name: "srv", Content: panickingMarshaler{}}),
		schema.NewContentBlock(&schema.ToolSearchFunctionToolResult{CallID: "t1", Name: "tool_search", Result: &schema.ToolSearchResult{
			Tools: []*schema.ToolInfo{{Name: "light/on", Desc: "turn on"}},
		}}),
	))
	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.ServerToolCall{CallID: "s5", Name: "srv", Arguments: make(chan int)}),
	))

	lines := parseLinesJSON(t, &buf)
	assertRecords(t, lines, "observation", "observation", "observation", "observation", "observation", "observation", "observation", "decision")

	assertField(t, lines[0], "result", `[{"type":"text","text":{"text":"see "}},{"type":"image","image":{"url":"https://img/x.png"}}]`)
	assertField(t, lines[1], "result", `{"content":"partial","error":{"code":-32000,"message":"failed"}}`)
	assertField(t, lines[2], "result", `{"n":1}`)
	assertField(t, lines[3], "result", "")
	assertField(t, lines[4], "result", "<unserializable>")
	assertField(t, lines[5], "result", "<unserializable>")
	if got, _ := lines[6]["result"].(string); !strings.Contains(got, "light/on") {
		t.Errorf("tool search result = %q, want the discovered tool definition", got)
	}
	assertField(t, lines[7], "arguments", "<unserializable>")
	if strings.Contains(buf.String(), "EXTRA-SECRET") {
		t.Error("function result part Extra leaked into a record")
	}
}

// Provider-specific, encrypted, and metadata fields never reach
// ReAct records, and neither does a final answer.
func TestTurnTapAgentic_PrivacyBoundary(t *testing.T) {
	var buf bytes.Buffer
	tt := newEnabledTurnTap(&buf, "cli")

	reasoning := schema.NewContentBlockChunk(&schema.Reasoning{
		Signature:       "SIGNATURE-SECRET",
		OpenAIExtension: &schemaopenai.ReasoningExtension{Content: []*schemaopenai.ReasoningContent{{Text: "RAW-REASONING-SECRET"}}},
	}, &schema.StreamingMeta{Index: 7})
	reasoning.Extra = map[string]any{"item_id": "EXTRA-SECRET"}

	call := schema.NewContentBlock(&schema.MCPToolCall{
		ServerLabel: "LABEL-SECRET", ApprovalRequestID: "APPROVAL-SECRET",
		CallID: "m1", Name: "mcp", Arguments: `{}`,
	})
	call.Extra = map[string]any{"status": "EXTRA-SECRET"}

	msg := agenticMsg(schema.AgenticRoleTypeAssistant,
		reasoning,
		schema.NewContentBlock(&schema.AssistantGenText{Text: "pre", OpenAIExtension: &schemaopenai.AssistantGenTextExtension{}}),
		call,
		schema.NewContentBlock(&schema.MCPToolResult{ServerLabel: "LABEL-SECRET", CallID: "m1", Name: "mcp", Content: "ok"}),
		schema.NewContentBlock(&schema.MCPToolApprovalRequest{ID: "APPROVAL-SECRET", Name: "mcp", ServerLabel: "LABEL-SECRET"}),
	)
	msg.ResponseMeta = &schema.AgenticResponseMeta{OpenAIExtension: &schemaopenai.ResponseMetaExtension{ID: "RESPMETA-SECRET"}}
	msg.Extra = map[string]any{"x": "EXTRA-SECRET"}
	tt.EmitAgentic(msg)
	tt.EmitAgentic(agenticMsg(schema.AgenticRoleTypeAssistant,
		schema.NewContentBlock(&schema.AssistantGenText{Text: "FINAL-ANSWER-SECRET"}),
	))

	// The reasoning block has no generic Text, so no reasoning thought.
	assertRecords(t, parseLinesJSON(t, &buf), "thought/content", "decision", "observation")

	for _, secret := range []string{
		"SIGNATURE-SECRET", "RAW-REASONING-SECRET", "EXTRA-SECRET", "LABEL-SECRET",
		"APPROVAL-SECRET", "RESPMETA-SECRET", "FINAL-ANSWER-SECRET", `"index"`,
	} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("ReAct records leaked %s: %s", secret, buf.String())
		}
	}
}

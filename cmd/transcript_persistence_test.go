package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
)

// This file exercises session transcript persistence end to end through
// processMessage → orchestrator → agent.Agent → Eino's tools node: groups are
// persisted mid-turn, a failed turn leaves a resumable transcript without the
// terminal string, a retry resumes from the grown history without
// re-executing tools, and the cross-turn prompt is a prefix-extension of the
// previous turn's final attempt (KV-cache continuity, tail-dynamic).

const (
	transcriptSession = testCLIChannelName
	lampToolName      = "light_on"
)

// transcriptModel is a scripted ToolCallingChatModel. Each step receives the
// model input and returns the response; every input is recorded.
type transcriptModel struct {
	mu     sync.Mutex
	calls  int
	steps  []func(input []*schema.Message) (*schema.Message, error)
	inputs [][]*schema.Message
}

var _ model.ToolCallingChatModel = (*transcriptModel)(nil)

func (m *transcriptModel) Generate(_ context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	idx := min(m.calls, len(m.steps)-1)
	m.calls++
	m.inputs = append(m.inputs, input)
	step := m.steps[idx]
	m.mu.Unlock()

	return step(input)
}

func (m *transcriptModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *transcriptModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *transcriptModel) input(i int) []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.inputs[i]
}

func (m *transcriptModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

// lampTool counts real executions.
type lampTool struct{ runs atomic.Int32 }

func (t *lampTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: lampToolName, Desc: "turns a light on"}, nil
}

func (t *lampTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	if !json.Valid([]byte(args)) {
		return "", errors.New("invalid arguments")
	}
	t.runs.Add(1)

	return `{"state":"on"}`, nil
}

// lampCall is an Ollama-shaped tool call: no id.
func lampCall() (*schema.Message, error) {
	return schema.AssistantMessage("", []schema.ToolCall{{
		Type:     "function",
		Function: schema.FunctionCall{Name: lampToolName, Arguments: `{"entity":"light.kitchen"}`},
	}}), nil
}

func finalAnswer(text string) func([]*schema.Message) (*schema.Message, error) {
	return func([]*schema.Message) (*schema.Message, error) { return schema.AssistantMessage(text, nil), nil }
}

// useOrchestrator swaps the env's plain runner for a single-candidate
// orchestrator over a real agent.Agent, so the restart gate and per-attempt
// re-assembly are exercised.
func (e *termEnv) useOrchestrator(t *testing.T, m model.ToolCallingChatModel, lamp *lampTool, retries int) {
	t.Helper()

	a, err := agent.New(context.Background(), agent.Config{ChatModel: m, Tools: []tool.BaseTool{lamp}}, nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	orch := orchestrator.New(orchestrator.Config{
		Candidates: []orchestrator.Candidate{{
			CandidateDescriptor: llm.CandidateDescriptor{Ref: "o::m", Provider: "o", ModelID: "m", API: "ollama", Layout: llm.LayoutTailDynamic},
			Runner:              a,
		}},
		Recovery: config.LLMRecoveryConfig{
			Soft:            config.LadderConfig{InitialCooldown: time.Minute, MaxCooldown: time.Hour},
			Hard:            config.LadderConfig{InitialCooldown: time.Minute, MaxCooldown: time.Hour},
			BackoffFactor:   2,
			ProbeTimeout:    time.Second,
			IdleProbeWindow: time.Minute,
		},
		Retries:      retries,
		RetryBackoff: time.Millisecond,
	}, zerolog.Nop())

	e.agentReg = &agentRegistry{exec: orch, orch: orch, layout: llm.LayoutTailDynamic}
	if e.rt != nil {
		e.rt.agentReg = e.agentReg
	}
}

// jsonlLines reads the current Session file's transcript messages (records
// — turn_usage, compaction — excluded; see jsonlRecords).
func (e *termEnv) jsonlLines(t *testing.T) []persistence.HistoryMessage {
	t.Helper()

	var out []persistence.HistoryMessage
	for _, m := range e.jsonlAll(t) {
		if !m.IsRecord() {
			out = append(out, m)
		}
	}

	return out
}

// jsonlRecords reads the current Session file's records.
func (e *termEnv) jsonlRecords(t *testing.T) []persistence.HistoryMessage {
	t.Helper()

	var out []persistence.HistoryMessage
	for _, m := range e.jsonlAll(t) {
		if m.IsRecord() {
			out = append(out, m)
		}
	}

	return out
}

// jsonlAll reads every entry of the current Session file of the cli address.
func (e *termEnv) jsonlAll(t *testing.T) []persistence.HistoryMessage {
	t.Helper()

	cur := e.sessions.Current(context.Background(), transcriptSession)

	data, err := os.ReadFile(cur.Path)
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}

	raw := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	out := make([]persistence.HistoryMessage, 0, len(raw))
	for _, line := range raw {
		var m persistence.HistoryMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode line %q: %v", line, err)
		}
		out = append(out, m)
	}

	return out
}

// restoredHistory is the current Session history as a restarted daemon
// sees it.
func (e *termEnv) restoredHistory(t *testing.T) *persistence.ConversationHistory {
	t.Helper()

	mgr, _, _ := testSessions(t, e.sessionsDir)

	return currentHistory(t, mgr, transcriptSession)
}

func roles(msgs []persistence.HistoryMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		r := string(m.Role)
		if m.Kind == persistence.KindTurnContext {
			r = "turn_context"
		}
		parts = append(parts, r)
	}

	return strings.Join(parts, ",")
}

func cliMsg(text string) channel.Message {
	return channel.Message{ChannelName: testCLIChannelName, Content: text}
}

// A tool iteration is persisted as it happens: the completed group is on
// disk before the next model call begins; the final reply closes the turn.
func TestTranscript_StreamingToolTurnPersistsGroupsMidTurn(t *testing.T) {
	buf := &termBuffer{}
	env := newTermEnv(t, channelFormatterCfg{}, newTestCLIChannel(buf))
	lamp := &lampTool{}

	var midTurn []persistence.HistoryMessage
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		func([]*schema.Message) (*schema.Message, error) {
			midTurn = env.jsonlLines(t)

			return schema.AssistantMessage("the kitchen light is on", nil), nil
		},
	}}
	env.useOrchestrator(t, m, lamp, 0)

	env.run(cliMsg("turn on the kitchen light"))

	if got := roles(midTurn); got != "user,turn_context,assistant,tool" {
		t.Fatalf("expected the group on disk before the second model call, got %q", got)
	}
	if midTurn[2].ToolCalls[0].ID == "" || midTurn[3].ToolCallID != midTurn[2].ToolCalls[0].ID {
		t.Fatalf("expected a synthesized id shared by call and result, got %+v / %+v", midTurn[2], midTurn[3])
	}
	if midTurn[2].ToolCalls[0].Name != lampToolName || midTurn[2].ToolCalls[0].Arguments != `{"entity":"light.kitchen"}` {
		t.Errorf("expected the canonical name and verbatim arguments, got %+v", midTurn[2].ToolCalls[0])
	}

	final := env.jsonlLines(t)
	if got := roles(final); got != "user,turn_context,assistant,tool,assistant" {
		t.Fatalf("expected the final reply appended once, got %q", got)
	}
	if final[4].Content != "the kitchen light is on" {
		t.Errorf("unexpected final line %+v", final[4])
	}
}

// The terminal message is not transcript, and a crash mid-turn leaves a
// resumable transcript: a turn failing after a complete group persists the
// group, never the terminal string, and restores to a valid prompt.
func TestTranscript_FailedTurnLeavesResumableTranscript(t *testing.T) {
	buf := &termBuffer{}
	env := newTermEnv(t, channelFormatterCfg{}, newTestCLIChannel(buf))
	lamp := &lampTool{}

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		func([]*schema.Message) (*schema.Message, error) { return nil, errors.New("error 400: bad request") },
	}}
	env.useOrchestrator(t, m, lamp, 0)

	env.run(cliMsg("turn on the kitchen light"))

	if !strings.Contains(buf.String(), msgIncompleteResponse) {
		t.Fatalf("expected the incomplete-response terminal message delivered, got %q", buf.String())
	}

	lines := env.jsonlLines(t)
	if got := roles(lines); got != "user,turn_context,assistant,tool" {
		t.Fatalf("expected the transcript to end at the last complete group, got %q", got)
	}
	for _, l := range lines {
		if strings.Contains(l.Content, msgIncompleteResponse) || strings.Contains(l.Content, msgLLMUnavailable) {
			t.Fatalf("terminal string persisted as transcript: %+v", l)
		}
	}

	// A restart restores a transcript whose projection pairs every result
	// with its call.
	restored := env.restoredHistory(t)
	if restored.HasOpenToolCallGroup() {
		t.Fatal("expected no open group after restore")
	}
	assertStrictPairing(t, toSchemaMessages(restored.Messages()))
}

func assertStrictPairing(t *testing.T, msgs []*schema.Message) {
	t.Helper()

	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				t.Fatalf("tool call without id: %+v", m)
			}
			seen[tc.ID] = true
		}
		if m.Role == schema.Tool && !seen[m.ToolCallID] {
			t.Fatalf("tool result %q without its call", m.ToolCallID)
		}
	}
}

func countTurnContexts(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, "<turn_context_") {
			n++
		}
	}

	return n
}

// Resume after recorded tool-call groups continues the loop: a
// same-candidate retry after a transient failure assembles from the grown
// history — exactly one turn-context message, ending at the recorded group —
// and the tool is not executed again.
func TestTranscript_RetryResumesFromGrownHistory(t *testing.T) {
	buf := &termBuffer{}
	env := newTermEnv(t, channelFormatterCfg{}, newTestCLIChannel(buf))
	lamp := &lampTool{}

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		func([]*schema.Message) (*schema.Message, error) {
			return nil, errors.New("error 503: service unavailable")
		},
		finalAnswer("done after resume"),
	}}
	env.useOrchestrator(t, m, lamp, 1)

	env.run(cliMsg("turn on the kitchen light"))

	if !strings.Contains(buf.String(), "done after resume") {
		t.Fatalf("expected the resumed answer delivered, got %q", buf.String())
	}
	if got := lamp.runs.Load(); got != 1 {
		t.Fatalf("expected the tool to run exactly once, ran %d times", got)
	}
	if m.callCount() != 3 {
		t.Fatalf("expected 3 model calls (call, failure, resumed final), got %d", m.callCount())
	}

	resumed := m.input(2)
	if n := countTurnContexts(resumed); n != 1 {
		t.Fatalf("expected exactly one turn-context message on the resumed attempt, got %d", n)
	}
	if last := resumed[len(resumed)-1]; last.Role != schema.Tool {
		t.Fatalf("expected the resumed prompt to end at the recorded tool result, got %+v", last)
	}
	assertStrictPairing(t, resumed)

	if got := roles(env.jsonlLines(t)); got != "user,turn_context,assistant,tool,assistant" {
		t.Fatalf("expected one transcript for the turn (no duplicated group), got %q", got)
	}
}

func sameMessage(a, b *schema.Message) bool {
	return a.Role == b.Role && a.Content == b.Content && a.ToolCallID == b.ToolCallID &&
		reflect.DeepEqual(a.ToolCalls, b.ToolCalls)
}

// Tool phase is cache-reusable across turns (tail-dynamic): the
// message list sent by turn N's final model call — prefix, history, user,
// turn context, tool-call groups — is a byte-identical prefix of turn N+1's
// first model call.
func TestTranscript_CrossTurnCacheContinuity(t *testing.T) {
	buf := &termBuffer{}
	env := newTermEnv(t, channelFormatterCfg{}, newTestCLIChannel(buf))
	lamp := &lampTool{}

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("both lights on"),
		finalAnswer("you're welcome"),
	}}
	env.useOrchestrator(t, m, lamp, 0)

	env.run(cliMsg("turn on the lights"))
	env.run(cliMsg("thanks"))

	turnNFinal := m.input(2)
	turnNext := m.input(3)
	if len(turnNext) <= len(turnNFinal) {
		t.Fatalf("expected turn N+1 to extend turn N (len %d vs %d)", len(turnNext), len(turnNFinal))
	}
	for i := range turnNFinal {
		if !sameMessage(turnNFinal[i], turnNext[i]) {
			t.Fatalf("prefix diverges at %d: %+v vs %+v", i, turnNFinal[i], turnNext[i])
		}
	}

	// A restart preserves the cache boundary: the restored history
	// projects to the identical prompt.
	live := toSchemaMessages(currentHistory(t, env.sessions, transcriptSession).Messages())
	restored := toSchemaMessages(env.restoredHistory(t).Messages())
	if len(live) != len(restored) {
		t.Fatalf("restored projection length %d, live %d", len(restored), len(live))
	}
	for i := range live {
		if !sameMessage(live[i], restored[i]) || live[i].ToolName != restored[i].ToolName {
			t.Fatalf("restored projection diverges at %d: %+v vs %+v", i, live[i], restored[i])
		}
	}
}

func TestSmallestContextWindow(t *testing.T) {
	cfg := config.LLMConfig{
		Default:  "a::m",
		Failover: []string{"b::m", "c::m", "bad-ref"},
		Providers: map[string]config.LLMProviderConfig{
			"a": {ContextWindow: 65536},
			"b": {ContextWindow: 32768},
			"c": {},
			"d": {ContextWindow: 1024}, // not in the chain
		},
	}
	if got := smallestContextWindow(cfg); got != 32768 {
		t.Fatalf("expected the smallest chain window (32768), got %d", got)
	}
	if got := smallestContextWindow(config.LLMConfig{Default: "c::m", Providers: cfg.Providers}); got != 0 {
		t.Fatalf("expected 0 when no chain provider declares a window, got %d", got)
	}
}

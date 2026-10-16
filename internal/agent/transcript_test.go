package agent_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/agent"
)

// portableID is the tool-call id shape valid on every wire protocol
// (Anthropic's tool_use.id pattern is the strictest).
var portableID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

const (
	echoArgs     = `{"text":"hi"}`
	transcriptOK = "done"
)

// sinkRecorder collects transcript emissions.
type sinkRecorder struct {
	mu   sync.Mutex
	msgs []schema.Message
}

func (r *sinkRecorder) emit(m schema.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
}

func (r *sinkRecorder) all() []schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]schema.Message(nil), r.msgs...)
}

func echoCall(id string) schema.ToolCall {
	return schema.ToolCall{ID: id, Type: testCallType, Function: schema.FunctionCall{Name: echoToolName, Arguments: echoArgs}}
}

func newEchoAgent(t *testing.T, m model.ToolCallingChatModel) *agent.Agent {
	t.Helper()

	ag, err := agent.New(context.Background(), agent.Config{
		ChatModel:    m,
		SystemPrompt: agent.BuildSystemPrompt(),
		Tools:        []tool.BaseTool{echoTool{}},
	}, nil)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	return ag
}

// Ollama returns tool calls with empty ids: the sink receives each assistant
// tool-call message followed by its results, with synthesized ids
// call_<turnID>_<iter>_<idx> that are valid on every protocol and shared by
// the results. The final answer is not emitted (it is the outcome's Reply).
func TestTranscriptSink_ChatPathSynthesizesIDs(t *testing.T) {
	mock := &scenarioMockModel{responses: []mockResponse{
		{message: &schema.Message{Content: "checking"}, toolCalls: []schema.ToolCall{echoCall(""), echoCall("")}},
		{message: &schema.Message{}, toolCalls: []schema.ToolCall{echoCall("")}},
		{message: &schema.Message{Content: transcriptOK}},
	}}
	ag := newEchoAgent(t, mock)

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "ab12cd34", rec.emit)

	outcome, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, func(string) {})
	if err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}
	if outcome.Reply != transcriptOK {
		t.Fatalf("expected reply %q, got %q", transcriptOK, outcome.Reply)
	}

	got := rec.all()
	if len(got) != 5 {
		t.Fatalf("expected 5 emissions (A, T, T, A, T), got %d: %+v", len(got), got)
	}

	first := got[0]
	if first.Role != schema.Assistant || first.Content != "checking" || len(first.ToolCalls) != 2 {
		t.Fatalf("expected the first assistant message with its pre-action text and 2 calls, got %+v", first)
	}
	wantIDs := []string{"call_ab12cd34_1_0", "call_ab12cd34_1_1"}
	for i, tc := range first.ToolCalls {
		if tc.ID != wantIDs[i] || !portableID.MatchString(tc.ID) {
			t.Errorf("call %d: expected id %q, got %q", i, wantIDs[i], tc.ID)
		}
	}

	results := map[string]bool{}
	for _, m := range got[1:3] {
		if m.Role != schema.Tool {
			t.Fatalf("expected tool results after the assistant message, got %+v", m)
		}
		results[m.ToolCallID] = true
	}
	if !results[wantIDs[0]] || !results[wantIDs[1]] {
		t.Errorf("expected results remapped onto the synthesized ids, got %v", results)
	}

	if got[3].ToolCalls[0].ID != "call_ab12cd34_2_0" || got[4].ToolCallID != "call_ab12cd34_2_0" {
		t.Errorf("expected second-iteration id call_ab12cd34_2_0, got %q / %q", got[3].ToolCalls[0].ID, got[4].ToolCallID)
	}
	if got[4].ToolName != echoToolName {
		t.Errorf("expected the tool name on the result, got %q", got[4].ToolName)
	}
}

// Provider-supplied ids are recorded verbatim.
func TestTranscriptSink_ChatPathKeepsProviderIDs(t *testing.T) {
	mock := &scenarioMockModel{responses: []mockResponse{
		{message: &schema.Message{}, toolCalls: []schema.ToolCall{echoCall("toolu_01A")}},
		{message: &schema.Message{Content: transcriptOK}},
	}}
	ag := newEchoAgent(t, mock)

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "t1", rec.emit)
	if _, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}

	got := rec.all()
	if len(got) != 2 || got[0].ToolCalls[0].ID != "toolu_01A" || got[1].ToolCallID != "toolu_01A" {
		t.Fatalf("expected the provider id preserved on call and result, got %+v", got)
	}
}

// A resumed attempt continues the iteration numbering from the groups
// already present after the last user message, so ids never collide.
func TestTranscriptSink_ResumedAttemptContinuesIteration(t *testing.T) {
	mock := &scenarioMockModel{responses: []mockResponse{
		{message: &schema.Message{}, toolCalls: []schema.ToolCall{echoCall("")}},
		{message: &schema.Message{Content: transcriptOK}},
	}}
	ag := newEchoAgent(t, mock)

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "t2", rec.emit)
	input := []*schema.Message{
		schema.UserMessage("hi"),
		schema.UserMessage("<turn_context>"),
		schema.AssistantMessage("", []schema.ToolCall{echoCall("call_t2_1_0")}),
		schema.ToolMessage("echo:{}", "call_t2_1_0"),
	}
	if _, err := ag.RunMessagesStreaming(ctx, "cli", input, nil); err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}

	got := rec.all()
	if len(got) != 2 || got[0].ToolCalls[0].ID != "call_t2_2_0" {
		t.Fatalf("expected the resumed iteration to be numbered 2, got %+v", got)
	}
}

// streamFnModel serves a scripted stream per call.
type streamFnModel struct {
	calls  int
	stream func(call int) (*schema.StreamReader[*schema.Message], error)
}

var _ model.ToolCallingChatModel = (*streamFnModel)(nil)

func (m *streamFnModel) Generate(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error) {
	return nil, errors.New("not used")
}

func (m *streamFnModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	m.calls++

	return m.stream(m.calls)
}

func (m *streamFnModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// A stream that fails after a partial tool call records nothing: a truncated
// tool-call argument never enters the transcript.
func TestTranscriptSink_TruncatedStreamEmitsNothing(t *testing.T) {
	m := &streamFnModel{stream: func(int) (*schema.StreamReader[*schema.Message], error) {
		sr, sw := schema.Pipe[*schema.Message](2)
		go func() {
			defer sw.Close()
			sw.Send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
				Type: "function", Function: schema.FunctionCall{Name: echoToolName, Arguments: `{"te`},
			}}}, nil)
			sw.Send(nil, errors.New("connection reset"))
		}()

		return sr, nil
	}}
	ag := newEchoAgent(t, m)

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "t3", rec.emit)
	outcome, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, func(string) {})
	if err == nil {
		t.Fatal("expected the stream failure to fail the turn")
	}
	if !outcome.ToolCallStarted {
		t.Error("expected ToolCallStarted from the partial tool-call chunk")
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("expected no transcript emission for an interrupted stream, got %+v", got)
	}
}

func TestTranscriptSink_UnarmedIsNil(t *testing.T) {
	if id, fn := agent.TranscriptSink(context.Background()); id != "" || fn != nil {
		t.Fatal("expected no sink on a bare context")
	}
	if ctx := agent.WithTranscriptSink(context.Background(), "x", nil); ctx != context.Background() {
		t.Fatal("expected a nil sink to leave the context unchanged")
	}
}

// Agentic path: the sink receives the function call and its result, with the
// provider's call id preserved.
func TestTranscriptSink_AgenticPath(t *testing.T) {
	mock := &scenarioAgenticModel{responses: []agenticScenarioResponse{
		{message: agenticEchoToolCall()},
		{message: &schema.AgenticMessage{
			Role:          schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: transcriptOK})},
		}},
	}}

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: mock, Tools: []tool.BaseTool{echoTool{}}}, nil)
	if err != nil {
		t.Fatalf("agent.NewAgentic: %v", err)
	}

	rec := &sinkRecorder{}
	ctx := agent.WithTranscriptSink(context.Background(), "t4", rec.emit)
	outcome, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, func(string) {})
	if err != nil {
		t.Fatalf("RunMessagesStreaming: %v", err)
	}
	if outcome.Reply != transcriptOK {
		t.Fatalf("expected reply %q, got %q", transcriptOK, outcome.Reply)
	}

	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("expected assistant + result, got %+v", got)
	}
	if got[0].Role != schema.Assistant || len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != testCallID ||
		got[0].ToolCalls[0].Function.Name != echoToolName || got[0].ToolCalls[0].Function.Arguments != echoArgs {
		t.Errorf("unexpected assistant emission %+v", got[0])
	}
	if got[1].Role != schema.Tool || got[1].ToolCallID != testCallID || got[1].Content == "" {
		t.Errorf("unexpected result emission %+v", got[1])
	}
}

// orderLog records the order of sink completions and model calls.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *orderLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.events, ",")
}

// slowSink records a tool result only after a delay — a slow checkpoint
// append — so a model call that does not wait would overtake it.
func slowSink(log *orderLog) func(schema.Message) {
	return func(m schema.Message) {
		if m.Role == schema.Tool {
			time.Sleep(30 * time.Millisecond)
			log.add("saved")
		}
	}
}

// Spec "Tool iteration persisted as it happens": the next model call begins
// only after the consumer recorded the previous group, on both paths.
func TestTranscriptSink_BarrierOrdersPersistenceBeforeNextCall(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		log := &orderLog{}
		m := &streamFnModel{stream: func(call int) (*schema.StreamReader[*schema.Message], error) {
			log.add("model")
			if call == 1 {
				return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage("", []schema.ToolCall{echoCall("")})}), nil
			}

			return schema.StreamReaderFromArray([]*schema.Message{schema.AssistantMessage(transcriptOK, nil)}), nil
		}}
		ag := newEchoAgent(t, m)

		ctx := agent.WithTranscriptSink(context.Background(), "b1", slowSink(log))
		if _, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
			t.Fatalf("RunMessagesStreaming: %v", err)
		}
		if got := log.String(); got != "model,saved,model" {
			t.Fatalf("expected the group saved before the next model call, got %q", got)
		}
	})

	t.Run("agentic", func(t *testing.T) {
		log := &orderLog{}
		final := &schema.AgenticMessage{
			Role:          schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: transcriptOK})},
		}
		calls := 0
		mock := &scenarioAgenticModel{streamFn: func(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
			log.add("model")
			calls++
			if calls == 1 {
				return schema.StreamReaderFromArray([]*schema.AgenticMessage{agenticEchoToolCall()}), nil
			}

			return schema.StreamReaderFromArray([]*schema.AgenticMessage{final}), nil
		}}
		ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: mock, Tools: []tool.BaseTool{echoTool{}}}, nil)
		if err != nil {
			t.Fatalf("agent.NewAgentic: %v", err)
		}

		ctx := agent.WithTranscriptSink(context.Background(), "b2", slowSink(log))
		if _, err := ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, nil); err != nil {
			t.Fatalf("RunMessagesStreaming: %v", err)
		}
		if got := log.String(); got != "model,saved,model" {
			t.Fatalf("expected the group saved before the next model call, got %q", got)
		}
	})
}

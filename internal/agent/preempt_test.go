package agent_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/agent"
)

// fakeSource is a controllable PreemptSource.
type fakeSource struct {
	mu     sync.Mutex
	due    bool
	reason agent.PreemptReason
	sig    chan struct{}
	closed bool
}

func newFakeSource() *fakeSource {
	return &fakeSource{reason: agent.PreemptSteer, sig: make(chan struct{})}
}

func (f *fakeSource) PreemptReason() (agent.PreemptReason, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.reason, f.due
}

// Signal returns the live channel; it only closes on fire, so tests driving
// tool-boundary preemption (makeDue) never wake the model-call watcher.
func (f *fakeSource) Signal() <-chan struct{} { return f.sig }

// makeDue marks a preemption due without signalling (boundary hooks only).
func (f *fakeSource) makeDue() {
	f.mu.Lock()
	f.due = true
	f.mu.Unlock()
}

// fire marks a preemption due and wakes the model-call watcher.
func (f *fakeSource) fire() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.due = true
	if !f.closed {
		f.closed = true
		close(f.sig)
	}
}

// fnModel is a ToolCallingChatModel whose Stream is a function of the call
// number (1-based).
type fnModel struct {
	n      atomic.Int32
	stream func(ctx context.Context, n int) (*schema.StreamReader[*schema.Message], error)
}

var _ model.ToolCallingChatModel = (*fnModel)(nil)

func (m *fnModel) Generate(ctx context.Context, in []*schema.Message, o ...model.Option) (*schema.Message, error) {
	r, err := m.Stream(ctx, in, o...)
	if err != nil {
		return nil, err
	}

	return schema.ConcatMessageStream(r)
}

func (m *fnModel) Stream(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.stream(ctx, int(m.n.Add(1)))
}

func (m *fnModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) { return m, nil }

func oneMsg(m *schema.Message) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{m})
}

func toolCallMsg() *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{echoCall("call_1")}}
}

// blockedStream never produces a chunk until its context ends, then fails
// with the context error — an in-flight model call.
func blockedStream(ctx context.Context) *schema.StreamReader[*schema.Message] {
	sr, sw := schema.Pipe[*schema.Message](1)

	go func() {
		defer sw.Close()
		<-ctx.Done()
		sw.Send(nil, ctx.Err())
	}()

	return sr
}

// hookTool runs fn when executed.
type hookTool struct{ fn func() }

func (hookTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: echoToolName, Desc: "hook"}, nil
}

func (h hookTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	if h.fn != nil {
		h.fn()
	}

	return "result", nil
}

func newToolAgent(t *testing.T, m model.ToolCallingChatModel, tl tool.BaseTool) *agent.Agent {
	t.Helper()

	ag, err := agent.New(context.Background(), agent.Config{ChatModel: m, SystemPrompt: agent.BuildSystemPrompt(), Tools: []tool.BaseTool{tl}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	return ag
}

func runWith(ag agent.Runner, src agent.PreemptSource, rec *sinkRecorder, onChunk func(string)) (agent.TurnOutcome, error) {
	ctx := agent.WithPreemptSource(context.Background(), src)
	ctx = agent.WithTranscriptSink(ctx, "ab12cd34", rec.emit)

	return ag.RunMessagesStreaming(ctx, "cli", []*schema.Message{schema.UserMessage("hi")}, onChunk)
}

func requirePreempt(t *testing.T, err error, reason agent.PreemptReason, point agent.PreemptPoint) {
	t.Helper()

	pe, ok := agent.IsPreempt(err)
	if !ok {
		t.Fatalf("err = %v, want a PreemptError", err)
	}

	if pe.Reason != reason || pe.Point != point {
		t.Fatalf("preempt = %s/%s, want %s/%s", pe.Reason, pe.Point, reason, point)
	}
}

func TestIsPreempt_SurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &agent.PreemptError{Reason: agent.PreemptStop, Point: agent.PointAfterTools}))
	if pe, ok := agent.IsPreempt(err); !ok || pe.Reason != agent.PreemptStop {
		t.Fatalf("IsPreempt(%v) = %v,%v", err, pe, ok)
	}

	if _, ok := agent.IsPreempt(errors.New("plain")); ok {
		t.Fatal("a plain error is not a preemption")
	}
}

// before_tools: the assistant message with the proposed calls is recorded,
// nothing executes, and the sentinel surfaces.
func TestPreempt_ChatBeforeTools(t *testing.T) {
	src := newFakeSource()
	var executed atomic.Int32

	m := &fnModel{stream: func(_ context.Context, n int) (*schema.StreamReader[*schema.Message], error) {
		if n == 1 {
			src.makeDue() // the candidate arrives while the call is streaming

			return oneMsg(toolCallMsg()), nil
		}

		t.Errorf("model call %d: no further call after a veto", n)

		return oneMsg(&schema.Message{Role: schema.Assistant, Content: "x"}), nil
	}}

	rec := &sinkRecorder{}
	outcome, err := runWith(newToolAgent(t, m, hookTool{fn: func() { executed.Add(1) }}), src, rec, func(string) {})

	requirePreempt(t, err, agent.PreemptSteer, agent.PointBeforeTools)

	if executed.Load() != 0 {
		t.Fatal("a vetoed tool call must not execute")
	}

	got := rec.all()
	if len(got) != 1 || got[0].Role != schema.Assistant || len(got[0].ToolCalls) != 1 {
		t.Fatalf("transcript = %+v, want only the assistant message with its proposed call", got)
	}

	if !outcome.ToolCallStarted || outcome.ToolCallCompleted || outcome.ContentDelivered {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// after_tools: every tool-result event is consumed (recorded) before the
// sentinel surfaces, and the next model call does not start.
func TestPreempt_ChatAfterTools(t *testing.T) {
	src := newFakeSource()

	m := &fnModel{stream: func(_ context.Context, n int) (*schema.StreamReader[*schema.Message], error) {
		if n == 1 {
			return oneMsg(toolCallMsg()), nil
		}

		t.Errorf("model call %d must not start after the preemption", n)

		return oneMsg(&schema.Message{Role: schema.Assistant, Content: "x"}), nil
	}}

	rec := &sinkRecorder{}
	outcome, err := runWith(newToolAgent(t, m, hookTool{fn: src.makeDue}), src, rec, func(string) {})

	requirePreempt(t, err, agent.PreemptSteer, agent.PointAfterTools)

	got := rec.all()
	if len(got) != 2 || got[0].Role != schema.Assistant || got[1].Role != schema.Tool {
		t.Fatalf("transcript = %+v, want assistant then its tool result", got)
	}

	if !outcome.ToolCallStarted || !outcome.ToolCallCompleted {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// model_call: an in-flight call that delivered nothing is cancelled, nothing
// of it is recorded or delivered.
func TestPreempt_ChatModelCall(t *testing.T) {
	src := newFakeSource()
	started := make(chan struct{})

	m := &fnModel{stream: func(ctx context.Context, _ int) (*schema.StreamReader[*schema.Message], error) {
		close(started)

		return blockedStream(ctx), nil
	}}

	go func() {
		<-started
		time.Sleep(20 * time.Millisecond)
		src.fire()
	}()

	rec := &sinkRecorder{}
	delivered := false
	outcome, err := runWith(newToolAgent(t, m, hookTool{}), src, rec, func(string) { delivered = true })

	requirePreempt(t, err, agent.PreemptSteer, agent.PointModelCall)

	if len(rec.all()) != 0 || delivered || outcome.ContentDelivered {
		t.Fatalf("nothing of a preempted call may be recorded or delivered: %+v delivered=%v outcome=%+v", rec.all(), delivered, outcome)
	}
}

// A candidate pending before the call starts preempts it as soon as it begins.
func TestPreempt_ChatModelCall_PendingBeforeStart(t *testing.T) {
	src := newFakeSource()
	src.fire()

	m := &fnModel{stream: func(ctx context.Context, _ int) (*schema.StreamReader[*schema.Message], error) {
		return blockedStream(ctx), nil
	}}

	_, err := runWith(newToolAgent(t, m, hookTool{}), src, &sinkRecorder{}, func(string) {})
	requirePreempt(t, err, agent.PreemptSteer, agent.PointModelCall)
}

// A stop preempts with its own reason.
func TestPreempt_ChatStopReason(t *testing.T) {
	src := newFakeSource()
	src.reason = agent.PreemptStop

	m := &fnModel{stream: func(_ context.Context, _ int) (*schema.StreamReader[*schema.Message], error) {
		src.makeDue()

		return oneMsg(toolCallMsg()), nil
	}}

	_, err := runWith(newToolAgent(t, m, hookTool{}), src, &sinkRecorder{}, func(string) {})
	requirePreempt(t, err, agent.PreemptStop, agent.PointBeforeTools)
}

// No source armed: a pass-through, the turn runs to its final answer.
func TestPreempt_NoSourceIsPassThrough(t *testing.T) {
	m := &fnModel{stream: func(_ context.Context, n int) (*schema.StreamReader[*schema.Message], error) {
		if n == 1 {
			return oneMsg(toolCallMsg()), nil
		}

		return oneMsg(&schema.Message{Role: schema.Assistant, Content: transcriptOK}), nil
	}}

	outcome, err := newToolAgent(t, m, hookTool{}).RunMessagesStreaming(context.Background(), "cli", []*schema.Message{schema.UserMessage("hi")}, func(string) {})
	if err != nil || outcome.Reply != "done" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

// The delivery CAS: whichever wins, a delivered chunk and a preemption never
// coexist.
func TestPreempt_ChatDeliveryCASRace(t *testing.T) {
	for i := 0; i < 300; i++ {
		src := newFakeSource()

		m := &fnModel{stream: func(_ context.Context, _ int) (*schema.StreamReader[*schema.Message], error) {
			go func() {
				time.Sleep(time.Duration(i%5) * 100 * time.Microsecond)
				src.fire()
			}()

			return oneMsg(&schema.Message{Role: schema.Assistant, Content: "final"}), nil
		}}

		var chunks atomic.Int32
		outcome, err := runWith(newToolAgent(t, m, hookTool{}), src, &sinkRecorder{}, func(string) { chunks.Add(1) })

		if pe, ok := agent.IsPreempt(err); ok {
			if chunks.Load() != 0 || outcome.ContentDelivered {
				t.Fatalf("iteration %d: preempted (%s) yet content delivered (%d chunks)", i, pe.Point, chunks.Load())
			}

			continue
		}

		if err != nil || chunks.Load() == 0 || !outcome.ContentDelivered {
			t.Fatalf("iteration %d: not preempted, want delivery: err=%v chunks=%d outcome=%+v", i, err, chunks.Load(), outcome)
		}
	}
}

// ── agentic (`responses`) path ───────────────────────────────────────────

func newAgenticToolAgent(t *testing.T, m model.AgenticModel, tl tool.BaseTool) *agent.AgenticAgent {
	t.Helper()

	ag, err := agent.NewAgentic(context.Background(), agent.AgenticConfig{Model: m, Tools: []tool.BaseTool{tl}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	return ag
}

func TestPreempt_AgenticBeforeTools(t *testing.T) {
	src := newFakeSource()
	var executed atomic.Int32

	m := &scenarioAgenticModel{streamFn: func(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
		src.makeDue()

		return schema.StreamReaderFromArray([]*schema.AgenticMessage{agenticEchoToolCall()}), nil
	}}

	rec := &sinkRecorder{}
	outcome, err := runWith(newAgenticToolAgent(t, m, hookTool{fn: func() { executed.Add(1) }}), src, rec, func(string) {})

	requirePreempt(t, err, agent.PreemptSteer, agent.PointBeforeTools)

	if executed.Load() != 0 {
		t.Fatal("a vetoed tool call must not execute")
	}

	if got := rec.all(); len(got) != 1 || len(got[0].ToolCalls) != 1 {
		t.Fatalf("transcript = %+v, want only the assistant message with its proposed call", got)
	}

	if !outcome.ToolCallStarted || outcome.ToolCallCompleted {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestPreempt_AgenticAfterTools(t *testing.T) {
	src := newFakeSource()
	calls := atomic.Int32{}

	m := &scenarioAgenticModel{streamFn: func(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
		if calls.Add(1) > 1 {
			t.Error("no model call may start after the preemption")
		}

		return schema.StreamReaderFromArray([]*schema.AgenticMessage{agenticEchoToolCall()}), nil
	}}

	rec := &sinkRecorder{}
	outcome, err := runWith(newAgenticToolAgent(t, m, hookTool{fn: src.makeDue}), src, rec, func(string) {})

	requirePreempt(t, err, agent.PreemptSteer, agent.PointAfterTools)

	if got := rec.all(); len(got) != 2 || got[1].Role != schema.Tool {
		t.Fatalf("transcript = %+v, want assistant then its tool result", got)
	}

	if !outcome.ToolCallCompleted {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestPreempt_AgenticModelCall(t *testing.T) {
	src := newFakeSource()
	started := make(chan struct{})

	m := &scenarioAgenticModel{streamFn: func(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
		close(started)

		sr, sw := schema.Pipe[*schema.AgenticMessage](1)
		go func() {
			defer sw.Close()
			<-ctx.Done()
			sw.Send(nil, ctx.Err())
		}()

		return sr, nil
	}}

	go func() {
		<-started
		time.Sleep(20 * time.Millisecond)
		src.fire()
	}()

	rec := &sinkRecorder{}
	delivered := false
	outcome, err := runWith(newAgenticToolAgent(t, m, hookTool{}), src, rec, func(string) { delivered = true })

	requirePreempt(t, err, agent.PreemptSteer, agent.PointModelCall)

	if len(rec.all()) != 0 || delivered || outcome.ContentDelivered {
		t.Fatalf("nothing of a preempted call may be recorded or delivered")
	}
}

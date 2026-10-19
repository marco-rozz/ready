package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/rs/zerolog"
)

// Shared fixture constants (goconst: repeated literals become constants).
const (
	testTurnChannel = "cli"
	testRefA0       = "a::m0"
	testRefB1       = "b::m1"
	testRefC2       = "c::m2"
	testProbeArgs   = `{"value": 1}`
	testDoneText    = "done"
	testToolType    = "function"
)

// ── Fakes ──────────────────────────────────────────────────────────────────

// fakeClock is a deterministic clock for cooldown/idle-window tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// runnerStep scripts one fakeRunner invocation.
type runnerStep struct {
	outcome agent.TurnOutcome
	err     error

	// chunk, when non-empty, is delivered via onChunk before the step
	// finishes — the "first token" that disarms the TTFT budget.
	chunk string

	// blockAfterChunk waits for ctx cancellation after delivering chunk.
	blockAfterChunk bool

	// block waits for ctx cancellation without delivering any chunk (the
	// silent candidate whose TTFT budget fires).
	block bool
}

// runnerCallInfo records what one fakeRunner invocation observed.
type runnerCallInfo struct {
	sessionKey  string
	msgs        []*schema.Message
	hadOnChunk  bool
	deadline    time.Time
	hasDeadline bool
}

// fakeRunner is a scripted agent.Runner.
type fakeRunner struct {
	mu    sync.Mutex
	calls []runnerCallInfo
	steps []runnerStep
}

var _ agent.Runner = (*fakeRunner)(nil)

func (r *fakeRunner) RunMessagesStreaming(ctx context.Context, sessionKey string, messages []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
	r.mu.Lock()
	idx := len(r.calls)
	if idx >= len(r.steps) {
		idx = len(r.steps) - 1
	}
	step := r.steps[idx]

	dl, hasDL := ctx.Deadline()
	r.calls = append(r.calls, runnerCallInfo{
		sessionKey:  sessionKey,
		msgs:        messages,
		hadOnChunk:  onChunk != nil,
		deadline:    dl,
		hasDeadline: hasDL,
	})
	r.mu.Unlock()

	if step.block {
		<-ctx.Done()

		return agent.TurnOutcome{}, ctx.Err()
	}

	if step.chunk != "" && onChunk != nil {
		agent.SignalLiveness(ctx)
		onChunk(step.chunk)
	}

	if step.blockAfterChunk {
		<-ctx.Done()

		return agent.TurnOutcome{ContentDelivered: true}, ctx.Err()
	}

	return step.outcome, step.err
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.calls)
}

func (r *fakeRunner) call(i int) runnerCallInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls[i]
}

// auxStep scripts one fakeAux invocation.
type auxStep struct {
	msg   *schema.Message
	err   error
	block bool
}

// fakeAux is a scripted model.BaseChatModel.
type fakeAux struct {
	mu    sync.Mutex
	calls int
	steps []auxStep
}

var _ model.BaseChatModel = (*fakeAux)(nil)

func (a *fakeAux) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	a.mu.Lock()
	a.calls++
	idx := a.calls - 1
	if idx >= len(a.steps) {
		idx = len(a.steps) - 1
	}
	step := a.steps[idx]
	a.mu.Unlock()

	if step.block {
		<-ctx.Done()

		return nil, ctx.Err()
	}

	return step.msg, step.err
}

func (a *fakeAux) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := a.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		if msg != nil {
			sw.Send(msg, nil)
		}
	}()

	return sr, nil
}

// probeMockModel is a model.ToolCallingChatModel whose Generate either
// completes a tool-call round trip (tool-capable) or answers in prose
// (text-only), recording every call's messages and attached tools.
type probeMockModel struct {
	mu        sync.Mutex
	genCalls  int
	toolsSeen [][]*schema.ToolInfo
	msgsSeen  [][]*schema.Message

	behavior int // 0 = tool-capable, 1 = text-only, 2 = always error
}

var _ model.ToolCallingChatModel = (*probeMockModel)(nil)

func (m *probeMockModel) Generate(_ context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	m.genCalls++
	common := model.GetCommonOptions(nil, opts...)
	m.toolsSeen = append(m.toolsSeen, common.Tools)
	m.msgsSeen = append(m.msgsSeen, input)
	idx := m.genCalls - 1
	behavior := m.behavior
	m.mu.Unlock()

	if behavior == 2 {
		return nil, errors.New("error 500: provider exploded") // test double
	}

	if behavior == 1 {
		return &schema.Message{Role: schema.Assistant, Content: "I would rather not call any tool."}, nil
	}

	if idx == 0 {
		return &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   "probe_call_1",
				Type: testToolType,
				Function: schema.FunctionCall{
					Name:      probeToolName,
					Arguments: testProbeArgs,
				},
			}},
		}, nil
	}

	return &schema.Message{Role: schema.Assistant, Content: "done"}, nil
}

func (m *probeMockModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func (m *probeMockModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *probeMockModel) stats() (gen int, tools [][]*schema.ToolInfo, msgs [][]*schema.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.genCalls, m.toolsSeen, m.msgsSeen
}

// ── Helpers ────────────────────────────────────────────────────────────────

func testRecovery() config.LLMRecoveryConfig {
	return config.LLMRecoveryConfig{
		Soft:            config.LadderConfig{InitialCooldown: 30 * time.Second, MaxCooldown: 5 * time.Minute},
		Hard:            config.LadderConfig{InitialCooldown: 5 * time.Minute, MaxCooldown: 1 * time.Hour},
		BackoffFactor:   2,
		ProbeTimeout:    2 * time.Second,
		IdleProbeWindow: 15 * time.Minute,
	}
}

func newTestOrch(clk *fakeClock, cands ...Candidate) *Orchestrator {
	return New(Config{
		Candidates:     cands,
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Clock:          clk.Now,
	}, zerolog.Nop())
}

// mkCand builds a constructed candidate with a scripted runner and aux model.
func mkCand(ref, provider, api string, layout llm.PromptLayout, r agent.Runner, aux model.BaseChatModel) Candidate {
	return Candidate{
		CandidateDescriptor: llm.CandidateDescriptor{
			Ref: ref, Provider: provider, ModelID: "m", API: api, Layout: layout,
		},
		Runner: r,
		Aux:    aux,
	}
}

func okOutcome(text string) agent.TurnOutcome {
	return agent.TurnOutcome{Reply: text, ContentDelivered: true}
}

func assembleRecorder(seen *[]llm.PromptLayout, msgs *[][]*schema.Message) func(llm.PromptLayout) []*schema.Message {
	return func(layout llm.PromptLayout) []*schema.Message {
		*seen = append(*seen, layout)
		msg := []*schema.Message{schema.SystemMessage("PREFIX"), schema.UserMessage("u")}
		*msgs = append(*msgs, msg)

		return msg
	}
}

// ── Ordered selection, exhaustion, promotion ──────────────────────────────

// Candidates are attempted in declared order, at most once
// each; an exhausted chain returns ErrChainExhausted wrapping the last
// candidate's error.
func TestRunTurn_ChainExhaustedInOneTurn(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 404: model not found")}}}
	r2 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 403: forbidden")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, r2, nil),
	)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, llm.ErrChainExhausted) {
		t.Fatalf("expected ErrChainExhausted, got: %v", err)
	}
	unwrapped := errors.Unwrap(err) // ChainExhaustedError -> last candidate's error
	if unwrapped == nil || !strings.Contains(unwrapped.Error(), "403") {
		t.Errorf("expected the last candidate's error to stay inspectable, got: %v (err: %v)", unwrapped, err)
	}

	for i, r := range []*fakeRunner{r0, r1, r2} {
		if got := r.callCount(); got != 1 {
			t.Errorf("candidate %d attempted %d times, want exactly 1", i, got)
		}
	}

	snap := o.Snapshot()
	if snap.State != StateUnavailable {
		t.Errorf("state = %q, want unavailable", snap.State)
	}
}

// Chain is exhausted in one turn and the second
// fallback's response is returned.
func TestRunTurn_SecondFallbackServesInSameTurn(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r2 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("from fallback 2")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, r2, nil),
	)

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "from fallback 2" {
		t.Errorf("reply = %q, want fallback 2's response", outcome.Reply)
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefC2 {
		t.Errorf("active = %q, want c::m2 (promoted after success)", snap.ActiveRef)
	}
	if !snap.Candidates[2].Verified {
		t.Error("successful candidate must be marked verified")
	}
}

// Candidate is promoted only after success — a
// fallback whose own first call fails never becomes active.
func TestRunTurn_PromotionOnlyAfterSuccess(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 404: model not found")}}}
	r1 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r2 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, r2, nil),
	)

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefC2 {
		t.Errorf("active = %q, want c::m2 — b::m1 failed and must not be active", snap.ActiveRef)
	}

	// Next turn: candidates 0 and 1 are cooling, so 2 serves directly.
	before := r2.callCount()
	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r0.callCount() != 1 || r1.callCount() != 1 {
		t.Error("cooling candidates must not be re-attempted on the next turn")
	}
	if r2.callCount() != before+1 {
		t.Error("active candidate should serve the next turn")
	}
}

// A mixed chat/`responses` chain is not filtered by protocol —
// selection is at the agent.Runner boundary, so protocol kinds are invisible.
func TestRunTurn_MixedProtocolChainNotFiltered(t *testing.T) {
	rChat := &fakeRunner{steps: []runnerStep{{err: errors.New("error 404: model not found")}}}
	rResponses := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("from responses")}}}

	o := newTestOrch(newFakeClock(),
		mkCand("chat::m", "chat", "chat_completions", llm.LayoutTailDynamic, rChat, nil),
		mkCand("resp::m", "resp", "responses", llm.LayoutTailDynamic, rResponses, nil),
	)

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "from responses" {
		t.Errorf("reply = %q, want the responses candidate's response", outcome.Reply)
	}
	if rResponses.callCount() != 1 {
		t.Error("responses candidate must have been attempted, not skipped")
	}
}

// Retry gate: a failure after a completed tool call ends the turn —
// the retry would restart a ReAct loop whose tool already ran.
func TestRunTurn_NoRetryAfterCompletedToolCall(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{outcome: agent.TurnOutcome{ToolCallCompleted: true}, err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("nope")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err == nil {
		t.Fatal("expected the original error to surface")
	}
	if r1.callCount() != 0 {
		t.Error("no other candidate may be invoked after a completed tool call")
	}
}

func TestRunTurn_NoRetryAfterToolCallStartedWithoutResult(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{outcome: agent.TurnOutcome{ToolCallStarted: true}, err: errors.New("error 500: result lost")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("must not run")}}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err == nil {
		t.Fatal("expected the original error")
	}
	if r0.callCount() != 1 || r1.callCount() != 0 {
		t.Fatalf("calls = primary %d fallback %d, want 1/0", r0.callCount(), r1.callCount())
	}
}

func TestRunTurn_TransientBlipRetriesSameCandidate(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{
		{err: errors.New("error 429: too many requests")},
		{outcome: okOutcome("recovered")},
	}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if outcome.Reply != "recovered" || r0.callCount() != 2 || r1.callCount() != 0 {
		t.Fatalf("outcome=%+v calls=%d/%d", outcome, r0.callCount(), r1.callCount())
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("a successful in-place retry must not demote the candidate")
	}
}

// An empty completion ("agent produced no response" — a 2026-10-05 live
// incident with glm-5.3-flash ending its turn after one tool round) is a
// silent-candidate malfunction: the same candidate must be retried (rewound
// to the last complete tool-call group) and, on repeated emptiness, the
// chain must advance with a soft demotion instead of aborting the turn.
func TestRunTurn_EmptyCompletionRetriesThenAdvances(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{
		{err: errors.New("agent produced no response")},
		{err: errors.New("agent produced no response")},
		{err: errors.New("agent produced no response")},
	}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("after empty fallback")}}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if outcome.Reply != "after empty fallback" || r0.callCount() != 3 || r1.callCount() != 1 {
		t.Fatalf("outcome=%+v calls=%d/%d", outcome, r0.callCount(), r1.callCount())
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want the fallback promoted", snap.ActiveRef)
	}
	c0 := snap.Candidates[0]
	if !c0.Cooling || c0.Ladder != llm.LadderSoft {
		t.Errorf("candidate 0 cooling=%v ladder=%v, want soft-ladder cooldown", c0.Cooling, c0.Ladder)
	}
}

func TestRunTurn_PermanentFailureSkipsSameCandidateRetry(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if r0.callCount() != 1 || r1.callCount() != 1 {
		t.Fatalf("calls = primary %d fallback %d, want 1/1", r0.callCount(), r1.callCount())
	}
}

func TestRunTurn_QuotaFailsFastAndSkipsProviderSiblings(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 429: quota exceeded")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("same provider")}}}
	r2 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("other provider")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand("shared::m0", "shared", "chat_completions", llm.LayoutTailDynamic, r0, nil),
			mkCand("shared::m1", "shared", "chat_completions", llm.LayoutTailDynamic, r1, nil),
			mkCand("other::m2", "other", "responses", llm.LayoutTailDynamic, r2, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if outcome.Reply != "other provider" || r0.callCount() != 1 || r1.callCount() != 0 || r2.callCount() != 1 {
		t.Fatalf("outcome=%+v calls=%d/%d/%d", outcome, r0.callCount(), r1.callCount(), r2.callCount())
	}
}

func TestRunTurn_ZeroRetriesRestoresSingleAttempt(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 500: transient")}, {outcome: okOutcome("would recover")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "chat_completions", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        0,
	}, zerolog.Nop())

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if r0.callCount() != 1 || r1.callCount() != 1 {
		t.Fatalf("calls = primary %d fallback %d, want 1/1", r0.callCount(), r1.callCount())
	}
}

func TestRunTurn_NonStreamingRetriesShareCandidateDeadline(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 500: transient")}, {outcome: okOutcome("recovered")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "chat_completions", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        1,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	first, second := r0.call(0), r0.call(1)
	if !first.hasDeadline || !second.hasDeadline || !first.deadline.Equal(second.deadline) {
		t.Fatalf("retry deadlines = %v / %v, want one shared candidate-cycle deadline", first.deadline, second.deadline)
	}
}

func TestRunTurn_DeadlineDuringBackoffDoesNotDemote(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 500: transient")}}}
	o := New(Config{
		Candidates:     []Candidate{mkCand(testRefA0, "a", "chat_completions", llm.LayoutTailDynamic, r0, nil)},
		Recovery:       testRecovery(),
		AttemptTimeout: 0,
		Retries:        2,
		RetryBackoff:   100 * time.Millisecond,
	}, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, func(string) {})
	if err == nil {
		t.Fatal("expected deadline failure")
	}
	if r0.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", r0.callCount())
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("outer deadline during backoff must not demote")
	}
}

func TestRunTurn_ConcurrentDemotionStopsPendingRetry(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 500: transient")}, {outcome: okOutcome("must not retry")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "chat_completions", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   100 * time.Millisecond,
	}, zerolog.Nop())

	done := make(chan error, 1)
	go func() {
		_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
			return []*schema.Message{schema.UserMessage("hi")}
		}}, nil)
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for r0.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r0.callCount() == 0 {
		t.Fatal("first attempt did not start")
	}

	cand := o.candidateCopy(0)
	o.demote(0, cand, llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}, "concurrent-test")

	if err := <-done; err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if r0.callCount() != 1 || r1.callCount() != 1 {
		t.Fatalf("calls = primary %d fallback %d, want 1/1", r0.callCount(), r1.callCount())
	}
}

func TestRunTurn_RetriesExhaustedDemotesOnce(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{
		{err: errors.New("error 500: one")},
		{err: errors.New("error 500: two")},
		{err: errors.New("error 500: three")},
	}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if r0.callCount() != 3 || r1.callCount() != 1 {
		t.Fatalf("calls = primary %d fallback %d, want 3/1", r0.callCount(), r1.callCount())
	}
	got := o.Snapshot().Candidates[0]
	if !got.Cooling || got.Ladder != llm.LadderSoft {
		t.Fatalf("candidate health = %+v, want one soft demotion", got)
	}
}

func TestRunTurn_OuterDeadlineDoesNotDemoteOrAdvance(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{block: true}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}
	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 0,
		Retries:        2,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, func(string) {})
	if err == nil {
		t.Fatal("expected outer deadline error")
	}
	if r0.callCount() != 1 || r1.callCount() != 0 {
		t.Fatalf("calls = primary %d fallback %d, want 1/0", r0.callCount(), r1.callCount())
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("outer deadline must not demote the candidate")
	}
}

// Retry gate: a stream failure after content was delivered ends the
// turn — a retry would duplicate already-streamed content.
func TestRunTurn_NoRetryAfterDeliveredContent(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{outcome: agent.TurnOutcome{ContentDelivered: true}, err: errors.New("error 500: boom")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("nope")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, func(string) {})
	if err == nil {
		t.Fatal("expected the original error to surface")
	}
	if r1.callCount() != 0 {
		t.Error("no other candidate may be invoked after content was delivered")
	}
}

// A request-shape 400 neither advances nor demotes.
func TestRunTurn_RequestShape400NoAdvanceNoDemote(t *testing.T) {
	var buf syncBuffer
	logger := zerolog.New(&buf)

	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("HTTP 400 Bad Request: system role not allowed")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("nope")}}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "anthropic", llm.LayoutHeadSafe, r0, nil),
			mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Clock:          newFakeClock().Now,
	}, logger)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err == nil {
		t.Fatal("expected the 400 error to surface unchanged")
	}
	if r1.callCount() != 0 {
		t.Error("a request-shape 400 must not advance the chain")
	}

	snap := o.Snapshot()
	if snap.Candidates[0].Cooling {
		t.Error("a request-shape 400 must not demote the candidate")
	}

	logged := buf.String()
	if !strings.Contains(logged, "request-shape") {
		t.Errorf("expected the diagnostic 400 WARN, got:\n%s", logged)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// ── Attempt budgets ────────────────────────────────────────────

// A silent streaming candidate is cut off at the TTFT budget,
// demoted on the hard ladder, and the fallback is attempted in the same turn.
func TestRunTurn_TTFTBudgetSilentCandidate(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{block: true}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, func(string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "fallback" {
		t.Errorf("reply = %q, want the fallback's response", outcome.Reply)
	}

	snap := o.Snapshot()
	if !snap.Candidates[0].Cooling || snap.Candidates[0].Ladder != llm.LadderHard {
		t.Errorf("silent candidate must be demoted on the hard ladder, got %+v", snap.Candidates[0])
	}
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want b::m1", snap.ActiveRef)
	}
}

// A slow but working candidate is not demoted — once
// the first chunk arrives, only the outer deadline applies.
func TestRunTurn_SlowButWorkingNotDemoted(t *testing.T) {
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("nope")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &delayedRunner{chunk: "tok", delay: 200 * time.Millisecond, reply: "slow but done"}, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, func(string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "slow but done" {
		t.Errorf("reply = %q", outcome.Reply)
	}

	snap := o.Snapshot()
	if snap.Candidates[0].Cooling {
		t.Error("a candidate that emitted its first chunk in time must not be demoted")
	}
	if r1.callCount() != 0 {
		t.Error("no other candidate may be attempted for a working candidate")
	}
}

type manyToolIterationsModel struct {
	calls      atomic.Int32
	iterations int32
}

var _ model.ToolCallingChatModel = (*manyToolIterationsModel)(nil)

func (m *manyToolIterationsModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	stream, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	return stream.Recv()
}

func (m *manyToolIterationsModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	n := m.calls.Add(1)
	if n <= m.iterations {
		msg := &schema.Message{
			Role: schema.Assistant,
			ToolCalls: []schema.ToolCall{{
				ID:   fmt.Sprintf("call-%d", n),
				Type: testToolType,
				Function: schema.FunctionCall{
					Name:      "slow-noop",
					Arguments: `{}`,
				},
			}},
		}

		return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
	}

	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: "all iterations complete"}}), nil
}

func (m *manyToolIterationsModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

type slowNoopTool struct{}

var _ tool.InvokableTool = slowNoopTool{}

func (slowNoopTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "slow-noop", Desc: "slow harmless tool"}, nil
}

func (slowNoopTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	timer := time.NewTimer(4 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestRunTurn_SeventeenToolIterationsDisarmBudgetWithThoughtTap(t *testing.T) {
	chatModel := &manyToolIterationsModel{iterations: 17}
	runner, err := agent.New(context.Background(), agent.Config{
		ChatModel:     chatModel,
		SystemPrompt:  "Use the tool until done.",
		Tools:         []tool.BaseTool{slowNoopTool{}},
		MaxIterations: 20,
	}, agent.NewTap(zerolog.Nop(), true))
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}

	o := New(Config{
		Candidates:     []Candidate{mkCand(testRefA0, "a", "chat_completions", llm.LayoutTailDynamic, runner, nil)},
		Recovery:       testRecovery(),
		AttemptTimeout: 20 * time.Millisecond,
	}, zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	outcome, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("run the sequence")}
	}}, func(string) {})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if outcome.Reply != "all iterations complete" || !outcome.ToolCallCompleted {
		t.Fatalf("outcome = %+v", outcome)
	}
	if elapsed := time.Since(started); elapsed <= 20*time.Millisecond {
		t.Fatalf("test did not exceed the attempt budget: %s", elapsed)
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("tool-heavy live candidate must not be demoted")
	}
}

// delayedRunner delivers a chunk immediately, then finishes after a delay
// exceeding the attempt budget (proving the budget is disarmed after TTFT).
type delayedRunner struct {
	chunk string
	delay time.Duration
	reply string
}

var _ agent.Runner = (*delayedRunner)(nil)

func (d *delayedRunner) RunMessagesStreaming(ctx context.Context, _ string, _ []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
	agent.SignalLiveness(ctx)
	if onChunk != nil {
		onChunk(d.chunk)
	}

	time.Sleep(d.delay)

	return agent.TurnOutcome{Reply: d.reply, ContentDelivered: true}, nil
}

// A non-streaming attempt receives a proportional share of the
// remaining outer deadline — one third with three untried candidates.
func TestRunTurn_NonStreamingProportionalShare(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r2 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
		mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, r2, nil),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	if _, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First attempt: ~1/3 of the outer deadline.
	first := r0.call(0)
	if !first.hasDeadline {
		t.Fatal("non-streaming attempt must carry a deadline")
	}
	share1 := first.deadline.Sub(start)
	if share1 > 1300*time.Millisecond {
		t.Errorf("first attempt budget = %s, want ~1s (one third of 3s)", share1)
	}

	// Last remaining candidate receives the whole remaining budget.
	third := r2.call(0)
	remaining := third.deadline.Sub(start)
	if remaining < 1500*time.Millisecond {
		t.Errorf("last attempt budget = %s, want most of the remaining 3s deadline", remaining)
	}
}

// An attempt-budget expiry on the non-streaming path is
// classified hard and the fallback is attempted.
func TestRunTurn_NonStreamingBudgetExpiryAdvances(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{block: true}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	outcome, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "fallback" {
		t.Errorf("reply = %q, want fallback", outcome.Reply)
	}

	snap := o.Snapshot()
	if !snap.Candidates[0].Cooling || snap.Candidates[0].Ladder != llm.LadderHard {
		t.Errorf("budget-expired candidate must be demoted hard, got %+v", snap.Candidates[0])
	}
}

// ── Cooldown ladders ───────────────────────────────────────────────────────

// Soft and hard ladders with the shared backoff factor, bounded by
// their max, reset on success.
func TestLadders_BackoffAndReset(t *testing.T) {
	clk := newFakeClock()
	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{}, nil),
	)

	cand := o.candidateCopy(0)
	soft := llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderSoft}
	hard := llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}

	// Soft: 30s, 60s, 120s, ... capped at 5m.
	o.demote(0, cand, soft, "test")
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != 30*time.Second {
		t.Errorf("soft cooldown #1 = %s, want 30s", got)
	}
	clk.Advance(time.Second)
	o.demote(0, cand, soft, "test")
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != 60*time.Second {
		t.Errorf("soft cooldown #2 = %s, want 60s", got)
	}
	for range 8 {
		clk.Advance(time.Second)
		o.demote(0, cand, soft, "test")
	}
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != 5*time.Minute {
		t.Errorf("soft cooldown must cap at 5m, got %s", got)
	}

	// Success resets.
	o.recordSuccess(0, true, "test")
	snap := o.Snapshot()
	if snap.Candidates[0].Cooling {
		t.Error("success must reset the cooldown")
	}

	// Hard: 5m, 10m, capped at 1h.
	o.demote(0, cand, hard, "test")
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != 5*time.Minute {
		t.Errorf("hard cooldown #1 = %s, want 5m", got)
	}
	clk.Advance(time.Second)
	o.demote(0, cand, hard, "test")
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != 10*time.Minute {
		t.Errorf("hard cooldown #2 = %s, want 10m", got)
	}
	for range 10 {
		clk.Advance(time.Second)
		o.demote(0, cand, hard, "test")
	}
	if got := o.Snapshot().Candidates[0].CooldownUntil.Sub(clk.Now()); got != time.Hour {
		t.Errorf("hard cooldown must cap at 1h, got %s", got)
	}
}

// Quota exhaustion demotes every candidate sharing the provider
// instance, on the hard ladder.
func TestDemote_ProviderScopedQuota(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 429: you exceeded your weekly usage limit")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(clk,
		mkCand("or::m0", "or", "chat_completions", llm.LayoutTailDynamic, r0, nil),
		mkCand("or::m1", "or", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{}, nil),
		mkCand("oc::m2", "oc", "responses", llm.LayoutTailDynamic, r1, nil),
	)

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	snap := o.Snapshot()
	if !snap.Candidates[0].Cooling || snap.Candidates[0].Ladder != llm.LadderHard {
		t.Errorf("quota-exhausted candidate must be demoted hard, got %+v", snap.Candidates[0])
	}
	if !snap.Candidates[1].Cooling || snap.Candidates[1].Ladder != llm.LadderHard {
		t.Errorf("the sibling candidate on the same provider instance must be demoted too, got %+v", snap.Candidates[1])
	}
	if snap.Candidates[2].Cooling {
		t.Error("a candidate on a different provider must not be demoted")
	}
	if snap.ActiveRef != "oc::m2" {
		t.Errorf("active = %q, want oc::m2", snap.ActiveRef)
	}
}

// ── Demand-aware recovery ──────────────────────────────────────────────────

// allCooling demotes every candidate so the orchestrator is unavailable.
func allCooling(o *Orchestrator, n int) {
	for i := range n {
		cand := o.candidateCopy(i)
		o.demote(i, cand, llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}, "test")
	}
}

// An idle daemon never probes.
func TestRecovery_IdleDaemonDoesNotProbe(t *testing.T) {
	clk := newFakeClock()
	mock := &probeMockModel{}
	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: mock}

	allCooling(o, 1)
	clk.Advance(10 * time.Minute) // cooldowns expired

	o.recoveryTick(context.Background())

	gen, _, _ := mock.stats()
	if gen != 0 {
		t.Errorf("idle daemon probed %d times, want 0", gen)
	}
}

// With recent traffic, the worker probes the expired
// candidate and promotes it.
func TestRecovery_ProbesExpiredAndPromotes(t *testing.T) {
	clk := newFakeClock()
	mock := &probeMockModel{}
	r := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}
	o := newTestOrch(clk, mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r, nil))
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: mock}

	allCooling(o, 1)
	clk.Advance(10 * time.Minute)

	o.markTraffic()
	o.recoveryTick(context.Background())

	gen, _, _ := mock.stats()
	if gen == 0 {
		t.Fatal("expected a recovery probe after traffic")
	}

	snap := o.Snapshot()
	if snap.State != StateAvailable || snap.ActiveRef != testRefA0 {
		t.Errorf("snapshot = %+v, want a::m0 active and available", snap)
	}
	if !snap.Candidates[0].Verified {
		t.Error("a successful probe must set verified")
	}
}

// Unproven fallbacks are never background-probed.
func TestRecovery_UnprovenNeverProbed(t *testing.T) {
	clk := newFakeClock()
	mock0 := &probeMockModel{}
	mock1 := &probeMockModel{}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: mock0}
	o.cands[1].cand.Model = llm.ResolvedModel{Chat: mock1}

	// Boot: candidate 0 probes ready; candidate 1 stays unproven.
	o.cands[0].health.verified = true
	o.recordSuccess(0, true, "boot")
	// Simulate traffic and many ticks.
	o.markTraffic()
	for range 5 {
		o.recoveryTick(context.Background())
	}

	gen0, _, _ := mock0.stats()
	gen1, _, _ := mock1.stats()
	if gen1 != 0 {
		t.Errorf("unproven fallback was background-probed %d times, want 0", gen1)
	}
	if gen0 != 0 {
		t.Errorf("healthy active candidate was probed %d times, want 0", gen0)
	}
}

// A user turn while every candidate is cooling forces one attempt
// of the highest-priority candidate, through its cooldown.
func TestRunTurn_ForcedAttemptThroughCooldown(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: still broken")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	allCooling(o, 2)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if !errors.Is(err, llm.ErrChainExhausted) {
		t.Fatalf("expected chain exhaustion, got: %v", err)
	}

	// The forced attempt hit the highest-priority candidate despite cooldown,
	// at most once this turn; the second candidate stays within cooldown and
	// is NOT force-attempted (at most one forced attempt per turn).
	if r0.callCount() != 1 {
		t.Errorf("highest-priority candidate forced %d times, want 1", r0.callCount())
	}
	if r1.callCount() != 0 {
		t.Errorf("second candidate attempted %d times, want 0 (one forced attempt per turn)", r1.callCount())
	}
}

// The forced attempt happens at most once per candidate per turn.
func TestRunTurn_ForcedAttemptAtMostOncePerTurn(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: broken")}}}

	o := newTestOrch(clk, mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil))

	allCooling(o, 1)

	_, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if !errors.Is(err, llm.ErrChainExhausted) {
		t.Fatalf("expected exhaustion, got: %v", err)
	}
	if r0.callCount() != 1 {
		t.Errorf("candidate attempted %d times in one turn, want exactly 1", r0.callCount())
	}
}

// A recovered higher-priority fallback replaces a
// lower-priority active reference.
func TestRecovery_HigherPriorityFallbackPromoted(t *testing.T) {
	clk := newFakeClock()
	mocks := []*probeMockModel{{}, {}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, nil),
		mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("z")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: mocks[0]}
	o.cands[1].cand.Model = llm.ResolvedModel{Chat: mocks[1]}

	// Candidate 2 active (recovered-to state), candidates 0 and 1 cooling.
	o.recordSuccess(2, true, "test")
	allCoolingExcept := func(except int) {
		for i := range 3 {
			if i == except {
				continue
			}
			c := o.candidateCopy(i)
			o.demote(i, c, llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}, "test")
		}
	}
	allCoolingExcept(2)

	// Advance past candidate 1's cooldown but NOT candidate 0's (its demotion
	// is re-armed each tick by the failing probe... here simply keep 0 within
	// cooldown by giving it a longer one).
	o.mu.Lock()
	o.cands[0].health.cooldownUntil = clk.Now().Add(time.Hour)
	o.mu.Unlock()
	clk.Advance(10 * time.Minute) // candidate 1's 5m cooldown expired

	o.markTraffic()
	o.recoveryTick(context.Background())

	gen0, _, _ := mocks[0].stats()
	gen1, _, _ := mocks[1].stats()
	if gen0 != 0 {
		t.Error("candidate 0 is still cooling and must not be probed")
	}
	if gen1 == 0 {
		t.Fatal("expected candidate 1's cooldown-expired probe")
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want b::m1 to replace c::m2", snap.ActiveRef)
	}
}

// ── Unproven candidates ────────────────────────────────────────────────────

// An unproven candidate is attempted directly at failover, without
// a preceding readiness probe, and a successful attempt marks it verified.
func TestRunTurn_UnprovenCandidateAttemptedDirectly(t *testing.T) {
	clk := newFakeClock()
	mock1 := &probeMockModel{}
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 404: model not found")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("from unproven")}}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)
	o.cands[1].cand.Model = llm.ResolvedModel{Chat: mock1}

	// Boot state: candidate 0 verified+active (as if it had probed ready),
	// candidate 1 unproven.
	o.cands[0].health.verified = true
	o.recordSuccess(0, true, "boot")

	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.Reply != "from unproven" {
		t.Errorf("reply = %q, want the unproven candidate's response", outcome.Reply)
	}

	gen1, _, _ := mock1.stats()
	if gen1 != 0 {
		t.Errorf("unproven candidate was probed %d times before use, want 0", gen1)
	}

	snap := o.Snapshot()
	if !snap.Candidates[1].Verified {
		t.Error("a successful attempt must set verified")
	}
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want b::m1", snap.ActiveRef)
	}
}

// ── Prompt assembly per attempt (task 5.4) ────────────────────────────────

// A tail-dynamic → head-safe retry assembles head-safe for the
// head-safe candidate, and a same-layout retry is byte-identical.
func TestRunTurn_PerAttemptAssembly(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}
	rSame := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	t.Run("tail-dynamic to head-safe retry", func(t *testing.T) {
		var layouts []llm.PromptLayout
		var msgs [][]*schema.Message

		o := newTestOrch(newFakeClock(),
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "anthropic", llm.LayoutHeadSafe, r1, nil),
		)

		if _, err := o.RunTurn(context.Background(), TurnRequest{
			SessionKey: testTurnChannel,
			Assemble:   assembleRecorder(&layouts, &msgs),
		}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(layouts) != 2 || layouts[0] != llm.LayoutTailDynamic || layouts[1] != llm.LayoutHeadSafe {
			t.Errorf("layouts = %v, want [tail-dynamic head-safe]", layouts)
		}
		if got := r1.call(0).msgs[0].Content; got != "PREFIX" {
			t.Errorf("head-safe candidate received %q, want the same prefix", got)
		}
	})

	t.Run("same-layout retry is byte-identical", func(t *testing.T) {
		var layouts []llm.PromptLayout
		var msgs [][]*schema.Message

		rr0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
		rr1 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}

		o := newTestOrch(newFakeClock(),
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, rr0, nil),
			mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, rr1, nil),
			mkCand(testRefC2, "c", "chat_completions", llm.LayoutTailDynamic, rSame, nil),
		)

		if _, err := o.RunTurn(context.Background(), TurnRequest{
			SessionKey: testTurnChannel,
			Assemble:   assembleRecorder(&layouts, &msgs),
		}, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(layouts) != 3 {
			t.Fatalf("layouts = %v, want three attempts", layouts)
		}
		for i, l := range layouts {
			if l != llm.LayoutTailDynamic {
				t.Errorf("layout %d = %v, want tail-dynamic", i, l)
			}
		}

		b0, _ := json.Marshal(msgs[0])
		b1, _ := json.Marshal(msgs[1])
		b2, _ := json.Marshal(msgs[2])
		if string(b0) != string(b1) || string(b1) != string(b2) {
			t.Error("same-layout retries must receive byte-identical conversations")
		}
	})
}

// ── Auxiliary consumers ────────────────────────────────────────────────────

// The aux model walks the chain with the shared
// health policy; exhaustion returns ErrChainExhausted.
func TestAuxModel_WalksChain(t *testing.T) {
	a0 := &fakeAux{steps: []auxStep{{err: errors.New("error 404: model not found")}}}
	a1 := &fakeAux{steps: []auxStep{{msg: &schema.Message{Role: schema.Assistant, Content: "formatted"}}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, a0),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, a1),
	)

	msg, err := o.AuxModel().Generate(context.Background(), []*schema.Message{schema.UserMessage("format this")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Content != "formatted" {
		t.Errorf("content = %q, want the second candidate's response", msg.Content)
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want b::m1", snap.ActiveRef)
	}
}

func TestAuxModel_Exhaustion(t *testing.T) {
	a0 := &fakeAux{steps: []auxStep{{err: errors.New("error 401: unauthorized")}}}
	a1 := &fakeAux{steps: []auxStep{{err: errors.New("error 403: forbidden")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, a0),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, a1),
	)

	_, err := o.AuxModel().Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if !errors.Is(err, llm.ErrChainExhausted) {
		t.Fatalf("expected ErrChainExhausted, got: %v", err)
	}
}

// A request-shape 400 on an aux call surfaces unchanged and demotes
// nothing.
func TestAuxModel_RequestShape400(t *testing.T) {
	a0 := &fakeAux{steps: []auxStep{{err: errors.New("HTTP 400 Bad Request")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "anthropic", llm.LayoutHeadSafe, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, a0),
	)

	_, err := o.AuxModel().Generate(context.Background(), []*schema.Message{schema.UserMessage("x")})
	if err == nil {
		t.Fatal("expected the 400 error to surface")
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("a 400 must not demote")
	}
}

// ── Construction failures, unavailable state, restart reset ───────────────

// A construction-failed candidate is unhealthy at boot; when
// no candidate is ready the daemon starts unavailable.
func TestBoot_NoCandidateReadyStartsUnavailable(t *testing.T) {
	clk := newFakeClock()
	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{}, nil),
	)
	// No registry: PreflightCatalog fails for the only candidate → none
	// ready → unavailable, but Boot returns without error.
	o.cfg.Registry = &config.LLMConfig{}

	o.Boot(context.Background())

	snap := o.Snapshot()
	if snap.State != StateUnavailable {
		t.Errorf("state = %q, want unavailable", snap.State)
	}
	if !snap.Candidates[0].Cooling {
		t.Error("the not-ready candidate must be cooling")
	}
}

// A construction failure marks only that candidate unhealthy.
func TestConstructionFailureIsolated(t *testing.T) {
	o := newTestOrch(newFakeClock(),
		func() Candidate {
			c := mkCand("bad::m", "bad", "ollama", llm.LayoutTailDynamic, nil, nil)
			c.ConstructErr = errors.New("unreadable api_key_file")

			return c
		}(),
		mkCand("good::m", "good", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
	)

	snap := o.Snapshot()
	if snap.Candidates[0].ConstructErr == "" {
		t.Error("construction error must be visible in the snapshot")
	}
	if snap.Candidates[0].Unproven {
		t.Error("a construction-failed candidate is not unproven, it is broken")
	}

	// The turn is served by the constructed candidate only.
	outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err != nil || outcome.Reply != "x" {
		t.Fatalf("turn = %q, %v — want served by the good candidate", outcome.Reply, err)
	}
}

// Health state resets on restart — a fresh orchestrator from
// the same configuration starts fully healthy.
func TestRestartResetsHealth(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	cands := []Candidate{
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, nil),
	}

	first := newTestOrch(clk, cands...)
	if _, err := first.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first.Snapshot().State == StateAvailable && !first.Snapshot().Candidates[0].Cooling {
		t.Fatal("setup: candidate 0 should be cooling after its failure")
	}

	// "Restart": same candidates (fresh orchestrator state).
	second := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, nil),
	)
	snap := second.Snapshot()
	for i, c := range snap.Candidates {
		if c.Cooling {
			t.Errorf("candidate %d must not carry cooldown across a restart", i)
		}
	}
}

// ── Concurrency ────────────────────────────────────────────────────────────

// Concurrent callers race safely (run under -race).
func TestRunTurn_ConcurrentCallers(t *testing.T) {
	// Two candidates: the first always fails hard, the second always
	// succeeds; concurrent turns must all complete with a valid answer and
	// leave consistent state.
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	const n = 16

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			outcome, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
				return []*schema.Message{schema.UserMessage("hi")}
			}}, nil)
			if err != nil || outcome.Reply != "ok" {
				errs[i] = fmt.Errorf("turn %d: %q, %w", i, outcome.Reply, err) // test
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("turn %d failed: %v", i, err)
		}
	}

	snap := o.Snapshot()
	if snap.ActiveRef != testRefB1 {
		t.Errorf("active = %q, want b::m1", snap.ActiveRef)
	}
}

// ── Transitions ────────────────────────────────────────────────────────────

// State transitions are observable through Subscribe.
func TestSubscribe_Transitions(t *testing.T) {
	clk := newFakeClock()
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	ch, cancel := o.Subscribe()
	defer cancel()

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Drain transitions (non-blocking).
	deadline := time.Now().Add(time.Second)
	var got bool
	for !got && time.Now().Before(deadline) {
		select {
		case tr := <-ch:
			if tr.To.ActiveRef == testRefB1 && tr.Reason != "" {
				got = true
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !got {
		t.Error("expected a promotion transition to b::m1")
	}
}

// ActiveLayout tracks the promoted candidate.
func TestActiveLayout_TracksPromotion(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{err: errors.New("error 401: unauthorized")}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("ok")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "anthropic", llm.LayoutHeadSafe, r1, nil),
	)

	if _, err := o.RunTurn(context.Background(), TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := o.ActiveLayout(); got != llm.LayoutHeadSafe {
		t.Errorf("ActiveLayout() = %v, want head-safe after promotion", got)
	}
}

// ── Caller cancellation ────────────────────────────────────────────────────

// D3: caller cancellation neither advances nor demotes.
func TestRunTurn_CallerCancellation(t *testing.T) {
	r0 := &fakeRunner{steps: []runnerStep{{block: true}}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("nope")}}}

	o := newTestOrch(newFakeClock(),
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, r1, nil),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := o.RunTurn(ctx, TurnRequest{SessionKey: testTurnChannel, Assemble: func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}}, nil)
	if err == nil {
		t.Fatal("expected the cancellation error to surface")
	}
	if r1.callCount() != 0 {
		t.Error("a cancelled turn must not advance the chain")
	}
	if o.Snapshot().Candidates[0].Cooling {
		t.Error("caller cancellation must not demote")
	}
}

// Suppress unused-import churn for io (kept for future stream fakes).
var _ io.Reader = io.Reader(nil)

// Recovery to the default — the default's cooldown expires while a
// fallback serves; the background probe restores the default as active.
func TestRecovery_ToDefault(t *testing.T) {
	clk := newFakeClock()
	mocks := []*probeMockModel{{}, {}}

	o := newTestOrch(clk,
		mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
		mkCand(testRefB1, "b", "chat_completions", llm.LayoutTailDynamic, &fakeRunner{steps: []runnerStep{{outcome: okOutcome("y")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: mocks[0]}
	o.cands[1].cand.Model = llm.ResolvedModel{Chat: mocks[1]}

	// Fallback 1 active and healthy; the default demoted (cooling).
	o.recordSuccess(1, true, "test")
	c0 := o.candidateCopy(0)
	o.demote(0, c0, llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}, "test")

	clk.Advance(10 * time.Minute) // default's 5m hard cooldown expired

	o.markTraffic()
	o.recoveryTick(context.Background())

	snap := o.Snapshot()
	if snap.ActiveRef != testRefA0 {
		t.Errorf("active = %q, want a::m0 — the recovered default must be restored", snap.ActiveRef)
	}
	if !snap.Candidates[0].Verified || snap.Candidates[0].Cooling {
		t.Errorf("recovered default must be verified and healthy, got %+v", snap.Candidates[0])
	}
}

// Every attempt of one turn — same-candidate retry and cross-protocol
// failover alike — receives the dispatcher's partitioned session key, so the
// thought-tap records of each new runner invocation (fresh turn_id/event_seq)
// stay correlated to the same session.
func TestExecuteTurn_RetryAndFailoverKeepSessionKey(t *testing.T) {
	const sessionKey = "telegram-123456789"

	r0 := &fakeRunner{steps: []runnerStep{
		{err: errors.New("error 429: too many requests")},
		{err: errors.New("error 429: too many requests")},
	}}
	r1 := &fakeRunner{steps: []runnerStep{{outcome: okOutcome("fallback")}}}

	o := New(Config{
		Candidates: []Candidate{
			mkCand(testRefA0, "a", "ollama", llm.LayoutTailDynamic, r0, nil),
			mkCand(testRefB1, "b", "responses", llm.LayoutTailDynamic, r1, nil),
		},
		Recovery:       testRecovery(),
		AttemptTimeout: 50 * time.Millisecond,
		Retries:        1,
		RetryBackoff:   time.Millisecond,
	}, zerolog.Nop())

	if _, err := o.ExecuteTurn(context.Background(), sessionKey, func(llm.PromptLayout) []*schema.Message {
		return []*schema.Message{schema.UserMessage("hi")}
	}, nil, nil); err != nil {
		t.Fatalf("ExecuteTurn() error = %v", err)
	}

	if r0.callCount() != 2 || r1.callCount() != 1 {
		t.Fatalf("calls = %d/%d, want retry then failover (2/1)", r0.callCount(), r1.callCount())
	}
	for _, r := range []*fakeRunner{r0, r1} {
		r.mu.Lock()
		for i, c := range r.calls {
			if c.sessionKey != sessionKey {
				t.Errorf("attempt %d session key = %q, want %q", i, c.sessionKey, sessionKey)
			}
		}
		r.mu.Unlock()
	}
}

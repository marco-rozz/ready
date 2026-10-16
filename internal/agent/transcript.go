package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type transcriptSinkKey struct{}

// transcriptSink is the per-turn hook installed by WithTranscriptSink.
type transcriptSink struct {
	turnID string
	emit   func(schema.Message)
	sync   *transcriptSync
}

// transcriptSyncTimeout bounds how long a model call waits for the
// transcript consumer to record the tool results already in its input.
const transcriptSyncTimeout = 5 * time.Second

// transcriptSync lets a model call wait until the event consumer has
// recorded every tool result already present in the model input. Eino's
// event iterator is an unbounded channel — the ReAct loop runs ahead of the
// consumer — so without it a completed group could still be unsaved when the
// next model call begins (spec "Tool iteration persisted as it happens").
type transcriptSync struct {
	mu        sync.Mutex
	baseline  int // tool results in the attempt's initial input (never emitted)
	processed int // tool results processed by the emitter this attempt
	changed   chan struct{}
}

func newTranscriptSync() *transcriptSync {
	return &transcriptSync{changed: make(chan struct{})}
}

// reset starts a new attempt whose input already carries baseline results.
func (s *transcriptSync) reset(baseline int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.baseline, s.processed = baseline, 0
}

// advance records one processed tool result.
func (s *transcriptSync) advance() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.processed++
	close(s.changed)
	s.changed = make(chan struct{})
}

// wait blocks until the consumer has processed the tool results present in a
// model input carrying inputResults of them, bounded by ctx and
// transcriptSyncTimeout (a timeout proceeds: the barrier only orders
// persistence, it never fails a turn).
func (s *transcriptSync) wait(ctx context.Context, inputResults int) {
	timer := time.NewTimer(transcriptSyncTimeout)
	defer timer.Stop()

	for {
		s.mu.Lock()
		done := s.processed >= inputResults-s.baseline
		changed := s.changed
		s.mu.Unlock()

		if done {
			return
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		}
	}
}

// waitTranscript applies the barrier of the sink armed on ctx, if any.
func waitTranscript(ctx context.Context, inputResults int) {
	if s, ok := ctx.Value(transcriptSinkKey{}).(transcriptSink); ok && s.sync != nil {
		s.sync.wait(ctx, inputResults)
	}
}

// WithTranscriptSink returns a context carrying the per-turn transcript sink
// (the session-transcript-persistence §D5, same pattern as
// WithLiveness). The agent loop emits, in materialization order, every
// assistant message carrying tool calls — only once its stream completed
// without error — and every tool result. The final answer is NOT emitted:
// it is the turn outcome's Reply, recorded by the caller.
//
// turnID seeds synthesized tool-call ids (call_<turnID>_<iter>_<idx>) and
// must match [a-zA-Z0-9_-]+. A nil fn leaves ctx unchanged, so probes and
// auxiliary consumers — which never arm a sink — emit nothing.
func WithTranscriptSink(ctx context.Context, turnID string, fn func(schema.Message)) context.Context {
	if fn == nil {
		return ctx
	}

	return context.WithValue(ctx, transcriptSinkKey{}, transcriptSink{turnID: turnID, emit: fn, sync: newTranscriptSync()})
}

// TranscriptSink returns the sink installed by WithTranscriptSink (turn id and
// emit function), or ("", nil) when none is armed.
func TranscriptSink(ctx context.Context) (string, func(schema.Message)) {
	if ctx == nil {
		return "", nil
	}

	if s, ok := ctx.Value(transcriptSinkKey{}).(transcriptSink); ok {
		return s.turnID, s.emit
	}

	return "", nil
}

// pendingCall is one tool call of the last emitted assistant message awaiting
// its result.
type pendingCall struct {
	origID   string // id as materialized by the provider (may be empty)
	id       string // recorded id (synthesized when origID is empty)
	name     string
	answered bool
}

// transcriptEmitter normalizes the in-turn transcript before it reaches the
// sink: empty tool-call ids receive a deterministic synthesized id and the
// matching results are remapped to it (§D3). A nil emitter is a no-op.
type transcriptEmitter struct {
	turnID  string
	emit    func(schema.Message)
	sync    *transcriptSync
	iter    int
	pending []pendingCall
}

// newTranscriptEmitter returns the emitter for one attempt, or nil when no
// sink is armed. The iteration counter continues from the tool-call
// iterations already present in input after its last user message — the
// recorded groups of earlier attempts of the same turn — so a resumed attempt
// never reuses an id.
func newTranscriptEmitter(ctx context.Context, input []*schema.Message) *transcriptEmitter {
	sink, ok := ctx.Value(transcriptSinkKey{}).(transcriptSink)
	if !ok {
		return nil
	}

	sink.sync.reset(trailingToolResults(input))

	return &transcriptEmitter{turnID: sink.turnID, emit: sink.emit, sync: sink.sync, iter: priorIterations(input)}
}

// trailingToolResults counts the tool results in input after its last user
// message — what the barrier compares against.
func trailingToolResults(input []*schema.Message) int {
	n := 0
	for i := len(input) - 1; i >= 0; i-- {
		m := input[i]
		if m == nil {
			continue
		}
		if m.Role == schema.User {
			break
		}
		if m.Role == schema.Tool {
			n++
		}
	}

	return n
}

// priorIterations counts the tool-call iterations already present in input
// after its last user message: the in-turn groups recorded by earlier
// attempts (or appended by Eino during this attempt's ReAct loop).
func priorIterations(input []*schema.Message) int {
	iter := 0
	for i := len(input) - 1; i >= 0; i-- {
		m := input[i]
		if m == nil {
			continue
		}
		if m.Role == schema.User {
			break
		}
		if m.Role == schema.Assistant && len(m.ToolCalls) > 0 {
			iter++
		}
	}

	return iter
}

// syntheticCallID is the protocol-portable id of a tool call the provider
// materialized without one (§D3): call_<turnID>_<iter>_<idx>.
func syntheticCallID(turnID string, iter, idx int) string {
	if turnID == "" {
		turnID = "0"
	}

	return fmt.Sprintf("call_%s_%d_%d", turnID, iter, idx)
}

// assistant emits a completed assistant message carrying tool calls.
func (e *transcriptEmitter) assistant(msg *schema.Message) {
	if e == nil || msg == nil || len(msg.ToolCalls) == 0 {
		return
	}

	e.iter++
	out := schema.Message{Role: schema.Assistant, Content: msg.Content}
	out.ToolCalls = make([]schema.ToolCall, len(msg.ToolCalls))
	e.pending = e.pending[:0]

	for idx, tc := range msg.ToolCalls {
		id := tc.ID
		if id == "" {
			id = syntheticCallID(e.turnID, e.iter, idx)
		}

		tc.ID = id
		if tc.Type == "" {
			tc.Type = "function"
		}
		out.ToolCalls[idx] = tc
		e.pending = append(e.pending, pendingCall{origID: msg.ToolCalls[idx].ID, id: id, name: tc.Function.Name})
	}

	e.emit(out)
}

// toolResult emits a tool result of the last recorded assistant message,
// remapping its call id onto the recorded one. A provider-supplied id matches directly; an empty id pairs
// positionally with the first unanswered call of the same tool name (results
// of parallel calls may arrive in any order), then with the first unanswered
// call.
func (e *transcriptEmitter) toolResult(msg *schema.Message) {
	if e == nil || msg == nil {
		return
	}
	// Processed (recorded or dropped) — release a model call waiting on it.
	defer e.sync.advance()

	// An unmatched result (its assistant message was never recorded) is
	// dropped: recording it would leave a result without its call.
	p := e.match(msg.ToolCallID, msg.ToolName)
	if p == nil {
		return
	}

	p.answered = true
	out := schema.Message{Role: schema.Tool, Content: msg.Content, ToolCallID: p.id, ToolName: msg.ToolName}
	if out.ToolName == "" {
		out.ToolName = p.name
	}

	e.emit(out)
}

func (e *transcriptEmitter) match(id, name string) *pendingCall {
	if id != "" {
		for i := range e.pending {
			if !e.pending[i].answered && (e.pending[i].origID == id || e.pending[i].id == id) {
				return &e.pending[i]
			}
		}

		return nil
	}

	for i := range e.pending {
		if !e.pending[i].answered && e.pending[i].origID == "" && e.pending[i].name == name {
			return &e.pending[i]
		}
	}
	for i := range e.pending {
		if !e.pending[i].answered && e.pending[i].origID == "" {
			return &e.pending[i]
		}
	}

	return nil
}

// toolCallIDModel is the chat-path model-layer hook of the transcript: it
// first waits (bounded) until the consumer recorded the tool results already
// in the input, then assigns the synthesized id to every whole tool call the
// provider materialized without one, BEFORE Eino sees the message: Eino's
// tools node keys executions by call id, so parallel empty-id calls (Ollama
// never sets one) would otherwise collide and lose results. The id is
// deterministic — the model input already carries the turn's earlier
// iterations — so it equals the one the transcript emitter would derive.
// Streamed deltas carrying an Index are left untouched: providers that
// stream indexed deltas send the id on the first one.
type toolCallIDModel struct {
	inner model.ToolCallingChatModel
}

// Compile-time interface assertion.
var _ model.ToolCallingChatModel = (*toolCallIDModel)(nil)

func withToolCallIDs(m model.ToolCallingChatModel) model.ToolCallingChatModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(*toolCallIDModel); ok {
		return m
	}

	return &toolCallIDModel{inner: m}
}

// idAssigner numbers the whole tool calls of one assistant message.
type idAssigner struct {
	turnID string
	iter   int
	next   int
}

func newIDAssigner(ctx context.Context, input []*schema.Message) *idAssigner {
	turnID, _ := TranscriptSink(ctx)

	return &idAssigner{turnID: turnID, iter: priorIterations(input) + 1}
}

// assign returns msg with ids filled in, copying the message and its
// ToolCalls slice only when a change is needed.
func (a *idAssigner) assign(msg *schema.Message) *schema.Message {
	if msg == nil || len(msg.ToolCalls) == 0 {
		return msg
	}

	var calls []schema.ToolCall
	for i, tc := range msg.ToolCalls {
		if tc.Index != nil {
			continue
		}

		idx := a.next
		a.next++
		if tc.ID != "" {
			continue
		}

		if calls == nil {
			calls = append([]schema.ToolCall(nil), msg.ToolCalls...)
		}
		calls[i].ID = syntheticCallID(a.turnID, a.iter, idx)
	}

	if calls == nil {
		return msg
	}

	out := *msg
	out.ToolCalls = calls

	return &out
}

// Generate assigns ids on the complete response.
func (m *toolCallIDModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	waitTranscript(ctx, trailingToolResults(input))

	msg, err := m.inner.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return newIDAssigner(ctx, input).assign(msg), nil
}

// Stream assigns ids chunk by chunk.
func (m *toolCallIDModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	waitTranscript(ctx, trailingToolResults(input))

	reader, err := m.inner.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	a := newIDAssigner(ctx, input)
	sr, sw := schema.Pipe[*schema.Message](1)

	go func() {
		defer reader.Close()
		defer sw.Close()

		for {
			chunk, recvErr := reader.Recv()
			if errors.Is(recvErr, io.EOF) {
				return
			}
			if recvErr != nil {
				sw.Send(nil, recvErr)

				return
			}
			if closed := sw.Send(a.assign(chunk), nil); closed {
				return
			}
		}
	}()

	return sr, nil
}

// WithTools re-wraps so the id assignment survives Eino's WithTools chain.
func (m *toolCallIDModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	inner, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}

	return withToolCallIDs(inner), nil
}

// transcriptBarrierAgenticModel applies the transcript barrier on the
// agentic (`responses`) path; ids need no synthesis there (the Responses API
// always returns call ids).
type transcriptBarrierAgenticModel struct {
	inner model.AgenticModel
}

// Compile-time interface assertion.
var _ model.AgenticModel = (*transcriptBarrierAgenticModel)(nil)

func withAgenticTranscriptBarrier(m model.AgenticModel) model.AgenticModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(*transcriptBarrierAgenticModel); ok {
		return m
	}

	return &transcriptBarrierAgenticModel{inner: m}
}

// trailingAgenticToolResults counts the function tool results after the last
// user message that is not itself a tool result.
func trailingAgenticToolResults(input []*schema.AgenticMessage) int {
	n := 0
	for i := len(input) - 1; i >= 0; i-- {
		m := input[i]
		if m == nil {
			continue
		}

		results := 0
		for _, b := range m.ContentBlocks {
			if b != nil && b.FunctionToolResult != nil {
				results++
			}
		}
		if m.Role == schema.AgenticRoleTypeUser && results == 0 {
			break
		}
		n += results
	}

	return n
}

// Generate waits for the transcript consumer, then delegates.
func (m *transcriptBarrierAgenticModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	waitTranscript(ctx, trailingAgenticToolResults(input))

	return m.inner.Generate(ctx, input, opts...)
}

// Stream waits for the transcript consumer, then delegates.
func (m *transcriptBarrierAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	waitTranscript(ctx, trailingAgenticToolResults(input))

	return m.inner.Stream(ctx, input, opts...)
}

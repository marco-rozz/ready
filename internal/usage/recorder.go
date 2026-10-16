package usage

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// extractTokenUsage reads the usage observation from one chat-path message.
// Providers attach it at different chunk positions (see the spike notes in
// the change design.md): the recorder keeps the LAST non-nil value per
// model call rather than summing across chunks (Anthropic streams emit the
// input tokens on MessageStart and the full input+output on MessageDelta —
// summation would double-count the prompt).
func extractTokenUsage(msg *schema.Message) (schema.TokenUsage, bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return schema.TokenUsage{}, false
	}

	return *msg.ResponseMeta.Usage, true
}

// extractAgenticTokenUsage is the `responses`-protocol counterpart: usage
// lives in AgenticResponseMeta.TokenUsage, emitted on the Generate result
// and on the stream's ResponseCompleted meta chunk.
func extractAgenticTokenUsage(msg *schema.AgenticMessage) (schema.TokenUsage, bool) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
		return schema.TokenUsage{}, false
	}

	return *msg.ResponseMeta.TokenUsage, true
}

// record attributes one usage observation to the accumulator in ctx, if
// any. Calls without an accumulator (probes, tests) record nothing.
func record(ctx context.Context, u schema.TokenUsage) {
	if acc := FromContext(ctx); acc != nil {
		acc.AddUsage(u, isAux(ctx))
	}
}

// RecordedChatModel is a transparent ToolCallingChatModel decorator that
// attributes each call's schema.TokenUsage to the session accumulator
// found in the call context. It wraps the resolved model BEFORE the agent
// is built (the toolnames normaliser and the ADK both see the wrapper), so
// every ReAct iteration and every failover attempt on any candidate is
// captured. Aux-consumer calls (marked WithAux in context) land on
// AuxCalls; everything else counts as LLMCalls.
type RecordedChatModel struct {
	base model.ToolCallingChatModel
}

// Compile-time assertion (code style: interface implementation validation).
var _ model.ToolCallingChatModel = (*RecordedChatModel)(nil)

// NewRecordedChatModel wraps base with usage recording. The wrapper is
// installed once per candidate at boot in cmd/serve.go:buildLLMCandidates.
func NewRecordedChatModel(base model.ToolCallingChatModel) *RecordedChatModel {
	return &RecordedChatModel{base: base}
}

// Generate delegates unchanged, then attributes the response's usage.
func (w *RecordedChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := w.base.Generate(ctx, input, opts...)
	if err == nil {
		if u, ok := extractTokenUsage(msg); ok {
			record(ctx, u)
		}
	}

	return msg, err
}

// Stream delegates unchanged, forwarding every chunk through a pipe while
// observing usage; the last non-nil observation is recorded on stream end.
func (w *RecordedChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, err := w.base.Stream(ctx, input, opts...)
	if err != nil || reader == nil {
		return reader, err
	}

	sr, sw := schema.Pipe[*schema.Message](0)

	go forwardRecorded(ctx, reader, sw, extractTokenUsage)

	return sr, nil
}

// WithTools re-wraps the derived model so the recorder survives the ADK's
// WithTools().Stream() call chain (same seam as openrouterStreamSafe).
func (w *RecordedChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	derived, err := w.base.WithTools(tools)
	if err != nil {
		return nil, err
	}

	return &RecordedChatModel{base: derived}, nil
}

// RecordedAgenticModel is the AgenticModel counterpart of RecordedChatModel
// for `responses`-protocol candidates (usage lives in
// AgenticResponseMeta.TokenUsage). AgenticModel has no WithTools — tools
// ride the per-request model.Option — so the wrapper is two methods.
type RecordedAgenticModel struct {
	base model.AgenticModel
}

// Compile-time assertion (code style: interface implementation validation).
var _ model.AgenticModel = (*RecordedAgenticModel)(nil)

// NewRecordedAgenticModel wraps an AgenticModel with usage recording.
func NewRecordedAgenticModel(base model.AgenticModel) *RecordedAgenticModel {
	return &RecordedAgenticModel{base: base}
}

// Generate delegates unchanged, then attributes the final message's usage.
func (w *RecordedAgenticModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	msg, err := w.base.Generate(ctx, input, opts...)
	if err == nil {
		if u, ok := extractAgenticTokenUsage(msg); ok {
			record(ctx, u)
		}
	}

	return msg, err
}

// Stream delegates unchanged, forwarding every chunk through a pipe while
// observing usage (the ResponseCompleted meta chunk carries the totals).
func (w *RecordedAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	reader, err := w.base.Stream(ctx, input, opts...)
	if err != nil || reader == nil {
		return reader, err
	}

	sr, sw := schema.Pipe[*schema.AgenticMessage](0)

	go forwardRecordedAgentic(ctx, reader, sw)

	return sr, nil
}

// forwardRecorded pumps chunks from reader into sw, keeping the last
// non-nil usage observation and recording it when the stream ends. The
// chunk payload is forwarded untouched — the recorder is invisible to the
// consumer.
func forwardRecorded[M interface {
	*schema.Message | *schema.AgenticMessage
}](
	ctx context.Context,
	reader *schema.StreamReader[M],
	sw *schema.StreamWriter[M],
	extract func(M) (schema.TokenUsage, bool),
) {
	defer reader.Close()
	defer sw.Close()

	var last schema.TokenUsage
	seen := false

	for {
		chunk, recvErr := reader.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			sw.Send(nil, recvErr)

			return
		}

		if u, ok := extract(chunk); ok {
			last = u
			seen = true
		}

		if closed := sw.Send(chunk, nil); closed {
			return
		}
	}

	// Stream completed without forwarding a final nil-usage marker: record
	// the last observed usage (never summed across chunks — see the spike
	// notes in the change design.md).
	if seen {
		record(ctx, last)
	}
}

// forwardRecordedAgentic specialises forwardRecorded for the agentic chunk
// type (Go type inference cannot bind the generic through the method
// receiver's package boundary cleanly in all call sites).
func forwardRecordedAgentic(
	ctx context.Context,
	reader *schema.StreamReader[*schema.AgenticMessage],
	sw *schema.StreamWriter[*schema.AgenticMessage],
) {
	forwardRecorded(ctx, reader, sw, extractAgenticTokenUsage)
}

package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// toAgenticMessages converts a plain []*schema.Message conversation (what the
// checkpoint store restores and what the formatter/dispatcher pipeline
// builds every turn) into the []*schema.AgenticMessage shape required by the
// agentic (Responses API) construction path.
//
// The session transcript carries the in-turn ReAct exchange
// (the session-transcript-persistence): an assistant message's
// tool calls become FunctionToolCall blocks and a tool result becomes a
// user-role FunctionToolResult block with the same call id — the shape
// Eino's own agentic tools node produces — so a transcript written under any
// protocol replays here.
func toAgenticMessages(messages []*schema.Message) []*schema.AgenticMessage {
	out := make([]*schema.AgenticMessage, 0, len(messages))

	for _, m := range messages {
		if m == nil {
			continue
		}

		switch m.Role {
		case schema.System:
			out = append(out, schema.SystemAgenticMessage(m.Content))
		case schema.Assistant:
			out = append(out, assistantAgenticMessageWithCalls(m.Content, m.ToolCalls))
		case schema.Tool:
			out = append(out, toolResultAgenticMessage(m))
		default:
			out = append(out, schema.UserAgenticMessage(m.Content))
		}
	}

	return out
}

// assistantAgenticMessage builds a minimal assistant-role AgenticMessage
// carrying a single AssistantGenText content block. There is no exported
// schema.AssistantAgenticMessage constructor (only System/User), so this is
// built by hand — mirroring the shape schema.SystemAgenticMessage /
// schema.UserAgenticMessage produce for their roles.
func assistantAgenticMessage(text string) *schema.AgenticMessage {
	return assistantAgenticMessageWithCalls(text, nil)
}

// assistantAgenticMessageWithCalls builds an assistant AgenticMessage with an
// optional AssistantGenText block (omitted when a tool-call message has no
// pre-action text) followed by one FunctionToolCall block per tool call.
func assistantAgenticMessageWithCalls(text string, calls []schema.ToolCall) *schema.AgenticMessage {
	msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant}

	if text != "" || len(calls) == 0 {
		msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: text}))
	}

	for _, tc := range calls {
		msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolCall{
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		}))
	}

	return msg
}

// toolResultAgenticMessage builds the user-role FunctionToolResult message
// answering tool call m.ToolCallID.
func toolResultAgenticMessage(m *schema.Message) *schema.AgenticMessage {
	ftr := &schema.FunctionToolResult{CallID: m.ToolCallID, Name: m.ToolName}
	if m.Content != "" {
		ftr.Content = []*schema.FunctionToolResultContentBlock{{
			Type: schema.FunctionToolResultContentBlockTypeText,
			Text: &schema.UserInputText{Text: m.Content},
		}}
	}

	return &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(ftr)},
	}
}

// agenticTranscript maps a materialized AgenticMessage onto the transcript
// shape emitted through the sink: the assistant message with its function
// tool calls (nil when it carries none), and one Role=Tool message per
// function tool result block. Server-side tool calls (MCP/server tools run by
// the provider) are not client transcript and are not recorded.
func agenticTranscript(msg *schema.AgenticMessage) (*schema.Message, []*schema.Message) {
	if msg == nil {
		return nil, nil
	}

	var calls []schema.ToolCall
	var results []*schema.Message

	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}

		if fc := block.FunctionToolCall; fc != nil {
			calls = append(calls, schema.ToolCall{
				ID:       fc.CallID,
				Type:     "function",
				Function: schema.FunctionCall{Name: fc.Name, Arguments: fc.Arguments},
			})
		}

		if fr := block.FunctionToolResult; fr != nil {
			var sb strings.Builder
			for _, c := range fr.Content {
				if c != nil && c.Text != nil {
					sb.WriteString(c.Text.Text)
				}
			}
			results = append(results, &schema.Message{
				Role:       schema.Tool,
				Content:    sb.String(),
				ToolCallID: fr.CallID,
				ToolName:   fr.Name,
			})
		}
	}

	var assistant *schema.Message
	if msg.Role == schema.AgenticRoleTypeAssistant && len(calls) > 0 {
		text, _ := agenticMessageText(msg)
		assistant = &schema.Message{Role: schema.Assistant, Content: text, ToolCalls: calls}
	}

	return assistant, results
}

// recordAgentic emits a materialized AgenticMessage through the transcript
// emitter (nil-safe).
func recordAgentic(rec *transcriptEmitter, msg *schema.AgenticMessage) {
	if rec == nil {
		return
	}

	assistant, results := agenticTranscript(msg)
	rec.assistant(assistant)
	for _, r := range results {
		rec.toolResult(r)
	}
}

// agenticMessageText extracts the concatenated AssistantGenText content from
// msg and reports whether the message also carries a tool-call content block
// (function, MCP, or server tool call) — i.e. whether this is an
// intermediate ReAct iteration rather than the turn's final answer. This is
// the AgenticMessage analogue of checking len(msg.ToolCalls) > 0 on a plain
// *schema.Message.
func agenticMessageText(msg *schema.AgenticMessage) (text string, hasToolCalls bool) {
	if msg == nil {
		return "", false
	}

	var sb strings.Builder

	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}

		if block.AssistantGenText != nil {
			sb.WriteString(block.AssistantGenText.Text)
		}

		if block.FunctionToolCall != nil || block.MCPToolCall != nil || block.ServerToolCall != nil {
			hasToolCalls = true
		}
	}

	return sb.String(), hasToolCalls
}

// agenticMessageHasToolResult reports whether msg carries any tool-result
// content block (function, MCP, server, or client-side tool-search tool
// result). On the agentic construction path a completed tool call is
// observable only this way: results are embedded as ContentBlocks within
// assistant AgenticMessages rather than emitted as separate Role=Tool events
// like on the chat path.
func agenticMessageHasToolResult(msg *schema.AgenticMessage) bool {
	if msg == nil {
		return false
	}

	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}

		if block.FunctionToolResult != nil ||
			block.MCPToolResult != nil ||
			block.ServerToolResult != nil ||
			block.ToolSearchFunctionToolResult != nil {
			return true
		}
	}

	return false
}

// readAgenticStreamFull reads a streaming AgenticMessage chunk-by-chunk,
// buffering AssistantGenText chunk text and forwarding it to onChunk only
// when the message ends without a tool-call content block. It also returns
// the materialized message — accumulated in the same read loop and
// concatenated with schema.ConcatAgenticMessages — which is nil when the
// stream failed (or could not be concatenated), so an incomplete message is
// never recorded in the transcript. The last return value reports whether
// any chunk carried a tool-result content block. Always closes stream;
// updates *lastErr on stream errors. This is the AgenticMessage analogue of
// readStreamingAssistantFull.
//
// criticalities-remediation (H6): like the chat path, pre-action text
// arriving before the tool-call block is never forwarded to any channel.
func readAgenticStreamFull(stream *schema.StreamReader[*schema.AgenticMessage], onChunk func(string), lastErr *error, signalLiveness func()) (text string, full *schema.AgenticMessage, hasToolCalls, hasToolResult bool) {
	defer stream.Close()

	var sb strings.Builder
	var buffered []string
	var chunks []*schema.AgenticMessage
	failed := false

	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			if lastErr != nil {
				*lastErr = recvErr
			}
			failed = true

			break
		}
		if chunk == nil {
			continue
		}
		if signalLiveness != nil {
			signalLiveness()
		}

		chunks = append(chunks, chunk)

		if agenticMessageHasToolResult(chunk) {
			hasToolResult = true
		}

		chunkText, chunkHasToolCalls := agenticMessageText(chunk)
		if chunkHasToolCalls {
			hasToolCalls = true
		}

		if chunkText != "" && !hasToolCalls {
			sb.WriteString(chunkText)
			buffered = append(buffered, chunkText)
		}
	}

	if !failed && len(chunks) > 0 {
		if merged, err := schema.ConcatAgenticMessages(chunks); err == nil {
			full = merged
		}
	}

	if hasToolCalls {
		return "", full, hasToolCalls, hasToolResult // intermediate — buffered pre-action text dropped
	}

	if onChunk != nil {
		for _, c := range buffered {
			onChunk(c)
		}
	}

	return sb.String(), full, hasToolCalls, hasToolResult
}

// AsBaseChatModel adapts an agentic model (model.AgenticModel) to the plain
// model.BaseChatModel ([]*schema.Message in, *schema.Message out) interface
// needed by auxiliary LLM consumers that don't need tool calling: the
// formatter post-step call (internal/formatter.Formatter) and the
// compaction summariser (internal/compaction). Both call
// sites only ever invoke Generate with a short system+user exchange and read
// back plain text, so the AgenticMessage round trip performed here (via
// toAgenticMessages / agenticMessageText) is lossless for their purposes.
func AsBaseChatModel(m model.AgenticModel) model.BaseChatModel {
	return &agenticBaseChatModel{inner: m}
}

// agenticBaseChatModel is the concrete adapter returned by AsBaseChatModel.
type agenticBaseChatModel struct {
	inner model.AgenticModel
}

// Compile-time interface assertion.
var _ model.BaseChatModel = (*agenticBaseChatModel)(nil)

// Generate converts input to AgenticMessages, calls the inner agentic model,
// and converts the response back to a plain assistant *schema.Message.
func (a *agenticBaseChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := a.inner.Generate(ctx, toAgenticMessages(input), opts...)
	if err != nil {
		return nil, err
	}

	text, _ := agenticMessageText(msg)

	return &schema.Message{Role: schema.Assistant, Content: text}, nil
}

// Stream serves a single chunk produced by Generate, then EOF. Neither
// auxiliary consumer (formatter post-step, compaction summariser) needs
// incremental delivery; this exists only to satisfy model.BaseChatModel.
func (a *agenticBaseChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := a.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		sw.Send(msg, nil)
	}()

	return sr, nil
}

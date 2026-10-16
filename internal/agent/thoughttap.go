package agent

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// reactEventMsg is the zerolog message string used for all ReAct tap records.
// Keeping it constant makes grep-based log filtering trivial.
const reactEventMsg = "react event"

// Tap is the thought-logging tap shared by both runner construction paths
// ([Agent] and [AgenticAgent]).  When enabled, each runner invocation starts a
// TurnTap which emits structured DEBUG records for every ReAct reasoning step,
// tool-call decision, and tool-result observation.
//
// When disabled (the default), all methods are no-ops and allocate nothing.
type Tap struct {
	logger  zerolog.Logger
	enabled bool
}

// NewTap creates a Tap bound to logger.  When enabled is false, the tap is
// fully inert and costs nothing beyond a single boolean check per event.
func NewTap(logger zerolog.Logger, enabled bool) *Tap {
	return &Tap{
		logger:  logger.With().Str("component", "agent").Logger(),
		enabled: enabled,
	}
}

// StartTurn creates a new TurnTap for a single RunMessages invocation.
// A fresh UUID v4 turn_id is generated and event_seq is initialised to 0.
// If the tap is nil or disabled, the returned *TurnTap is safe to call but
// performs no work.
func (t *Tap) StartTurn(sessionKey string) *TurnTap {
	if t == nil || !t.enabled {
		return &TurnTap{} // disabled sentinel — all methods are no-ops
	}

	return &TurnTap{
		tap:        t,
		sessionKey: sessionKey,
		turnID:     uuid.New().String(),
	}
}

// TurnTap is a handle for one RunMessages invocation.  It accumulates an
// event_seq counter and emits zerolog DEBUG records for each AgentEvent.
type TurnTap struct {
	tap        *Tap // nil when disabled
	sessionKey string
	turnID     string
	seq        atomic.Uint64
}

// Emit inspects a chat-path ev and emits zero or more DEBUG log records:
//
//   - event_type=thought, source=reasoning  — when msg.ReasoningContent is non-empty
//   - event_type=thought, source=content    — when msg.Content is non-empty AND the message carries ToolCalls
//   - event_type=decision                   — one per entry in msg.ToolCalls (tool_name, tool_call_id, arguments)
//   - event_type=observation                — when msg.Role == schema.Tool
//
// The final assistant reply (non-empty Content with no ToolCalls and Role=Assistant)
// is NOT logged to avoid duplicating what the channel layer already handles.
//
// If tt is nil or disabled, Emit returns immediately without accessing any field on ev.
func (tt *TurnTap) Emit(ev *adk.AgentEvent) {
	if tt == nil || tt.tap == nil {
		return
	}

	if ev == nil {
		return
	}

	// Skip error events — the agent loop handles them separately.
	if ev.Err != nil {
		return
	}

	if ev.Output == nil || ev.Output.MessageOutput == nil {
		return
	}

	msg, err := ev.Output.MessageOutput.GetMessage()
	if err != nil || msg == nil {
		return
	}

	switch msg.Role {
	case schema.Tool:
		tt.emitObservation(msg.ToolName, msg.ToolCallID, msg.Content)
	case schema.Assistant:
		tt.emitAssistant(msg)
	}
}

// emitAssistant logs thought and decision records for a chat assistant message.
func (tt *TurnTap) emitAssistant(msg adk.Message) {
	// Reasoning content (e.g. DeepSeek, o3, Claude extended thinking).
	if msg.ReasoningContent != "" {
		tt.emitThought("reasoning", msg.ReasoningContent)
	}

	// Non-reasoning models (e.g. gemma4:31b) externalise their ReAct text in
	// Content when they are about to call a tool.  Log it as source=content so
	// operators can observe the reasoning trace on default config.
	if msg.Content != "" && len(msg.ToolCalls) > 0 {
		tt.emitThought("content", msg.Content)
	}

	// One decision record per tool-call entry.
	for _, tc := range msg.ToolCalls {
		tt.emitDecision(tc.Function.Name, tc.ID, tc.Function.Arguments)
	}
}

// EmitAgentic inspects a complete, materialized AgenticMessage (the
// `responses` construction path) and emits the same record vocabulary as
// [TurnTap.Emit]. Content blocks are inspected independently of the message
// role, because tool results arrive in user-role messages:
//
//   - event_type=thought, source=reasoning — at most one, the non-empty
//     generic Reasoning.Text values concatenated in block order (a provider
//     summary or generic reasoning text; Signature and OpenAIExtension raw
//     reasoning are never logged)
//   - event_type=thought, source=content   — at most one, the AssistantGenText
//     values concatenated in block order, only when the message carries a
//     Function, MCP, or Server tool call
//   - event_type=decision                   — one per Function/MCP/Server tool call
//   - event_type=observation                — one per Function/MCP/Server/Tool Search result
//
// Decisions and observations follow content-block order. A final-answer-only
// message emits nothing, as do nil, unknown, and unsupported blocks (MCP
// list-tools, approvals). Callers pass only fully materialized messages: a
// failed or unmergeable stream must never reach the tap.
//
// If tt is nil or disabled, EmitAgentic returns without touching msg.
func (tt *TurnTap) EmitAgentic(msg *schema.AgenticMessage) {
	if tt == nil || tt.tap == nil || msg == nil {
		return
	}

	var reasoning, content strings.Builder
	hasToolCall := false

	for _, b := range msg.ContentBlocks {
		if b == nil {
			continue
		}
		if b.Reasoning != nil {
			reasoning.WriteString(b.Reasoning.Text)
		}
		if b.AssistantGenText != nil {
			content.WriteString(b.AssistantGenText.Text)
		}
		if b.FunctionToolCall != nil || b.MCPToolCall != nil || b.ServerToolCall != nil {
			hasToolCall = true
		}
	}

	if reasoning.Len() > 0 {
		tt.emitThought("reasoning", reasoning.String())
	}
	if hasToolCall && content.Len() > 0 {
		tt.emitThought("content", content.String())
	}

	for _, b := range msg.ContentBlocks {
		if b == nil {
			continue
		}

		switch {
		case b.FunctionToolCall != nil:
			c := b.FunctionToolCall
			tt.emitDecision(c.Name, c.CallID, c.Arguments)
		case b.MCPToolCall != nil:
			c := b.MCPToolCall
			tt.emitDecision(c.Name, c.CallID, c.Arguments)
		case b.ServerToolCall != nil:
			c := b.ServerToolCall
			tt.emitDecision(c.Name, c.CallID, renderValue(c.Arguments))
		case b.FunctionToolResult != nil:
			r := b.FunctionToolResult
			tt.emitObservation(r.Name, r.CallID, renderFunctionResult(r.Content))
		case b.MCPToolResult != nil:
			r := b.MCPToolResult
			tt.emitObservation(r.Name, r.CallID, renderMCPResult(r))
		case b.ServerToolResult != nil:
			r := b.ServerToolResult
			tt.emitObservation(r.Name, r.CallID, renderValue(r.Content))
		case b.ToolSearchFunctionToolResult != nil:
			r := b.ToolSearchFunctionToolResult
			tt.emitObservation(r.Name, r.CallID, renderValue(r.Result))
		}
	}
}

// emitThought logs one event_type=thought record.
func (tt *TurnTap) emitThought(source, content string) {
	seq := tt.nextSeq()
	tt.tap.logger.Debug().
		Str("event_type", "thought").
		Str("source", source).
		Str("session_key", tt.sessionKey).
		Str("turn_id", tt.turnID).
		Uint64("event_seq", seq).
		Str("content", content).
		Msg(reactEventMsg)
}

// emitDecision logs one event_type=decision record.
func (tt *TurnTap) emitDecision(toolName, toolCallID, arguments string) {
	seq := tt.nextSeq()
	tt.tap.logger.Debug().
		Str("event_type", "decision").
		Str("session_key", tt.sessionKey).
		Str("turn_id", tt.turnID).
		Uint64("event_seq", seq).
		Str("tool_name", toolName).
		Str("tool_call_id", toolCallID).
		Str("arguments", arguments).
		Msg(reactEventMsg)
}

// emitObservation logs one event_type=observation record.
func (tt *TurnTap) emitObservation(toolName, toolCallID, result string) {
	seq := tt.nextSeq()
	tt.tap.logger.Debug().
		Str("event_type", "observation").
		Str("session_key", tt.sessionKey).
		Str("turn_id", tt.turnID).
		Uint64("event_seq", seq).
		Str("tool_name", toolName).
		Str("tool_call_id", toolCallID).
		Str("result", result).
		Msg(reactEventMsg)
}

// unserializable replaces a value the tap cannot render, preserving the
// record's identity and sequence.
const unserializable = "<unserializable>"

// renderValue renders a structured tool value best-effort: strings pass
// through, nil is empty, anything else is JSON-encoded. An encoding error or
// panic yields [unserializable]; nothing escapes to the runner.
func renderValue(v any) (out string) {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}

	defer func() {
		if recover() != nil {
			out = unserializable
		}
	}()

	b, err := json.Marshal(v)
	if err != nil {
		return unserializable
	}

	return string(b)
}

// functionResultPart is the semantic projection of a function-result content
// part: its media fields without the per-part Extra metadata.
type functionResultPart struct {
	Type  schema.FunctionToolResultContentBlockType `json:"type"`
	Text  *schema.UserInputText                     `json:"text,omitempty"`
	Image *schema.UserInputImage                    `json:"image,omitempty"`
	Audio *schema.UserInputAudio                    `json:"audio,omitempty"`
	Video *schema.UserInputVideo                    `json:"video,omitempty"`
	File  *schema.UserInputFile                     `json:"file,omitempty"`
}

// renderFunctionResult concatenates a text-only result's parts in order; a
// result with any non-text part is JSON-encoded as its ordered semantic parts.
func renderFunctionResult(parts []*schema.FunctionToolResultContentBlock) string {
	var sb strings.Builder
	textOnly := true

	for _, p := range parts {
		if p == nil {
			continue
		}
		if p.Type != schema.FunctionToolResultContentBlockTypeText {
			textOnly = false

			break
		}
		if p.Text != nil {
			sb.WriteString(p.Text.Text)
		}
	}

	if textOnly {
		return sb.String()
	}

	projected := make([]functionResultPart, 0, len(parts))
	for _, p := range parts {
		if p == nil {
			continue
		}
		projected = append(projected, functionResultPart{
			Type: p.Type, Text: p.Text, Image: p.Image, Audio: p.Audio, Video: p.Video, File: p.File,
		})
	}

	return renderValue(projected)
}

// renderMCPResult returns an MCP result's content, or — when the server
// reported an error — a JSON object carrying both content and the error.
func renderMCPResult(r *schema.MCPToolResult) string {
	if r.Error == nil {
		return r.Content
	}

	return renderValue(struct {
		Content string                   `json:"content"`
		Error   *schema.MCPToolCallError `json:"error"`
	}{r.Content, r.Error})
}

// nextSeq atomically increments the sequence counter and returns the new value.
func (tt *TurnTap) nextSeq() uint64 {
	return tt.seq.Add(1)
}

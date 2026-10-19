package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// DefaultMaxIterations is the default limit for ReAct loop cycles.
const DefaultMaxIterations = 25

// agentName and agentDescription are shared across both agent-construction
// paths ([New]/[Agent.Rebuild] and [NewAgentic]/[AgenticAgent.Rebuild]).
const (
	agentName        = "ready"
	agentDescription = "READY. — a versatile personal assistant with tool integrations."
)

// ErrNilModel is returned when no ChatModel is provided.
var ErrNilModel = errors.New("chat model is required")

// Config holds all configuration for creating an Agent.
type Config struct {
	// ChatModel is the Eino ToolCallingChatModel to use.
	ChatModel model.ToolCallingChatModel

	// SystemPrompt is the initial system instruction for the agent.
	SystemPrompt string

	// Tools is the list of tools available to the agent (skills + MCP).
	Tools []tool.BaseTool

	// MaxIterations limits the number of ReAct cycles to prevent infinite loops.
	MaxIterations int
}

// Agent wraps the Eino ADK ChatModelAgent and Runner for processing user messages.
// The agent supports live system-prompt updates via [Rebuild]: on a skill
// hot-reload the caller recreates the internal Eino agent with the fresh
// prompt while the message loop keeps running.
type Agent struct {
	mu     sync.RWMutex
	runner *adk.Runner
	agent  *adk.ChatModelAgent
	cfg    Config // retained for Rebuild
	tap    *Tap   // debug thought-logging tap; nil-safe
}

// New creates and configures the Eino ADK agent.
func New(ctx context.Context, cfg Config, tap *Tap) (*Agent, error) {
	if cfg.ChatModel == nil {
		return nil, ErrNilModel
	}

	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	// Build ToolsConfig only if tools are provided.
	var toolsConfig adk.ToolsConfig
	if len(cfg.Tools) > 0 {
		toolsConfig.Tools = cfg.Tools
	}

	// Protocol-portable tool-call ids before Eino's tools node sees a call
	cfg.ChatModel = withToolCallIDs(cfg.ChatModel)

	chatModelAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          agentName,
		Description:   agentDescription,
		Instruction:   cfg.SystemPrompt,
		Model:         cfg.ChatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: maxIter,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create chat model agent: %w", err)
	}

	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           chatModelAgent,
		EnableStreaming: true,
	})

	return &Agent{
		runner: runner,
		agent:  chatModelAgent,
		cfg:    cfg,
		tap:    tap,
	}, nil
}

// Rebuild recreates the internal Eino agent and runner with a new system
// prompt.  All other configuration (model, tools, limits) is preserved from
// the original [Config].  This is safe to call concurrently with [Run];
// in-flight Run calls complete with the old agent while subsequent calls use
// the new one.
func (a *Agent) Rebuild(ctx context.Context, systemPrompt string) error {
	maxIter := a.cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	var toolsConfig adk.ToolsConfig
	if len(a.cfg.Tools) > 0 {
		toolsConfig.Tools = a.cfg.Tools
	}

	newAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          agentName,
		Description:   agentDescription,
		Instruction:   systemPrompt,
		Model:         a.cfg.ChatModel,
		ToolsConfig:   toolsConfig,
		MaxIterations: maxIter,
	})
	if err != nil {
		return fmt.Errorf("failed to rebuild agent: %w", err)
	}

	newRunner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent:           newAgent,
		EnableStreaming: true,
	})

	a.mu.Lock()
	a.runner = newRunner
	a.agent = newAgent
	a.cfg.SystemPrompt = systemPrompt
	a.mu.Unlock()

	return nil
}

// Run processes a single user message and returns the agent's final response text.
// channelName is retained for tracing/logging purposes; formatting is handled
// via per-channel system prompts set at construction time.
func (a *Agent) Run(ctx context.Context, channelName, userMessage string) (TurnOutcome, error) {
	return a.RunMessages(ctx, channelName, []*schema.Message{schema.UserMessage(userMessage)})
}

// RunMessages processes a conversation (prior turns + latest user message)
// and returns the agent's turn outcome.  The caller is responsible for
// terminating the slice with the new user message so the model sees the full
// session context — this is how per-session history is fed into the LLM.
//
// sessionKey is used as the session_key in thought-log records when
// logging.thoughts is enabled; it is the partitioned session identifier
// (e.g. "cli", "telegram-<chat_id>") passed by the dispatcher.
//
// This method delegates to [RunMessagesStreaming] with a nil onChunk callback,
// preserving the original non-streaming behaviour for callers that do not need
// incremental delivery.
func (a *Agent) RunMessages(ctx context.Context, sessionKey string, messages []*schema.Message) (TurnOutcome, error) {
	return a.RunMessagesStreaming(ctx, sessionKey, messages, nil)
}

// RunMessagesStreaming processes a conversation with the streaming-enabled
// runner and optionally delivers content chunks to onChunk as they arrive.
//
// When onChunk is non-nil, the method invokes it for each Content chunk of the
// final-answer model output (the last ReAct iteration whose message carries
// Content and no ToolCalls).  Intermediate ReAct iterations (those with
// ToolCalls) are consumed silently — their streams are read and closed without
// invoking onChunk.  The accumulated final content is returned in the
// outcome's Reply, identical to what RunMessages would return.
//
// When onChunk is nil, the method behaves exactly like RunMessages: the
// streaming runner is used but no chunks are forwarded, and the full response
// is returned in the outcome's Reply.
//
// sessionKey is used as the session_key in thought-log records when
// logging.thoughts is enabled.
//
// The returned TurnOutcome is populated even when the error is non-nil, so a
// failed turn still reports whether a tool call completed and whether
// final-answer content was already delivered to the caller. A turn in which
// a stream yielded partial content and then failed returns a non-nil error
// together with ContentDelivered=true and the partial text in Reply — the
// dispatcher decides what the user sees from that metadata.
func (a *Agent) RunMessagesStreaming(ctx context.Context, sessionKey string, messages []*schema.Message, onChunk func(string)) (TurnOutcome, error) {
	if len(messages) == 0 {
		return TurnOutcome{}, errors.New("agent: no messages provided")
	}

	a.mu.RLock()
	r := a.runner
	a.mu.RUnlock()

	adkMessages := make([]adk.Message, len(messages))
	copy(adkMessages, messages)

	iter := r.Run(ctx, adkMessages)

	// Start a per-turn thought tap; no-op when tap is nil or disabled.
	tt := a.tap.StartTurn(sessionKey)

	// Transcript sink: nil when the caller
	// armed none (probes, aux consumers, tests).
	rec := newTranscriptEmitter(ctx, messages)

	var finalContent string
	var lastErr error
	var toolCallStarted bool
	var toolCallCompleted bool
	var contentDelivered bool

	// Wrap onChunk so any non-empty forwarded chunk records ContentDelivered
	// on the streaming path; the non-streaming path is covered by the
	// finalContent check below.
	var forwarded func(string)
	if onChunk != nil {
		forwarded = func(chunk string) {
			if chunk != "" {
				contentDelivered = true
			}
			onChunk(chunk)
		}
	}

	for {
		event, ok := iter.Next()
		if !ok {
			break
		}

		if event.Err != nil {
			lastErr = event.Err

			continue
		}

		if event.Output != nil && event.Output.MessageOutput != nil {
			mv := event.Output.MessageOutput

			if mv.IsStreaming && mv.Role == schema.Assistant {
				// Streaming assistant event: when the thought tap is enabled,
				// copy the stream BEFORE Emit so both the tap (via GetMessage)
				// and our streaming loop can read independently.  When the tap
				// is disabled, skip the copy and read directly.
				stream := mv.MessageStream

				if tt != nil && tt.tap != nil {
					copies := mv.MessageStream.Copy(2)
					mv.MessageStream = copies[0] // left for tap (Emit → GetMessage)
					stream = copies[1]
				}

				// Consume the delivery-side copy first so first-chunk liveness
				// cannot be delayed by thought-tap materialisation of its copy.
				content, full, hasToolCalls := readStreamingAssistantFull(stream, forwarded, &lastErr, func() {
					SignalLiveness(ctx)
				})
				finalContent = content // "" for an intermediate iteration
				toolCallStarted = toolCallStarted || hasToolCalls
				// full is nil when the stream failed: a truncated tool call is
				// never recorded.
				rec.assistant(full)

				// Emit ReAct debug records after the delivery copy has observed
				// liveness. Emit reads from the copy left above.
				tt.Emit(event)
			} else {
				// Non-streaming or non-assistant event: emit first, then
				// materialise the message via GetMessage if needed.
				tt.Emit(event)

				msg, err := mv.GetMessage()
				if err != nil {
					lastErr = fmt.Errorf("tool result message error: %w", err)

					continue
				}

				if msg == nil {
					continue
				}
				if msg.Role == schema.Assistant && len(msg.ToolCalls) > 0 {
					toolCallStarted = true
					rec.assistant(msg)
				}

				// A materialised tool-role message is a completed tool call: the
				// tool ran and produced a result (the ReAct observation).
				if msg.Role == schema.Tool {
					toolCallCompleted = true
					rec.toolResult(msg)
				}

				if msg.Role == schema.Assistant && msg.Content != "" && len(msg.ToolCalls) == 0 {
					finalContent = msg.Content
				}
			}
		} else {
			// Event with no message output (e.g. action-only event).
			tt.Emit(event)
		}
	}

	// A complete final answer means the caller received content even on the
	// non-streaming path (onChunk == nil), so ContentDelivered stays accurate
	// for both delivery modes.
	if finalContent != "" {
		contentDelivered = true
	}

	outcome := TurnOutcome{
		Reply:             finalContent,
		ToolCallStarted:   toolCallStarted,
		ToolCallCompleted: toolCallCompleted,
		ContentDelivered:  contentDelivered,
	}

	// Any error in the turn fails the turn, even when partial content was
	// already delivered: the dispatcher needs to know the response is
	// incomplete so it can mark the partial output instead of presenting a
	// truncated answer as complete.
	if lastErr != nil {
		return outcome, fmt.Errorf("agent execution failed: %w", lastErr)
	}

	if finalContent == "" {
		return outcome, errors.New("agent produced no response")
	}

	return outcome, nil
}

// readStreamingAssistant reads a streaming Assistant event chunk-by-chunk,
// distinguishing intermediate ReAct iterations (ToolCalls present) from the
// final answer (Content-only).  Only Content from the final answer is forwarded
// to onChunk; intermediate iterations are consumed silently.
//
// criticalities-remediation (H6): the content of an iteration is BUFFERED and
// forwarded only when the iteration terminates without tool calls — content
// chunks can arrive before the chunk carrying the tool calls, so a per-chunk
// check cannot distinguish a final answer from pre-action text until the
// stream is exhausted. Buffered intermediate content is dropped here, so it
// can never reach a channel, set ContentDelivered, or enter history.
//
// The stream copy (when the thought tap is enabled) is handled by the caller
// before this function is invoked.  This function always closes the stream it
// receives.
//
// Returns the accumulated content if this event is the final answer, or an
// empty string if it is an intermediate iteration.  Updates *lastErr on
// stream errors.
func readStreamingAssistant(stream *schema.StreamReader[*schema.Message], onChunk func(string), lastErr *error, signalLiveness func()) (string, bool) {
	content, _, hasToolCalls := readStreamingAssistantFull(stream, onChunk, lastErr, signalLiveness)
	if hasToolCalls {
		return "", true
	}

	return content, false
}

// readStreamingAssistantFull is readStreamingAssistant plus the materialized
// message: every chunk is accumulated in the same read loop (no extra stream
// Copy) and concatenated with schema.ConcatMessages once the stream ends. The
// message is nil when the stream failed, so an assistant message whose
// stream did not complete is never recorded in the transcript.
func readStreamingAssistantFull(stream *schema.StreamReader[*schema.Message], onChunk func(string), lastErr *error, signalLiveness func()) (string, *schema.Message, bool) {
	defer stream.Close()

	var sb strings.Builder
	var hasToolCalls bool
	var buffered []string
	var chunks []*schema.Message
	var streamErr error

	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			streamErr = fmt.Errorf("stream read error: %w", recvErr)

			break
		}
		if chunk == nil {
			continue
		}
		if signalLiveness != nil {
			signalLiveness()
		}

		chunks = append(chunks, chunk)

		if len(chunk.ToolCalls) > 0 {
			hasToolCalls = true
		}

		if chunk.Content != "" && !hasToolCalls {
			sb.WriteString(chunk.Content)
			buffered = append(buffered, chunk.Content)
		}
	}

	var full *schema.Message
	if streamErr != nil {
		if lastErr != nil {
			*lastErr = streamErr
		}
	} else if len(chunks) > 0 {
		// A concat failure only costs the transcript line (the emitter then
		// also drops the group's results), never the turn itself.
		if merged, err := schema.ConcatMessages(chunks); err == nil {
			full = merged
		}
	}

	if hasToolCalls {
		return "", full, true // intermediate iteration — buffered pre-action text is dropped, never forwarded
	}

	// Final answer: forward the buffered head now that the iteration is
	// known to be the final answer (the caller's wrapper sets
	// ContentDelivered), preserving chunk order.
	if onChunk != nil {
		for _, c := range buffered {
			onChunk(c)
		}
	}

	return sb.String(), full, false
}

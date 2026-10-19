package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// AgenticConfig holds configuration for creating an [AgenticAgent] — the
// second agent-construction path, used for protocols whose eino component
// implements model.AgenticModel (currently: the OpenAI Responses API via
// eino-ext/components/model/agenticopenai, config api: responses) instead of
// model.ToolCallingChatModel.
//
// The field set intentionally mirrors [Config] so callers (cmd/serve.go)
// build whichever config applies from the same shared inputs (tools, system
// prompt, iteration/history limits).
type AgenticConfig struct {
	// Model is the Eino AgenticModel to use.
	Model model.AgenticModel

	// SystemPrompt is the initial system instruction for the agent.
	SystemPrompt string

	// Tools is the list of tools available to the agent (skills + MCP).
	Tools []tool.BaseTool

	// MaxIterations limits the number of ReAct cycles to prevent infinite loops.
	MaxIterations int
}

// AgenticAgent wraps an Eino ADK agent parameterised over
// *schema.AgenticMessage (adk.NewTypedChatModelAgent[*schema.AgenticMessage]
// / adk.NewTypedRunner[*schema.AgenticMessage]) — the construction path
// required by protocols whose eino component implements model.AgenticModel
// rather than model.ToolCallingChatModel.
//
// AgenticAgent converts to/from the plain []*schema.Message shape at its
// boundary (see toAgenticMessages / agenticMessageText in
// agentic_convert.go) so it is a drop-in [Runner] alongside [Agent]: the
// dispatcher, formatter pipeline, and checkpoint history in cmd/serve.go are
// unaware which protocol produced the response.
//
// Thought-tap logging (thoughttap.go) runs through [TurnTap.EmitAgentic] on
// each complete, materialized AgenticMessage — the same value handed to the
// transcript sink — so no stream is copied or re-read for logging.
type AgenticAgent struct {
	mu     sync.RWMutex
	runner *adk.TypedRunner[*schema.AgenticMessage]
	agent  *adk.TypedChatModelAgent[*schema.AgenticMessage]
	cfg    AgenticConfig // retained for Rebuild
	tap    *Tap          // debug thought-logging tap; nil-safe, retained across Rebuild
}

// NewAgentic creates and configures the agentic Eino ADK agent. tap may be
// nil (no thought logging), as for readiness probes.
func NewAgentic(ctx context.Context, cfg AgenticConfig, tap *Tap) (*AgenticAgent, error) {
	if cfg.Model == nil {
		return nil, ErrNilModel
	}

	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	var toolsConfig adk.ToolsConfig
	if len(cfg.Tools) > 0 {
		toolsConfig.Tools = cfg.Tools
	}

	// Transcript persistence barrier.
	cfg.Model = withAgenticTranscriptBarrier(cfg.Model)

	chatModelAgent, err := adk.NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:          agentName,
		Description:   agentDescription,
		Instruction:   cfg.SystemPrompt,
		Model:         cfg.Model,
		ToolsConfig:   toolsConfig,
		MaxIterations: maxIter,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create agentic chat model agent: %w", err)
	}

	runner := adk.NewTypedRunner[*schema.AgenticMessage](adk.TypedRunnerConfig[*schema.AgenticMessage]{
		Agent:           chatModelAgent,
		EnableStreaming: true,
	})

	return &AgenticAgent{
		runner: runner,
		agent:  chatModelAgent,
		cfg:    cfg,
		tap:    tap,
	}, nil
}

// Rebuild recreates the internal Eino agentic agent and runner with a new
// system prompt. Mirrors [Agent.Rebuild]; safe to call concurrently with
// [AgenticAgent.RunMessagesStreaming].
func (a *AgenticAgent) Rebuild(ctx context.Context, systemPrompt string) error {
	maxIter := a.cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = DefaultMaxIterations
	}

	var toolsConfig adk.ToolsConfig
	if len(a.cfg.Tools) > 0 {
		toolsConfig.Tools = a.cfg.Tools
	}

	newAgent, err := adk.NewTypedChatModelAgent[*schema.AgenticMessage](ctx, &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name:          agentName,
		Description:   agentDescription,
		Instruction:   systemPrompt,
		Model:         a.cfg.Model,
		ToolsConfig:   toolsConfig,
		MaxIterations: maxIter,
	})
	if err != nil {
		return fmt.Errorf("failed to rebuild agentic agent: %w", err)
	}

	newRunner := adk.NewTypedRunner[*schema.AgenticMessage](adk.TypedRunnerConfig[*schema.AgenticMessage]{
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

// RunMessagesStreaming implements [Runner]. It converts messages to
// []*schema.AgenticMessage, runs the agentic ReAct loop, and returns a typed
// turn outcome carrying the concatenated final-answer text — forwarding it
// incrementally to onChunk as it streams, exactly like
// [Agent.RunMessagesStreaming].
//
// ToolCallCompleted is set when any message or streamed chunk carries a
// tool-result content block (function, MCP, or server tool result) — the
// AgenticMessage analogue of a Role=Tool event on the chat path, since tool
// calls and results are embedded as ContentBlocks within assistant messages
// rather than emitted as separate Role=Tool events.
//
// sessionKey correlates the thought-tap records of this invocation; each
// complete message is emitted after the transcript sink recorded it, and
// after the delivery-side read already signalled first-chunk liveness.
func (a *AgenticAgent) RunMessagesStreaming(ctx context.Context, sessionKey string, messages []*schema.Message, onChunk func(string)) (TurnOutcome, error) {
	if len(messages) == 0 {
		return TurnOutcome{}, errors.New("agent: no messages provided")
	}

	a.mu.RLock()
	r := a.runner
	a.mu.RUnlock()

	agenticMessages := toAgenticMessages(messages)

	iter := r.Run(ctx, agenticMessages)

	// Transcript sink: nil when unarmed.
	rec := newTranscriptEmitter(ctx, messages)

	// Per-invocation thought tap; no-op when tap is nil or disabled.
	tt := a.tap.StartTurn(sessionKey)

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

		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}

		mv := event.Output.MessageOutput

		if mv.IsStreaming {
			content, full, hasToolCalls, toolResult := readAgenticStreamFull(mv.MessageStream, forwarded, &lastErr, func() {
				SignalLiveness(ctx)
			})
			recordAgentic(rec, full)
			tt.EmitAgentic(full) // nil when the stream failed or could not be merged
			if hasToolCalls {
				toolCallStarted = true
			}
			if toolResult {
				toolCallCompleted = true
			}
			if !hasToolCalls && content != "" {
				finalContent = content
			}

			continue
		}

		if agenticMessageHasToolResult(mv.Message) {
			toolCallCompleted = true
		}
		recordAgentic(rec, mv.Message)
		tt.EmitAgentic(mv.Message)

		text, hasToolCalls := agenticMessageText(mv.Message)
		if hasToolCalls {
			toolCallStarted = true
		}
		if !hasToolCalls && text != "" {
			finalContent = text
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
	// already delivered — same contract as [Agent.RunMessagesStreaming].
	if lastErr != nil {
		return outcome, fmt.Errorf("agent execution failed: %w", lastErr)
	}

	if finalContent == "" {
		return outcome, errors.New("agent produced no response")
	}

	return outcome, nil
}

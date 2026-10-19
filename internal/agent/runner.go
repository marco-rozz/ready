package agent

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// TurnOutcome reports what a turn actually produced — including failed
// turns. The dispatcher uses the effect booleans to select the terminal
// failure message a user sees when a turn fails (llm-outage-delivery), and
// llm-failover-orchestrator reuses them as its retry-eligibility gate: a
// turn is retryable only before any tool call starts and before final-answer
// content is delivered, because either effect cannot safely be taken back.
type TurnOutcome struct {
	// Reply is the final-answer text produced by the turn. On a failed turn
	// it carries whatever partial final-answer content was accumulated before
	// the failure (possibly empty); the streamed partial is usually already
	// visible to the user via onChunk.
	Reply string

	// ToolCallStarted is true as soon as the model emits a tool call. It is a
	// conservative retry gate: the tool may have changed the world even when
	// its result was lost before ToolCallCompleted could be observed.
	ToolCallStarted bool

	// ToolCallCompleted is true when at least one tool call in the turn
	// produced a result — i.e. a tool actually ran and may have changed the
	// world (a light switched on, a plan step executed). A tool call that was
	// requested but never answered does not count.
	ToolCallCompleted bool

	// ContentDelivered is true when final-answer content reached the caller:
	// the streaming callback was invoked at least once with non-empty
	// content, or a complete final answer was returned on the non-streaming
	// path (onChunk == nil).
	ContentDelivered bool
}

// Runner is the narrow contract cmd/serve.go's dispatcher depends on: a
// single per-turn entry point that accepts the full conversation (prior
// turns restored from the checkpoint store plus the latest user message,
// with any per-turn system messages already prepended by the caller) and
// returns a typed turn outcome carrying the model's final response text plus
// what the turn actually did, optionally streaming content chunks to onChunk
// as they arrive. The outcome is populated on failed turns too, so the
// dispatcher can distinguish "nothing ran" from "a tool ran or content was
// already delivered" without parsing the error.
//
// Both [Agent] (the model.ToolCallingChatModel / adk.NewChatModelAgent
// construction path, used by chat_completions/anthropic/google/ollama/
// openrouter) and [AgenticAgent] (the model.AgenticModel /
// adk.NewTypedChatModelAgent[*schema.AgenticMessage] path, used by the
// `responses` protocol) implement Runner, so cmd/serve.go's agentRegistry
// holds one interface value regardless of which protocol resolved at boot —
// the formatter pipeline, checkpoint history, and dispatcher code are
// unaware which construction path is active.
type Runner interface {
	RunMessagesStreaming(ctx context.Context, sessionKey string, messages []*schema.Message, onChunk func(string)) (TurnOutcome, error)
}

// Compile-time interface assertions.
var (
	_ Runner = (*Agent)(nil)
	_ Runner = (*AgenticAgent)(nil)
)

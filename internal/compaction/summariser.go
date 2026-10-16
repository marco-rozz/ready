package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// ErrEmptySummary is returned when the summariser answers with no text.
var ErrEmptySummary = errors.New("compaction summariser returned an empty summary")

// fallbackPrompt is the compiled-in generic summary format used when the
// Compaction skill cannot be loaded (one WARN per boot).
const fallbackPrompt = `You compact the conversation history of a home-automation assistant.
Write a concise, factual summary that lets the assistant continue the conversation without the original messages.
Use these sections, omitting empty ones:
## User requests
## Actions performed (tool, entity ids, outcome)
## Pending items
## Preferences and constraints
## Resolved references (names the user used → entity ids)
<entities-touched>comma-separated entity ids</entities-touched>
Every reported entity state is historical: the assistant must re-read current state with ha_state_query before acting on it.
Do not restate skills marked as preserved verbatim; record only what was done with them.
When a previous summary is given, merge it with the new conversation into one updated summary.
Answer with the summary only.`

// Request is one summarisation.
type Request struct {
	// PreviousSummary is the latest compaction's summary ("" when none):
	// summaries are iterative.
	PreviousSummary string
	// Conversation is the Serialise output of the entries to summarise.
	Conversation string
	// Instructions are the optional /compact free-text instructions.
	Instructions string
}

// Summariser produces a summary with one call on the auxiliary model chain,
// bounded by Timeout. The ctx must carry the Session's usage accumulator:
// the call is counted as an auxiliary call.
type Summariser struct {
	model   model.BaseChatModel
	prompt  func(ctx context.Context) (string, error)
	timeout time.Duration
	logger  zerolog.Logger

	fallbackOnce sync.Once
}

// NewSummariser wires the summariser. prompt loads the Compaction skill's
// SKILL.md (through the skill cache, so hot edits apply).
func NewSummariser(m model.BaseChatModel, prompt func(ctx context.Context) (string, error), timeout time.Duration, logger zerolog.Logger) *Summariser {
	return &Summariser{model: m, prompt: prompt, timeout: timeout, logger: logger}
}

// SystemPrompt returns the Compaction skill's SKILL.md, or the compiled
// fallback (logged once per boot) when it cannot be loaded.
func (s *Summariser) SystemPrompt(ctx context.Context) string {
	if s.prompt != nil {
		if p, err := s.prompt(ctx); err == nil && strings.TrimSpace(p) != "" {
			return p
		} else if err != nil {
			s.fallbackOnce.Do(func() {
				s.logger.Warn().Err(err).Str("consumer", "compaction").Msg("compaction skill unavailable — using the compiled generic summary format")
			})
		}
	}

	return fallbackPrompt
}

// UserMessage renders the summariser's user message.
func UserMessage(req Request) string {
	var b strings.Builder

	if req.PreviousSummary != "" {
		b.WriteString("<previous_summary>\n")
		b.WriteString(req.PreviousSummary)
		b.WriteString("\n</previous_summary>\n\n")
	}

	b.WriteString("<conversation>\n")
	b.WriteString(req.Conversation)
	b.WriteString("\n</conversation>\n\n")

	if req.Instructions != "" {
		b.WriteString("<instructions>\n")
		b.WriteString(req.Instructions)
		b.WriteString("\n</instructions>\n\n")
	}

	if req.PreviousSummary != "" {
		b.WriteString("Write the updated summary, merging the previous summary with the conversation above.")
	} else {
		b.WriteString("Write the summary of the conversation above.")
	}

	return b.String()
}

// Summarise runs the summariser call.
func (s *Summariser) Summarise(ctx context.Context, req Request) (string, error) {
	if s.model == nil {
		return "", errors.New("compaction summariser has no model")
	}

	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	out, err := s.model.Generate(callCtx, []*schema.Message{
		schema.SystemMessage(s.SystemPrompt(callCtx)),
		schema.UserMessage(UserMessage(req)),
	})
	if err != nil {
		return "", fmt.Errorf("compaction summariser failed: %w", err)
	}

	if out == nil || strings.TrimSpace(out.Content) == "" {
		return "", ErrEmptySummary
	}

	return strings.TrimSpace(out.Content), nil
}

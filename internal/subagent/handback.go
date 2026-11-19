// handback.go — the handback builder from a TurnOutcome (openspec change
// subagent-runtime, task 4.5, design D6): status mapping, the fixed notes,
// the error severity class, chain exhaustion as shared LLM state, and the
// token cap.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	adk "github.com/cloudwego/eino/adk"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
)

// Fixed harness-note strings (English product strings; delivered inside the
// handback envelope, never free-floating).
const (
	NoteIterationLimit = "The Subagent reached its iteration limit before producing a final answer; the text below is its last work in progress."
	NoteNothing        = "The Subagent produced no response (neither content nor a tool call)."
	NoteInterrupted    = "The Subagent's dispatch was interrupted before it could finish; its partial work is not available."
	NoteDeadline       = "The Subagent's dispatch reached its time budget before finishing; its partial work is not available. Ask again to retry the task."
	NoteFailed         = "The Subagent's dispatch failed."
	NoteChainExhausted = "The LLM candidate chain was exhausted (every model unhealthy or unavailable); the dispatch could not run. This is shared LLM health, not a Subagent problem."
)

// errExceedMaxIterations is the vendored ADK's iteration-limit sentinel
// (spike task 0.2: detectable via errors.Is through the compose wrapping).
var errExceedMaxIterations = adk.ErrExceedMaxIterations

// buildHandback maps one dispatch's outcome to a Handback. severity maps a
// failure to the error-digest class (the daemon classifier; "" when
// unclassified).
func buildHandback(sub *Agent, dispatchID string, outcome agent.TurnOutcome, runErr error, _ time.Time, usage UsageSnapshot, maxTokens int, severity func(error) string) Handback {
	hb := Handback{
		HandbackID: newHandbackID(),
		DispatchID: dispatchID,
		SubagentID: sub.ID,
		Name:       sub.Name,
		CreatedAt:  time.Now(),
		Usage:      usage,
	}

	capReport := func(text string) string { return capTokens(text, maxTokens) }

	switch {
	case runErr != nil && errors.Is(runErr, context.Canceled):
		hb.Status = HandbackInterrupted
		hb.Report = capReport(NoteInterrupted)
	case runErr != nil && errors.Is(runErr, context.DeadlineExceeded):
		hb.Status = HandbackInterrupted
		hb.Report = capReport(NoteDeadline)
	case runErr != nil && errors.Is(runErr, llm.ErrChainExhausted):
		hb.Status = HandbackFailed
		hb.Report = capReport(NoteChainExhausted)
	case runErr != nil && errors.Is(runErr, errExceedMaxIterations):
		hb.Status = HandbackIterationLimit
		hb.Report = capReport(NoteIterationLimit + "\n" + strings.TrimSpace(outcome.Reply))
	case runErr != nil:
		hb.Status = HandbackFailed
		if severity != nil {
			hb.Severity = severity(runErr)
		}

		hb.Report = capReport(NoteFailed + " Error: " + oneLine(runErr))
	case outcome.Reply == "" && !outcome.ToolCallCompleted:
		// An empty completion: neither content nor a tool call.
		hb.Status = HandbackNothingDelivered
		hb.Report = capReport(NoteNothing)
	case outcome.Reply == "":
		// A tool-only dispatch that never produced a final answer within
		// its budget has the iteration-limit shape.
		hb.Status = HandbackIterationLimit
		hb.Report = capReport(NoteIterationLimit)
	default:
		hb.Status = HandbackCompleted
		hb.Report = capReport(strings.TrimSpace(outcome.Reply))
	}

	return hb
}

// oneLine renders an error for the report (single line).
func oneLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", " ")
}

// capTokens truncates text to maxTokens (estimated chars ÷ 4) with a fixed
// note — the handback_max_tokens cap (D6).
func capTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}

	runes := []rune(text)
	limit := maxTokens * 4
	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit]) + fmt.Sprintf("… [truncated: %d more characters]", len(runes)-limit)
}

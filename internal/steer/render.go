package steer

import (
	"fmt"
	"time"

	"github.com/marco-rozz/ready/internal/injection"
)

// steerClockLayout renders the admission time as local HH:MM:SS.
const steerClockLayout = "15:04:05"

// RenderSteer renders the stored content of a steer record (turn-steering
// S9): a nonce-protected harness envelope carrying the local admission time
// and the user's text verbatim. nonce is the Turn's formatter nonce, so tool
// results or Subagent reports cannot forge the closing tag. The user's text
// is never scanned or altered.
func RenderSteer(nonce string, receivedAt time.Time, text string) string {
	tag := fmt.Sprintf("%s_%s", injection.TagUserSteer, nonce)

	return fmt.Sprintf(
		"<%s>\nThe user sent this message at %s while you were working on the current request. "+
			"It may correct, extend or replace that request: take it into account before continuing.\n\n%s\n</%s>",
		tag, receivedAt.Local().Format(steerClockLayout), text, tag,
	)
}

// Fixed English results of the interruptible waits (turn-steering S12).
const (
	// WaitInterruptedSteer ends a `wait` interrupted by a steering candidate.
	WaitInterruptedSteer = "Interrupted by a new user message; the remaining wait was skipped."
	// WaitInterruptedStop ends a `wait` interrupted by /stop.
	WaitInterruptedStop = "Interrupted: the user stopped the request; the remaining wait was skipped."
	// SubagentWaitInterruptedSteer ends a `subagent_wait` interrupted by a
	// steering candidate; pending results stay pending.
	SubagentWaitInterruptedSteer = "Interrupted by a new user message; subagent results stay pending."
	// SubagentWaitInterruptedStop ends a `subagent_wait` interrupted by /stop.
	SubagentWaitInterruptedStop = "Interrupted: the user stopped the request; subagent results stay pending."
)

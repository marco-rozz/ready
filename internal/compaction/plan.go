// Package compaction implements append-only context compaction (the architecture decision records,
// the session-lifecycle the design doc): the cut-point planner, the
// plain-text serialiser of the entries to summarise, the summariser call on
// the auxiliary model chain, the trigger arithmetic with its failure
// backoff, and the summary message carrying the pinned skills. The Session
// file is never rewritten: a Compaction appends one `compaction` entry and
// the model then sees the summary followed by the verbatim tail.
package compaction

import (
	"github.com/marco-rozz/ready/internal/persistence"
)

// Tokens converts an estimated character count to tokens (chars ÷ 4, the
// same heuristic as the /context breakdown).
func Tokens(chars int) int { return chars / 4 }

// PlanInput describes the view to compact and the budget.
type PlanInput struct {
	// Entries is the current view (the previous compaction's kept tail plus
	// newer entries — so a new compaction is iterative from the previous
	// first_kept). Every entry must be saved.
	Entries []persistence.HistoryMessage
	// Lines are the entries' physical line indices in the Session file.
	Lines []int
	// TurnMode keeps the last KeepRecentTurns Turns (no context_window is
	// declared); otherwise KeepRecentTokens are kept, walking back.
	TurnMode         bool
	KeepRecentTokens int
	KeepRecentTurns  int
	// MaxInputTokens bounds the summariser input (0 = unbounded: turn mode,
	// no window known). OverheadTokens is the fixed part of that input
	// (skill prompt, previous summary, instructions).
	MaxInputTokens int
	OverheadTokens int
}

// Plan is a chosen cut.
type Plan struct {
	// Cut is the index in Entries of the first kept entry.
	Cut int
	// FirstKept is the physical line of the first kept entry.
	FirstKept int
	// ToSummarise is Entries[:Cut].
	ToSummarise []persistence.HistoryMessage
}

// isBoundary reports whether entries[i] opens a Turn.
func isBoundary(entries []persistence.HistoryMessage, i int) bool {
	return entries[i].IsTurnBoundary()
}

// isValidCut reports whether the kept tail may start at i: a Turn boundary
// or an assistant message (a split oversized Turn). Never a tool result (a
// call would be separated from its result) and never a turn-context line
// (it belongs with its user message).
func isValidCut(entries []persistence.HistoryMessage, lines []int, i int) bool {
	if i <= 0 || i >= len(entries) || lines[i] < 0 {
		return false
	}

	return isBoundary(entries, i) || entries[i].Role == persistence.RoleAssistant
}

// PlanCut chooses the first kept entry. ok is false when nothing can be
// summarised (the whole view fits the kept budget, or no valid cut fits the
// input guard).
func PlanCut(in PlanInput) (Plan, bool) {
	var cut int

	var ok bool
	if in.TurnMode {
		cut, ok = turnCut(in)
	} else {
		cut, ok = tokenCut(in)
	}

	if !ok {
		return Plan{}, false
	}

	if in.MaxInputTokens > 0 {
		if cut, ok = guardInput(in, cut); !ok {
			return Plan{}, false
		}
	}

	return Plan{Cut: cut, FirstKept: in.Lines[cut], ToSummarise: in.Entries[:cut]}, true
}

// turnCut keeps the last KeepRecentTurns Turns.
func turnCut(in PlanInput) (int, bool) {
	var bounds []int
	for i := range in.Entries {
		if isBoundary(in.Entries, i) {
			bounds = append(bounds, i)
		}
	}

	keep := max(in.KeepRecentTurns, 1)
	if len(bounds) <= keep {
		return 0, false
	}

	cut := bounds[len(bounds)-keep]

	return cut, isValidCut(in.Entries, in.Lines, cut)
}

// tokenCut walks back from the newest entry accumulating estimated tokens
// until KeepRecentTokens is reached (the kept tail is at least the budget),
// then settles on the nearest valid cut at or before that edge: preferably a
// Turn boundary (whole Turns kept); when the edge falls inside the view's
// first Turn, an assistant message inside that oversized Turn (a split
// Turn); failing both, the nearest valid cut after the edge.
func tokenCut(in PlanInput) (int, bool) {
	candidate := -1
	acc := 0

	for i := len(in.Entries) - 1; i >= 0; i-- {
		acc += Tokens(in.Entries[i].EstimatedChars())
		if acc >= in.KeepRecentTokens {
			candidate = i

			break
		}
	}

	if candidate < 0 {
		return 0, false // the whole view fits the kept budget
	}

	for i := candidate; i > 0; i-- {
		if isBoundary(in.Entries, i) && isValidCut(in.Entries, in.Lines, i) {
			return i, true
		}
	}

	for i := candidate; i > 0; i-- {
		if isValidCut(in.Entries, in.Lines, i) {
			return i, true
		}
	}

	for i := candidate + 1; i < len(in.Entries); i++ {
		if isValidCut(in.Entries, in.Lines, i) {
			return i, true
		}
	}

	return 0, false
}

// guardInput shrinks the summarised slice to the oldest one that fits
// MaxInputTokens (the design doc input guard): the rest stays verbatim after the
// new first kept entry, and a later evaluation may compact again.
func guardInput(in PlanInput, cut int) (int, bool) {
	fits := func(end int) bool {
		chars := 0
		for _, part := range serialiseParts(in.Entries[:end]) {
			chars += len(part) + len(partSeparator)
		}

		return in.OverheadTokens+Tokens(chars) <= in.MaxInputTokens
	}

	if fits(cut) {
		return cut, true
	}

	for i := cut - 1; i > 0; i-- {
		if isBoundary(in.Entries, i) && isValidCut(in.Entries, in.Lines, i) && fits(i) {
			return i, true
		}
	}

	for i := cut - 1; i > 0; i-- {
		if isValidCut(in.Entries, in.Lines, i) && fits(i) {
			return i, true
		}
	}

	return 0, false
}

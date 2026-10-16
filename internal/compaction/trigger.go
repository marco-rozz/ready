package compaction

import (
	"fmt"
	"html"
	"strings"
	"sync"
)

// Trigger is the automatic-compaction arithmetic.
type Trigger struct {
	// Window is the smallest context_window declared in the LLM chain
	// (0 = none declared: turn-count mode).
	Window int
	// Reserve is the room left for the next Turn (reserve_tokens).
	Reserve int
	// MaxTurns is the turn-count trigger used without a window.
	MaxTurns int
}

// TurnMode reports whether no window is declared.
func (t Trigger) TurnMode() bool { return t.Window <= 0 }

// Threshold is the prompt size above which a compaction is due (window −
// reserve); 0 in turn mode.
func (t Trigger) Threshold() int {
	if t.TurnMode() {
		return 0
	}

	return max(t.Window-t.Reserve, 1)
}

// Compared is the value checked against the threshold: the larger of the
// last main-agent call's measured prompt tokens plus the entries appended
// since (only when the measurement was taken on the current view), and the
// whole-prompt estimate. The max keeps the trigger safe against a provider
// that under-reports prompt tokens.
func Compared(measured int, measuredValid bool, appendedTokens, estimate int) int {
	if !measuredValid || measured <= 0 {
		return estimate
	}

	return max(measured+appendedTokens, estimate)
}

// Due reports whether a compaction is due: compared above the threshold, or
// at least MaxTurns Turns since the last compaction in turn mode.
func (t Trigger) Due(compared, turnsSinceCompaction int) bool {
	if t.TurnMode() {
		return t.MaxTurns > 0 && turnsSinceCompaction >= t.MaxTurns
	}

	return compared > t.Threshold()
}

// maxSkip caps the failure backoff.
const maxSkip = 8

// Backoff tracks consecutive automatic-compaction failures of one Session:
// after the k-th consecutive failure the next min(2^(k-1), 8) evaluations
// are skipped; any success (manual /compact included) resets it.
type Backoff struct {
	mu       sync.Mutex
	failures int
	skip     int
}

// Skip consumes one skipped evaluation, reporting whether this evaluation
// must be skipped.
func (b *Backoff) Skip() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.skip > 0 {
		b.skip--

		return true
	}

	return false
}

// Failed records a failure.
func (b *Backoff) Failed() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	b.skip = min(1<<(b.failures-1), maxSkip)
}

// Succeeded resets the backoff.
func (b *Backoff) Succeeded() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures, b.skip = 0, 0
}

// SkillBlock is one pinned skill's name and current SKILL.md body.
type SkillBlock struct {
	Name string
	Body string
}

// LoadedSkillsBlock renders the pinned skills block ("" when none).
func LoadedSkillsBlock(skills []SkillBlock) string {
	if len(skills) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<loaded_skills>\n")

	for _, s := range skills {
		fmt.Fprintf(&b, "<skill name=%q>\n%s\n</skill>\n", html.EscapeString(s.Name), s.Body)
	}

	b.WriteString("</loaded_skills>")

	return b.String()
}

// SummaryMessage renders the single `user` message the model sees right
// after the stable prefix: the summary in <summary>…</summary>
// followed by the pinned skills block. Either part may be empty (a failure
// projection carries only the skills block); "" when both are.
func SummaryMessage(summary string, skills []SkillBlock) string {
	var parts []string
	if summary != "" {
		parts = append(parts, "<summary>\n"+summary+"\n</summary>")
	}

	if block := LoadedSkillsBlock(skills); block != "" {
		parts = append(parts, block)
	}

	return strings.Join(parts, "\n\n")
}

// PreservedSkills returns the skills to pin, oldest-loaded first: the Active
// skills minus the excluded ones (bootstrap, formatter, Compaction skill),
// ordered by previously pinned order then first-load order (skills with no
// known load come first — they were pinned before).
func PreservedSkills(active []string, exclude map[string]bool, previous, loadOrder []string) []string {
	isActive := map[string]bool{}
	for _, a := range active {
		if !exclude[a] {
			isActive[a] = true
		}
	}

	seen := map[string]bool{}

	var out []string

	add := func(names []string) {
		for _, n := range names {
			if isActive[n] && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}

	add(previous)
	add(loadOrder)
	add(active) // Active without a recorded load (e.g. restored): keep them too

	return out
}

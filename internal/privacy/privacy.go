// Package privacy provides a deterministic guardrail for redacting known
// absolute filesystem path prefixes from text that crosses into or out of the
// LLM (system prompt, tool results, final response).
//
// Unlike a heuristic "looks-like-a-path" matcher, the Sanitizer only ever
// rewrites the exact prefixes it was configured with at boot. This means it
// cannot produce false positives on strings that merely resemble paths — e.g.
// MCP tool names such as "light/turn_on", URL paths, or dates — which is the
// failure mode that made the previous regex-based approach unusable.
package privacy

import (
	"os"
	"sort"
	"strings"
)

// Sanitizer redacts configured absolute path prefixes from text, replacing
// each with its placeholder. The zero value (and a nil pointer) is a safe
// no-op, so callers may pass a nil *Sanitizer when no prefixes are known.
type Sanitizer struct {
	// rules are kept sorted by descending prefix length so that nested
	// prefixes redact to the most specific placeholder.
	rules []rule
}

type rule struct {
	from string
	to   string
}

// New builds a Sanitizer from the given prefix→placeholder pairs. Empty "from"
// keys are ignored. The returned Sanitizer is immutable and safe for
// concurrent use.
func New(pairs map[string]string) *Sanitizer {
	s := &Sanitizer{}
	for from, to := range pairs {
		if from == "" {
			continue
		}
		s.rules = append(s.rules, rule{from: from, to: to})
	}

	// Longest prefix first: e.g. redact "/home/u/app/.ready" before "/home/u".
	sort.Slice(s.rules, func(i, j int) bool {
		return len(s.rules[i].from) > len(s.rules[j].from)
	})

	return s
}

// FromHome returns a Sanitizer mapping the current user's home directory to
// "~", plus any additional prefixes supplied. If the home directory cannot be
// resolved it is simply omitted; the extra prefixes are still applied. The
// result is never nil.
//
// The extra prefixes must carry every runtime-home path the run can expose
// (the elected config's directory — sessions, logs, skills) so the deployed
// directory name is redacted too, not only the user home. There is no
// hard-coded runtime-home literal to strip: it resolves per run.
func FromHome(extra map[string]string) *Sanitizer {
	pairs := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		pairs[k] = v
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// Don't clobber a more specific extra prefix that equals home.
		if _, ok := pairs[home]; !ok {
			pairs[home] = "~"
		}
	}

	return New(pairs)
}

// FromRun builds a Sanitizer for one daemon run: the user home plus the
// resolved runtime home (the elected config's directory, when non-empty and
// outside the user home) mapped to a neutral placeholder. Longest prefix
// wins, so a runtime home nested under the user home is redacted as itself.
func FromRun(runtimeHome string, extra map[string]string) *Sanitizer {
	pairs := make(map[string]string, len(extra)+2)
	for k, v := range extra {
		pairs[k] = v
	}

	if runtimeHome != "" {
		if _, ok := pairs[runtimeHome]; !ok {
			pairs[runtimeHome] = "<runtime home>"
		}
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// Don't clobber a more specific extra prefix that equals home.
		if _, ok := pairs[home]; !ok {
			pairs[home] = "~"
		}
	}

	return New(pairs)
}

// Redact replaces every configured prefix occurrence in text with its
// placeholder. A nil Sanitizer or empty input returns the text unchanged.
func (s *Sanitizer) Redact(text string) string {
	if s == nil || text == "" {
		return text
	}

	for _, r := range s.rules {
		text = strings.ReplaceAll(text, r.from, r.to)
	}

	return text
}

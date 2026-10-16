package compaction

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marco-rozz/ready/internal/persistence"
)

// MaxToolResultChars bounds every serialised tool result.
const MaxToolResultChars = 2000

// SkillsLoadTool is the name of the tool whose results carry a skill body;
// those results are replaced by a marker (the skill is pinned verbatim in
// the summary message instead — the design doc).
const SkillsLoadTool = "skills-load"

// partSeparator joins serialised entries.
const partSeparator = "\n\n"

// Serialise renders entries in the plain-text form the summariser reads:
// `[User]:`, `[Assistant]:`, `[Assistant tool calls]:`, `[Tool result]:`.
// Turn-context lines are omitted (per-turn directives, not conversation);
// tool results are truncated to MaxToolResultChars with a marker; a
// skills-load result becomes a marker naming the skill.
func Serialise(entries []persistence.HistoryMessage) string {
	return strings.Join(serialiseParts(entries), partSeparator)
}

// serialiseParts renders one part per serialised entry.
func serialiseParts(entries []persistence.HistoryMessage) []string {
	calls := map[string]persistence.ToolCallRecord{}
	parts := make([]string, 0, len(entries))

	for _, e := range entries {
		switch {
		case e.IsRecord(), e.Kind == persistence.KindTurnContext:
			continue
		case e.Role == persistence.RoleUser:
			parts = append(parts, "[User]: "+e.Content)
		case e.Role == persistence.RoleSystem:
			parts = append(parts, "[System]: "+e.Content)
		case e.Role == persistence.RoleAssistant:
			if e.Content != "" {
				parts = append(parts, "[Assistant]: "+e.Content)
			}

			if len(e.ToolCalls) > 0 {
				rendered := make([]string, 0, len(e.ToolCalls))
				for _, tc := range e.ToolCalls {
					calls[tc.ID] = tc
					rendered = append(rendered, tc.Name+"("+tc.Arguments+")")
				}

				parts = append(parts, "[Assistant tool calls]: "+strings.Join(rendered, "; "))
			}
		case e.Role == persistence.RoleTool:
			parts = append(parts, "[Tool result]: "+toolResultText(calls[e.ToolCallID], e.Content))
		}
	}

	return parts
}

// toolResultText renders one tool result for the summariser.
func toolResultText(tc persistence.ToolCallRecord, content string) string {
	if tc.Name == SkillsLoadTool {
		if name := SkillNameFromArgs(tc.Arguments); name != "" {
			return fmt.Sprintf("skill %q loaded; preserved verbatim, do not summarise", name)
		}
	}

	runes := []rune(content)
	if len(runes) <= MaxToolResultChars {
		return content
	}

	return string(runes[:MaxToolResultChars]) + fmt.Sprintf("… [truncated: %d more characters]", len(runes)-MaxToolResultChars)
}

// SkillNameFromArgs extracts the skill name of a skills-load call.
func SkillNameFromArgs(args string) string {
	var in struct {
		SkillName string `json:"skill_name"`
	}

	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return ""
	}

	return in.SkillName
}

// LoadOrder returns the skills loaded by skills-load calls in entries, in
// first-load order.
func LoadOrder(entries []persistence.HistoryMessage) []string {
	seen := map[string]bool{}

	var out []string

	for _, e := range entries {
		for _, tc := range e.ToolCalls {
			if tc.Name != SkillsLoadTool {
				continue
			}

			if name := SkillNameFromArgs(tc.Arguments); name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}

	return out
}

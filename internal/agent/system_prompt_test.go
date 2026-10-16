package agent

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_CapabilityBoundary pins the capability-boundary
// rule (2026-10-05 incident: glm-5.3-flash deliberated for ~90s per attempt
// about a tool name it remembered from restored history but could not find
// in its schema, then closed three attempts with an empty reply instead of
// telling the user the operation was impossible). The rule must instruct the
// model to treat the current schema as the only truth and to decline
// immediately when no tool covers the request.
func TestBuildSystemPrompt_CapabilityBoundary(t *testing.T) {
	prompt := BuildSystemPrompt()

	for _, want := range []string{
		"## Capability boundary",
		"ONLY truth",
		"no longer exist",
		"IMMEDIATELY",
		"cannot perform",
		"Do NOT hunt for indirect",
		"empty reply",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt missing capability-boundary fragment %q", want)
		}
	}
}

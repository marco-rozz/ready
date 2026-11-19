// prompt.go — the child's static prompt prefix (openspec change
// subagent-runtime, task 4.1/4.2, design D19): harness preamble, definition
// body, skills metadata catalog, the owner path's bootstrap skills and the
// definition's skills preloaded. Static per child Session (cache-stable);
// the current time travels in the task/mail record's harness header, not in
// a turn context.
package subagent

import (
	"context"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// childPrefix builds the static system-message prefix of a child Session.
func (r *Runtime) childPrefix(ctx context.Context, skillsSID string, def Definition, env GenEnv) []*schema.Message {
	var b strings.Builder

	b.WriteString(childPreamble)
	b.WriteString("\n\n## Your definition\n\n")
	b.WriteString(strings.TrimSpace(def.Body))

	// The skills metadata catalog (progressive disclosure).
	if skillsSID != "" {
		if meta, err := r.deps.Skills.MetadataPrompt(ctx, skillsSID); err == nil && meta != "" {
			b.WriteString("\n\n## Available skills\n\n")
			b.WriteString(meta)
		}
	}

	// The definition's skills are PRELOADED: their bodies are in the prompt
	// (not just the catalog) — D19.
	preloaded := append([]string(nil), def.Skills...)
	preloaded = append(preloaded, env.BootstrapSkills...)

	if len(preloaded) > 0 {
		var bodies strings.Builder
		seen := map[string]bool{}

		for _, name := range preloaded {
			if name == "" || seen[name] {
				continue
			}

			seen[name] = true

			if body, ok := r.deps.Skills.SkillBody(ctx, name); ok {
				bodies.WriteString("\n\n### Skill: " + name + "\n\n")
				bodies.WriteString(body)
			}
		}

		if bodies.Len() > 0 {
			b.WriteString("\n\n## Preloaded skills (already in context — do NOT call skills-load for these)\n")
			b.WriteString(bodies.String())
		}
	}

	// The child's prompt carries skill bodies — redact the operator home the
	// same way every system prompt section is redacted.
	text := r.deps.Redact(b.String())

	return []*schema.Message{schema.SystemMessage(text)}
}

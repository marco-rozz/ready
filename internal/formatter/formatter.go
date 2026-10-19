// Package formatter applies a formatter skill to agent output.
//
// Two operating modes are supported, selected per-channel via
// [config.CLIChannelConfig.FormatterPostStep] (and the equivalent field on
// every other channel config):
//
//   - formatter_post_step: true  — [Formatter.Format] is called after the
//     agent produces its Final Answer.  The skill's SKILL.md becomes the LLM
//     system prompt and the agent response is passed as the user message.
//
//   - formatter_post_step: false (default) — inline reinforcement mode.
//     [Formatter.LoadSkillPrompt] returns the skill's SKILL.md content so the
//     caller can inject it into the initial system prompt.  In the daemon
//     (cmd/serve.go) this happens with three coordinated reinforcements:
//     1. The SKILL.md body is wrapped in a static
//     <formatter_skill>...</formatter_skill> tag at prompt position 1.
//     2. The current-turn user message is passed through verbatim — NOT
//     rewritten. The final reminder and formatting directive instead
//     live in the separate per-turn context message built by
//     [BuildTurnContext], tagged with a fresh 4-byte random ID from
//     [NewTurnID] so untrusted content cannot forge a closing tag. This
//     keeps the assembled user-message bytes identical to what is
//     persisted, which is required for the KV-cache warm-start prefix
//     to survive across turns.
//     3. The formatter skill is marked Active in the per-session
//     agentskills session so the progressive-disclosure metadata
//     catalog does not list it (no duplicate signal to the LLM).
//     No separate LLM call occurs; the model produces a channel-aware
//     response in a single pass.  Persisted history (the legacy checkpoints directory of the pre-.ready era)
//     remains raw — the turn-context message is a prompt-shape concern only.
//
// If formatter_skill is empty or absent both methods are no-ops.
package formatter

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// PathResolver resolves a skill name to the absolute path of its SKILL.md file.
// [skills.Manager] satisfies this interface.
type PathResolver interface {
	SkillFilePath(skillName string) (string, error)
}

// ContentLoader loads file content by absolute path, typically with caching.
// [skillcache.SkillCache] satisfies this interface.
type ContentLoader interface {
	Load(ctx context.Context, path string) (string, error)
}

// Formatter applies a formatter skill to a text response.
type Formatter struct {
	chatModel model.BaseChatModel
	resolver  PathResolver
	cache     ContentLoader
}

// New creates a Formatter backed by the given chat model, skill-path resolver,
// and content-loader cache.  All three parameters are required.
//
// chatModel is typed model.BaseChatModel (Generate/Stream only, no tool
// binding) because Format never needs tool calling — this lets callers hand
// in either a model.ToolCallingChatModel or an [internal/agent.AsBaseChatModel]
// adapter over a model.AgenticModel (the `responses` protocol) uniformly.
func New(chatModel model.BaseChatModel, resolver PathResolver, cache ContentLoader) *Formatter {
	return &Formatter{
		chatModel: chatModel,
		resolver:  resolver,
		cache:     cache,
	}
}

// loadSkillContent resolves skillName to an absolute SKILL.md path via the
// PathResolver, then loads the file content via the ContentLoader.
// Returns ("", nil) when skillName is empty.
func (f *Formatter) loadSkillContent(ctx context.Context, skillName string) (string, error) {
	if skillName == "" {
		return "", nil
	}

	path, err := f.resolver.SkillFilePath(skillName)
	if err != nil {
		return "", fmt.Errorf("formatter: skill %q not found: %w", skillName, err)
	}

	content, err := f.cache.Load(ctx, path)
	if err != nil {
		return "", fmt.Errorf("formatter: failed to load skill content for %q: %w", skillName, err)
	}

	return content, nil
}

// Format reformats content using the specified formatter skill via a single LLM call.
// The skill's SKILL.md is used as the LLM system prompt; content is the user message.
// Returns content unchanged when skillName is empty.
//
// This method is called only when formatter_post_step is true.
func (f *Formatter) Format(ctx context.Context, skillName, content string) (string, error) {
	if skillName == "" {
		return content, nil
	}

	skillContent, err := f.loadSkillContent(ctx, skillName)
	if err != nil {
		return "", err
	}

	messages := []*schema.Message{
		schema.SystemMessage(skillContent),
		schema.UserMessage(content),
	}

	resp, err := f.chatModel.Generate(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("formatter: LLM call failed: %w", err)
	}

	if resp == nil {
		return "", errors.New("formatter: LLM returned nil response")
	}

	formatted := resp.Content
	if formatted == "" {
		// Fallback: return original content rather than an empty string.
		return content, nil
	}

	return formatted, nil
}

// LoadSkillPrompt resolves and returns the SKILL.md content for the named
// formatter skill so that the caller can inject it into the agent's initial
// system prompt (formatter_post_step: false mode).
// Returns ("", nil) when skillName is empty.
func (f *Formatter) LoadSkillPrompt(ctx context.Context, skillName string) (string, error) {
	if skillName == "" {
		return "", nil
	}

	content, err := f.loadSkillContent(ctx, skillName)
	if err != nil {
		return "", fmt.Errorf("formatter: LoadSkillPrompt failed for %q: %w", skillName, err)
	}

	return content, nil
}

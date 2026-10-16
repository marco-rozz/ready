//nolint:goconst
package formatter_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/formatter"
)

// ---- test doubles -----------------------------------------------------------

// mockPathResolver implements formatter.PathResolver.
type mockPathResolver struct {
	paths map[string]string // skillName → absolute path
}

func (r *mockPathResolver) SkillFilePath(skillName string) (string, error) {
	if path, ok := r.paths[skillName]; ok {
		return path, nil
	}

	return "", errors.New("skill not found: " + skillName)
}

// mockContentLoader implements formatter.ContentLoader.
type mockContentLoader struct {
	contents map[string]string // path → content
}

func (l *mockContentLoader) Load(_ context.Context, path string) (string, error) {
	if content, ok := l.contents[path]; ok {
		return content, nil
	}

	return "", errors.New("file not found: " + path)
}

// mockChatModel implements model.ToolCallingChatModel.
type mockChatModel struct {
	response string
	err      error
	called   bool
}

var _ model.ToolCallingChatModel = (*mockChatModel)(nil)

func (m *mockChatModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.called = true

	if m.err != nil {
		return nil, m.err
	}

	return &schema.Message{Role: schema.Assistant, Content: m.response}, nil
}

func (m *mockChatModel) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: m.response}}), nil
}

func (m *mockChatModel) WithTools(_ []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

// ---- unit tests: LoadSkillPrompt --------------------------------------------

// TestLoadSkillPrompt_EmptySkillName verifies that an empty skill name returns
// ("", nil) without any resolver or cache call.
func TestLoadSkillPrompt_EmptySkillName(t *testing.T) {
	f := formatter.New(&mockChatModel{}, &mockPathResolver{}, &mockContentLoader{})

	got, err := f.LoadSkillPrompt(context.Background(), "")
	if err != nil {
		t.Fatalf("LoadSkillPrompt(\"\") returned unexpected error: %v", err)
	}

	if got != "" {
		t.Errorf("LoadSkillPrompt(\"\") = %q; want empty string", got)
	}
}

// TestLoadSkillPrompt_ValidSkill verifies that a known skill name resolves to
// the correct content via the cache mock.
func TestLoadSkillPrompt_ValidSkill(t *testing.T) {
	const skillPath = "/fake/skills/telegram-formatter/SKILL.md"
	const skillContent = "## Telegram Formatter\nFormat for Telegram."

	resolver := &mockPathResolver{paths: map[string]string{"telegram-formatter": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: skillContent}}
	f := formatter.New(&mockChatModel{}, resolver, cache)

	got, err := f.LoadSkillPrompt(context.Background(), "telegram-formatter")
	if err != nil {
		t.Fatalf("LoadSkillPrompt() returned unexpected error: %v", err)
	}

	if got != skillContent {
		t.Errorf("LoadSkillPrompt() = %q; want %q", got, skillContent)
	}
}

// TestLoadSkillPrompt_MissingSkill verifies that an unknown skill name
// propagates an error from the resolver.
func TestLoadSkillPrompt_MissingSkill(t *testing.T) {
	f := formatter.New(&mockChatModel{}, &mockPathResolver{}, &mockContentLoader{})

	_, err := f.LoadSkillPrompt(context.Background(), "nonexistent-skill")
	if err == nil {
		t.Fatal("LoadSkillPrompt() for missing skill should return an error; got nil")
	}
}

// TestLoadSkillPrompt_CacheMiss verifies that a cache miss (file not in loader)
// propagates an error.
func TestLoadSkillPrompt_CacheMiss(t *testing.T) {
	const skillPath = "/fake/skills/missing/SKILL.md"

	resolver := &mockPathResolver{paths: map[string]string{"missing-skill": skillPath}}
	// cache has no entry for skillPath
	cache := &mockContentLoader{contents: map[string]string{}}
	f := formatter.New(&mockChatModel{}, resolver, cache)

	_, err := f.LoadSkillPrompt(context.Background(), "missing-skill")
	if err == nil {
		t.Fatal("LoadSkillPrompt() should return an error when content is not in cache; got nil")
	}
}

// ---- unit tests: Format -----------------------------------------------------

// TestFormat_EmptySkillName verifies that Format returns content unchanged
// without any LLM call when skillName is empty.
func TestFormat_EmptySkillName(t *testing.T) {
	llm := &mockChatModel{response: "should not be called"}
	f := formatter.New(llm, &mockPathResolver{}, &mockContentLoader{})

	got, err := f.Format(context.Background(), "", "original response")
	if err != nil {
		t.Fatalf("Format(\"\") returned unexpected error: %v", err)
	}

	if got != "original response" {
		t.Errorf("Format(\"\") = %q; want %q", got, "original response")
	}

	if llm.called {
		t.Error("Format(\"\") must not invoke the LLM")
	}
}

// TestFormat_ValidSkill verifies the happy path: skill resolved, LLM called,
// reformatted content returned.
func TestFormat_ValidSkill(t *testing.T) {
	const skillPath = "/fake/petscii/SKILL.md"
	const skillContent = "Format for PETSCII."
	const agentResponse = "The light is now on."
	const reformatted = "LICHT AN."

	resolver := &mockPathResolver{paths: map[string]string{"petscii-formatter": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: skillContent}}
	llm := &mockChatModel{response: reformatted}
	f := formatter.New(llm, resolver, cache)

	got, err := f.Format(context.Background(), "petscii-formatter", agentResponse)
	if err != nil {
		t.Fatalf("Format() returned unexpected error: %v", err)
	}

	if got != reformatted {
		t.Errorf("Format() = %q; want %q", got, reformatted)
	}

	if !llm.called {
		t.Error("Format() must invoke the LLM")
	}
}

// TestFormat_LLMError verifies that LLM errors are propagated.
func TestFormat_LLMError(t *testing.T) {
	const skillPath = "/fake/skill/SKILL.md"

	resolver := &mockPathResolver{paths: map[string]string{"my-fmt": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: "format rules"}}
	llm := &mockChatModel{err: errors.New("LLM unavailable")}
	f := formatter.New(llm, resolver, cache)

	_, err := f.Format(context.Background(), "my-fmt", "some content")
	if err == nil {
		t.Fatal("Format() should return an error on LLM failure; got nil")
	}
}

// TestFormat_LLMEmptyResponse verifies that an empty LLM response falls back
// to the original content.
func TestFormat_LLMEmptyResponse(t *testing.T) {
	const skillPath = "/fake/skill/SKILL.md"

	resolver := &mockPathResolver{paths: map[string]string{"my-fmt": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: "format rules"}}
	llm := &mockChatModel{response: ""} // empty — triggers fallback
	f := formatter.New(llm, resolver, cache)

	got, err := f.Format(context.Background(), "my-fmt", "fallback content")
	if err != nil {
		t.Fatalf("Format() returned unexpected error: %v", err)
	}

	if got != "fallback content" {
		t.Errorf("Format() with empty LLM response = %q; want original content", got)
	}
}

// ---- integration tests: operating modes ------------------------------------

// TestIntegration_PostStepFalse_LoadSkillPromptUsed verifies the
// formatter_post_step: false path:
//   - LoadSkillPrompt returns the skill's SKILL.md content.
//   - The content can be appended to the system prompt.
//   - Format (LLM call) is NOT invoked.
func TestIntegration_PostStepFalse_LoadSkillPromptUsed(t *testing.T) {
	const skillPath = "/fake/telegram/SKILL.md"
	const skillContent = "## Telegram Formatter\nUse HTML formatting."

	resolver := &mockPathResolver{paths: map[string]string{"telegram-formatter": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: skillContent}}
	llm := &mockChatModel{response: "should not be called"}

	f := formatter.New(llm, resolver, cache)

	// formatter_post_step: false → LoadSkillPrompt to get inline injection
	formatterSkill := "telegram-formatter"
	formatterPostStep := false

	var systemPromptExtra string

	if formatterSkill != "" && !formatterPostStep {
		var err error

		systemPromptExtra, err = f.LoadSkillPrompt(context.Background(), formatterSkill)
		if err != nil {
			t.Fatalf("LoadSkillPrompt() failed: %v", err)
		}
	}

	// Verify: skill content appears in the assembled system prompt.
	systemPrompt := "Base system prompt.\n\n" + systemPromptExtra
	if !strings.Contains(systemPrompt, skillContent) {
		t.Errorf("system prompt does not contain formatter skill content:\nPrompt: %q\nExpected to contain: %q",
			systemPrompt, skillContent)
	}

	// Verify: LLM (Format) was NOT called.
	if llm.called {
		t.Error("Format (LLM) must NOT be called in formatter_post_step: false mode")
	}
}

// TestIntegration_PostStepTrue_FormatCalled verifies the
// formatter_post_step: true path:
//   - Format is called after the agent's Final Answer.
//   - The LLM produces a reformatted response.
//   - LoadSkillPrompt is not involved (no injection into system prompt).
func TestIntegration_PostStepTrue_FormatCalled(t *testing.T) {
	const skillPath = "/fake/petscii/SKILL.md"
	const skillContent = "PETSCII rules."
	const agentResponse = "The lights are on."
	const reformatted = "LICHT AN"

	resolver := &mockPathResolver{paths: map[string]string{"petscii-formatter": skillPath}}
	cache := &mockContentLoader{contents: map[string]string{skillPath: skillContent}}
	llm := &mockChatModel{response: reformatted}

	f := formatter.New(llm, resolver, cache)

	formatterSkill := "petscii-formatter"
	formatterPostStep := true

	var systemPromptExtra string // should remain empty — no injection

	// formatter_post_step: true → no LoadSkillPrompt injection.
	if formatterSkill != "" && !formatterPostStep {
		var err error

		systemPromptExtra, err = f.LoadSkillPrompt(context.Background(), formatterSkill)
		if err != nil {
			t.Fatalf("LoadSkillPrompt() unexpected call: %v", err)
		}
	}

	// Verify: nothing was injected into the system prompt.
	if systemPromptExtra != "" {
		t.Errorf("system prompt extra must be empty for post_step=true; got %q", systemPromptExtra)
	}

	// Simulate: Final Answer produced → apply formatter post-step.
	finalResponse := agentResponse

	if formatterSkill != "" && formatterPostStep {
		var err error

		finalResponse, err = f.Format(context.Background(), formatterSkill, agentResponse)
		if err != nil {
			t.Fatalf("Format() failed: %v", err)
		}
	}

	// Verify: LLM was called and response is the reformatted version.
	if !llm.called {
		t.Error("Format (LLM) must be called in formatter_post_step: true mode")
	}

	if finalResponse != reformatted {
		t.Errorf("final response = %q; want %q", finalResponse, reformatted)
	}
}

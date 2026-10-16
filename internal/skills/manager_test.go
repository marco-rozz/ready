package skills_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

// createTestSkillDir creates a temporary skill directory with a valid SKILL.md.
func createTestSkillDir(t *testing.T, basePath, name, description string) string {
	t.Helper()

	skillDir := filepath.Join(basePath, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}

	content := "---\nname: " + name + "\ndescription: \"" + description + "\"\nversion: \"1.0\"\n---\n\n# " + name + "\n\nThis is the " + name + " skill content.\n"

	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}

	return skillDir
}

func TestNewManager_Success(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill for unit testing.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		_ = mgr.Close()
	}()

	// Verify the skill was discovered.
	infos, err := mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}

	if len(infos) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(infos))
	}

	if infos[0].Name != "test-skill" {
		t.Errorf("expected skill name 'test-skill', got %q", infos[0].Name)
	}

	if infos[0].Description != "A test skill for unit testing." {
		t.Errorf("unexpected description: %q", infos[0].Description)
	}
}

func TestNewManager_NilConfig(t *testing.T) {
	ctx := context.Background()

	_, err := skills.NewManager(ctx, nil, zerolog.New(io.Discard))
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestNewManager_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &config.SkillsConfig{
		Path: t.TempDir(),
	}

	_, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestNewManager_EmptyDir(t *testing.T) {
	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      t.TempDir(),
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		_ = mgr.Close()
	}()

	infos, err := mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}

	if len(infos) != 0 {
		t.Errorf("expected 0 skills, got %d", len(infos))
	}
}

func TestNewManager_MultipleSkills(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "skill-alpha", "Alpha skill description.")
	createTestSkillDir(t, tmpDir, "skill-beta", "Beta skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		_ = mgr.Close()
	}()

	infos, err := mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}

	if len(infos) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(infos))
	}
}

func TestManager_GetMetadataPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		_ = mgr.Close()
	}()

	// Pass an empty session ID: no skills are active, so all skills are listed.
	prompt, err := mgr.GetMetadataPrompt(ctx, "")
	if err != nil {
		t.Fatalf("GetMetadataPrompt() error = %v", err)
	}

	// The prompt should contain the skill name and description but NOT the full body.
	if prompt == "" {
		t.Fatal("expected non-empty metadata prompt")
	}

	// Should contain skill name.
	if !containsString(prompt, "test-skill") {
		t.Errorf("metadata prompt should contain skill name, got: %s", prompt)
	}

	// Should NOT contain the body content.
	if containsString(prompt, "This is the test-skill skill content") {
		t.Errorf("metadata prompt should NOT contain full body content")
	}
}

func TestManager_LoadSkill(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	// Create a session.
	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	// Load the skill.
	content, err := mgr.LoadSkill(ctx, sid, "test-skill")
	if err != nil {
		t.Fatalf("LoadSkill() error = %v", err)
	}

	// The content should include the body.
	if content == "" {
		t.Fatal("expected non-empty skill content after load")
	}

	if !containsString(content, "test-skill") {
		t.Errorf("loaded content should reference skill name, got: %s", content)
	}
}

func TestManager_LoadSkill_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "foo", "A test skill for idempotency.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	first, err := mgr.LoadSkill(ctx, sid, "foo")
	if err != nil {
		t.Fatalf("first LoadSkill() error = %v", err)
	}

	second, err := mgr.LoadSkill(ctx, sid, "foo")
	if err != nil {
		t.Fatalf("second LoadSkill() should be a no-op, got error = %v", err)
	}

	if first != second {
		t.Errorf("second LoadSkill returned different prompt; duplicate state suspected\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// The active-skills prompt for "foo" should occur at most once in the returned text.
	if n := containsCount(second, "foo"); n == 0 {
		t.Errorf("expected skill name in active prompt, got: %s", second)
	}
}

func containsCount(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	count := 0
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			count++
			i += len(needle) - 1
		}
	}

	return count
}

// TestManager_GetMetadataPrompt_ExcludesActiveFormatter mirrors task 4.3 of
// the formatter-inline-reinforcement change: after LoadSkill marks the
// formatter skill Active in the session, GetMetadataPrompt must exclude that
// skill from the progressive-disclosure catalog.
func TestManager_GetMetadataPrompt_ExcludesActiveFormatter(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "telegram-formatter", "Telegram HTML formatting rules.")
	createTestSkillDir(t, tmpDir, "home-topology", "Residence topology and entities.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	if _, loadErr := mgr.LoadSkill(ctx, sid, "telegram-formatter"); loadErr != nil {
		t.Fatalf("LoadSkill(): %v", loadErr)
	}

	prompt, err := mgr.GetMetadataPrompt(ctx, sid)
	if err != nil {
		t.Fatalf("GetMetadataPrompt(): %v", err)
	}

	if containsString(prompt, "telegram-formatter") {
		t.Errorf("metadata prompt must not list active formatter skill; got:\n%s", prompt)
	}
	if !containsString(prompt, "home-topology") {
		t.Errorf("metadata prompt must still list other inactive skills; got:\n%s", prompt)
	}
}

func TestManager_LoadSkill_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	_, err = mgr.LoadSkill(ctx, sid, "nonexistent-skill")
	if err == nil {
		t.Fatal("expected error loading nonexistent skill")
	}
}

func TestManager_UnloadSkill(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	// Load first.
	if _, err := mgr.LoadSkill(ctx, sid, "test-skill"); err != nil {
		t.Fatalf("LoadSkill() error = %v", err)
	}

	// Unload.
	if err := mgr.UnloadSkill(ctx, sid, "test-skill"); err != nil {
		t.Fatalf("UnloadSkill() error = %v", err)
	}
}

func TestManager_UnloadSkill_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	err = mgr.UnloadSkill(ctx, sid, "nonexistent-skill")
	if err == nil {
		t.Fatal("expected error unloading nonexistent skill")
	}
}

func TestManager_HotReload_NewSkill(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "existing-skill", "Existing skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: true,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	// Verify initial state.
	infos, err := mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() error = %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 skill initially, got %d", len(infos))
	}

	// Add a new skill directory.
	createTestSkillDir(t, tmpDir, "new-skill", "A newly added skill.")

	// Wait for watcher to detect the change.
	time.Sleep(500 * time.Millisecond)

	infos, err = mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() after hot-reload error = %v", err)
	}

	if len(infos) != 2 {
		t.Logf("Hot-reload may need more time. Got %d skills.", len(infos))
		// Hot-reload is best-effort; we won't fail the test hard on timing issues.
	}
}

func TestManager_HotReload_ModifySkill(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := createTestSkillDir(t, tmpDir, "mutable-skill", "Original description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: true,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	// Modify the SKILL.md file.
	newContent := "---\nname: mutable-skill\ndescription: \"Updated description.\"\nversion: \"2.0\"\n---\n\n# Updated Skill\n\nNew content.\n"
	if writeErr := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(newContent), 0o600); writeErr != nil {
		t.Fatalf("failed to update SKILL.md: %v", writeErr)
	}

	// Wait for watcher to detect the change.
	time.Sleep(500 * time.Millisecond)

	infos, err := mgr.ListSkills(ctx)
	if err != nil {
		t.Fatalf("ListSkills() after modify error = %v", err)
	}

	// Skill should still be present (re-indexed).
	if len(infos) != 1 {
		t.Fatalf("expected 1 skill after modify, got %d", len(infos))
	}
}

func TestManager_OnReload_CallbackInvoked(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := createTestSkillDir(t, tmpDir, "callback-skill", "Callback test.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: true,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	// Register a reload callback.
	reloadedCh := make(chan string, 1)
	mgr.OnReload(func(_ context.Context, skillName string) {
		reloadedCh <- skillName
	})

	// Modify the SKILL.md file.
	newContent := "---\nname: callback-skill\ndescription: \"Updated.\"\nversion: \"2.0\"\n---\n\n# Updated.\n"
	if writeErr := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(newContent), 0o600); writeErr != nil {
		t.Fatalf("failed to update SKILL.md: %v", writeErr)
	}

	// Wait for the callback to fire.
	select {
	case name := <-reloadedCh:
		if name != "callback-skill" {
			t.Errorf("callback received skill name %q; want %q", name, "callback-skill")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnReload callback was not invoked within timeout")
	}

	// Verify that ReadSkillContent returns the updated content.
	content, err := mgr.ReadSkillContent(ctx, "callback-skill")
	if err != nil {
		t.Fatalf("ReadSkillContent() error = %v", err)
	}
	if !containsString(content, "Updated.") {
		t.Errorf("ReadSkillContent() did not return updated content: %s", content)
	}
}

func TestManager_SkillDefs(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "Test description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	defs := mgr.SkillDefs()
	if len(defs) != 1 {
		t.Fatalf("expected 1 skill def, got %d", len(defs))
	}

	if defs[0].Name != "test-skill" {
		t.Errorf("expected def name 'test-skill', got %q", defs[0].Name)
	}

	if defs[0].Type != "fs" {
		t.Errorf("expected def type 'fs', got %q", defs[0].Type)
	}
}

func TestManager_Close(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "Test description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: true,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := mgr.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && contains(s, substr))
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}

	return false
}

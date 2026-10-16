package skills_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

func TestSkillsLoadTool_Info(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	tool := skills.NewSkillsLoadTool(mgr)
	info, err := tool.Info(ctx)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	if info.Name != "skills-load" {
		t.Errorf("expected tool name 'skills-load', got %q", info.Name)
	}

	if info.Desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestSkillsLoadTool_InvokableRun_Success(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	// Create session and set in context.
	sid, err := mgr.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	toolCtx := skills.WithSessionID(ctx, sid)
	tool := skills.NewSkillsLoadTool(mgr)

	result, err := tool.InvokableRun(toolCtx, `{"skill_name":"test-skill"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if result == "" {
		t.Error("expected non-empty result from InvokableRun")
	}
}

func TestSkillsLoadTool_InvokableRun_NoSession(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	tool := skills.NewSkillsLoadTool(mgr)

	// No session in context.
	_, err = tool.InvokableRun(ctx, `{"skill_name":"test-skill"}`)
	if err == nil {
		t.Fatal("expected error when no session in context")
	}
}

func TestSkillsLoadTool_InvokableRun_EmptyName(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

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

	toolCtx := skills.WithSessionID(ctx, sid)
	tool := skills.NewSkillsLoadTool(mgr)

	_, err = tool.InvokableRun(toolCtx, `{"skill_name":""}`)
	if err == nil {
		t.Fatal("expected error for empty skill_name")
	}
}

func TestSkillsLoadTool_InvokableRun_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

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

	toolCtx := skills.WithSessionID(ctx, sid)
	tool := skills.NewSkillsLoadTool(mgr)

	_, err = tool.InvokableRun(toolCtx, `{invalid json}`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSkillsUnloadTool_Info(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	tool := skills.NewSkillsUnloadTool(mgr)
	info, err := tool.Info(ctx)
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	if info.Name != "skills-unload" {
		t.Errorf("expected tool name 'skills-unload', got %q", info.Name)
	}

	if info.Desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestSkillsUnloadTool_InvokableRun_Success(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

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

	toolCtx := skills.WithSessionID(ctx, sid)

	// Load first.
	loadTool := skills.NewSkillsLoadTool(mgr)
	if _, loadErr := loadTool.InvokableRun(toolCtx, `{"skill_name":"test-skill"}`); loadErr != nil {
		t.Fatalf("LoadTool InvokableRun() error = %v", loadErr)
	}

	// Then unload.
	unloadTool := skills.NewSkillsUnloadTool(mgr)
	result, err := unloadTool.InvokableRun(toolCtx, `{"skill_name":"test-skill"}`)
	if err != nil {
		t.Fatalf("UnloadTool InvokableRun() error = %v", err)
	}

	if result == "" {
		t.Error("expected non-empty result from unload")
	}
}

func TestSkillsUnloadTool_InvokableRun_NoSession(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer mgr.Close()

	tool := skills.NewSkillsUnloadTool(mgr)

	_, err = tool.InvokableRun(ctx, `{"skill_name":"test-skill"}`)
	if err == nil {
		t.Fatal("expected error when no session in context")
	}
}

func TestSkillsUnloadTool_InvokableRun_EmptyName(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

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

	toolCtx := skills.WithSessionID(ctx, sid)
	tool := skills.NewSkillsUnloadTool(mgr)

	_, err = tool.InvokableRun(toolCtx, `{"skill_name":""}`)
	if err == nil {
		t.Fatal("expected error for empty skill_name")
	}
}

func TestWithSessionID_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "test-skill", "A test skill.")

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

	toolCtx := skills.WithSessionID(ctx, sid)
	got, ok := skills.GetSessionID(toolCtx)

	if !ok {
		t.Fatal("expected session ID to be present in context")
	}

	if got != sid {
		t.Errorf("expected session ID %q, got %q", sid, got)
	}
}

func TestGetSessionID_Missing(t *testing.T) {
	ctx := context.Background()
	_, ok := skills.GetSessionID(ctx)

	if ok {
		t.Fatal("expected no session ID in bare context")
	}
}

func TestIntegration_LoadAndRetrieveSkillContent(t *testing.T) {
	// This test verifies the full flow: manager → session → load → content retrieval.
	tmpDir := t.TempDir()

	// Create a skill with specific content.
	skillDir := filepath.Join(tmpDir, "my-skill")
	if err := createSkillWithContent(skillDir, "my-skill", "A detailed skill.", "# My Skill\n\nDetailed body content here."); err != nil {
		t.Fatalf("failed to create skill: %v", err)
	}

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

	content, err := mgr.LoadSkill(ctx, sid, "my-skill")
	if err != nil {
		t.Fatalf("LoadSkill() error = %v", err)
	}

	if !containsString(content, "Detailed body content here") {
		t.Errorf("expected loaded content to contain body, got: %s", content)
	}
}

// createSkillWithContent creates a skill directory with specific body content.
func createSkillWithContent(dir, name, description, body string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	content := "---\nname: " + name + "\ndescription: \"" + description + "\"\nversion: \"1.0\"\n---\n\n" + body + "\n"

	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600)
}

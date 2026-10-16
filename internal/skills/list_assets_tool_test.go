package skills_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

// listAssetsSkillName is the skill created by newListAssetsFixture.
const listAssetsSkillName = "test-skill"

// newListAssetsFixture creates a manager rooted at a temp dir containing a
// single skill, and returns the manager plus the skill directory path.
func newListAssetsFixture(t *testing.T) (*skills.Manager, string) {
	t.Helper()

	tmpDir := t.TempDir()
	skillDir := createTestSkillDir(t, tmpDir, listAssetsSkillName, "A test skill.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{Path: tmpDir, HotReload: false}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	return mgr, skillDir
}

// writeAsset writes a file (creating parent dirs) inside the skill directory.
func writeAsset(t *testing.T, skillDir, relPath, content string) {
	t.Helper()

	full := filepath.Join(skillDir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("failed to create asset dir: %v", err)
	}

	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write asset %q: %v", relPath, err)
	}
}

// parseAssetLines extracts the listed relative paths from the tool output.
func parseAssetLines(t *testing.T, output string) []string {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected a header plus at least one asset line, got %q", output)
	}

	got := append([]string(nil), lines[1:]...)
	sort.Strings(got)

	return got
}

func TestListSkillAssetsTool_Info(t *testing.T) {
	mgr, _ := newListAssetsFixture(t)

	info, err := skills.NewListSkillAssetsTool(mgr).Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error = %v", err)
	}

	if info.Name != "skills-list-assets" {
		t.Errorf("expected tool name 'skills-list-assets', got %q", info.Name)
	}

	if info.Desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestListSkillAssetsTool_InvokableRun_ListsNestedRelativePaths(t *testing.T) {
	mgr, skillDir := newListAssetsFixture(t)
	writeAsset(t, skillDir, filepath.Join("assets", "devices.md"), "devices")
	writeAsset(t, skillDir, filepath.Join("generated", "ha-config.md"), "config")

	tool := skills.NewListSkillAssetsTool(mgr)

	result, err := tool.InvokableRun(context.Background(), `{"skill_name":"test-skill"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	want := []string{
		"assets",
		filepath.Join("assets", "devices.md"),
		"generated",
		filepath.Join("generated", "ha-config.md"),
	}
	sort.Strings(want)

	got := parseAssetLines(t, result)
	if len(got) != len(want) {
		t.Fatalf("expected %d assets, got %d: %v", len(want), len(got), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("asset[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestListSkillAssetsTool_InvokableRun_ExcludesRootAndSkillMD(t *testing.T) {
	mgr, skillDir := newListAssetsFixture(t)
	// Every file named SKILL.md is excluded, at any depth; directories are kept.
	writeAsset(t, skillDir, filepath.Join("nested", "SKILL.md"), "nested skill doc")
	writeAsset(t, skillDir, filepath.Join("nested", "notes.md"), "notes")

	tool := skills.NewListSkillAssetsTool(mgr)

	result, err := tool.InvokableRun(context.Background(), `{"skill_name":"test-skill"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	got := parseAssetLines(t, result)
	want := []string{"nested", filepath.Join("nested", "notes.md")}
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("asset[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if strings.Contains(result, "SKILL.md") {
		t.Errorf("SKILL.md files must be excluded from the listing, got: %s", result)
	}
}

func TestListSkillAssetsTool_InvokableRun_NoAssets(t *testing.T) {
	mgr, _ := newListAssetsFixture(t)

	tool := skills.NewListSkillAssetsTool(mgr)

	result, err := tool.InvokableRun(context.Background(), `{"skill_name":"test-skill"}`)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}

	if result != "No additional assets found for this skill." {
		t.Errorf("unexpected result for skill without assets: %q", result)
	}
}

func TestListSkillAssetsTool_InvokableRun_ContextCancelled(t *testing.T) {
	mgr, skillDir := newListAssetsFixture(t)
	writeAsset(t, skillDir, filepath.Join("assets", "devices.md"), "devices")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := skills.NewListSkillAssetsTool(mgr)

	_, err := tool.InvokableRun(ctx, `{"skill_name":"test-skill"}`)
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected error to wrap context.Canceled, got %v", err)
	}
}

func TestListSkillAssetsTool_InvokableRun_EmptyName(t *testing.T) {
	mgr, _ := newListAssetsFixture(t)

	tool := skills.NewListSkillAssetsTool(mgr)

	if _, err := tool.InvokableRun(context.Background(), `{"skill_name":""}`); err == nil {
		t.Fatal("expected error for empty skill_name")
	}
}

func TestListSkillAssetsTool_InvokableRun_InvalidJSON(t *testing.T) {
	mgr, _ := newListAssetsFixture(t)

	tool := skills.NewListSkillAssetsTool(mgr)

	if _, err := tool.InvokableRun(context.Background(), `{invalid json}`); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestListSkillAssetsTool_InvokableRun_UnknownSkill(t *testing.T) {
	mgr, _ := newListAssetsFixture(t)

	tool := skills.NewListSkillAssetsTool(mgr)

	if _, err := tool.InvokableRun(context.Background(), `{"skill_name":"does-not-exist"}`); err == nil {
		t.Fatal("expected error for unknown skill")
	}
}

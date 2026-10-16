package skills_test

// Regression test for criticalities-review-2026-09-24 M11: skill asset
// containment used a bare strings.HasPrefix without a path-separator
// boundary, so a skill at .ready/skills/foo could expose sibling directories
// sharing its name prefix (.ready/skills/foo-bar).

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

// cacheLoader adapts the skills Manager's own cache shape: the tool needs a
// simple Load(ctx, path) (string, error).
type fsLoader struct{}

func (fsLoader) Load(_ context.Context, path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

// TestReadSkillAsset_SiblingDirectoryNotExposed pins M11: a skill named
// "foo" cannot read assets from a sibling "foo-bar" directory via the
// "../foo-bar/..." escape.
func TestReadSkillAsset_SiblingDirectoryNotExposed(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "foo", "Foo skill.")
	createTestSkillDir(t, tmpDir, "foo-bar", "Sibling skill.")

	// A secret asset inside the sibling skill directory.
	sibling := filepath.Join(tmpDir, "foo-bar", "secret.md")
	if err := os.WriteFile(sibling, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatalf("write sibling secret: %v", err)
	}

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() { _ = mgr.Close() }()

	readTool := skills.NewReadSkillAssetTool(mgr, fsLoader{})

	args := `{"skill_name": "foo", "asset_path": "../foo-bar/secret.md"}`
	_, runErr := readTool.InvokableRun(ctx, args)
	if runErr == nil {
		t.Fatal("asset path ../foo-bar/secret.md accepted — the prefix-only containment let a skill expose sibling directories")
	}
}

// TestReadSkillAsset_OwnDirectoryStillAllowed pins the positive side of M11:
// assets inside the skill's own directory (and subdirectories) still load.
func TestReadSkillAsset_OwnDirectoryStillAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	skillDir := createTestSkillDir(t, tmpDir, "own", "Own skill.")

	asset := filepath.Join(skillDir, "notes.md")
	if err := os.WriteFile(asset, []byte("visible"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: false,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() { _ = mgr.Close() }()

	readTool := skills.NewReadSkillAssetTool(mgr, fsLoader{})

	if err := os.WriteFile(filepath.Join(skillDir, "asset.md"), []byte("sub content"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	args := `{"skill_name": "own", "asset_path": "asset.md"}`

	content, runErr := readTool.InvokableRun(ctx, args)
	if runErr != nil {
		t.Fatalf("own asset rejected: %v", runErr)
	}

	if content != "sub content" {
		t.Fatalf("content = %q", content)
	}
}

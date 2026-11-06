package initwizard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skillssync"
	"github.com/rs/zerolog"
)

const (
	testGuideSkill = "ready-guide"
	testSkillFile  = "SKILL.md"
	testAssetsDir  = "assets"
	testHelpFile   = "help.md"
	testSkillsDir  = "skills"
)

func installTestSkillLibrary(ctx context.Context, t *testing.T) string {
	t.Helper()
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	library := config.SharedLibraryDir()
	if err := os.MkdirAll(filepath.Join(library, testGuideSkill, testAssetsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, testGuideSkill, testSkillFile), []byte("shipped guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, testGuideSkill, testAssetsDir, testHelpFile), []byte("shipped help\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return library
}

func TestInitCopiesSkillsAndServeSyncPreservesEdits(t *testing.T) {
	library := installTestSkillLibrary(t.Context(), t)
	var output strings.Builder
	opts := baseOptions(freshInterviewAnswer+"n\n", true, &output)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	outcome, err := Run(t.Context(), opts)
	if err != nil || !outcome.Committed {
		t.Fatalf("init: committed=%v err=%v", outcome.Committed, err)
	}
	cfg, err := config.Load(t.Context(), outcome.Resolution.Path)
	if err != nil {
		t.Fatal(err)
	}
	guide := filepath.Join(cfg.Skills.Path, testGuideSkill, testSkillFile)
	help := filepath.Join(cfg.Skills.Path, testGuideSkill, testAssetsDir, testHelpFile)
	if readFileOrFail(t, guide) != "shipped guide\n" || readFileOrFail(t, help) != "shipped help\n" {
		t.Fatal("init did not prepare the installed skill and its assets")
	}
	if _, err := os.Stat(filepath.Join(cfg.Skills.Path, ".library-state.json")); err != nil {
		t.Fatalf("initial copy has no synchronization baseline: %v", err)
	}
	if !strings.Contains(output.String(), "Skills prepared in") {
		t.Fatal("wizard did not report the initial skill copy")
	}
	if err := os.WriteFile(guide, []byte("my personalized guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, testGuideSkill, testSkillFile), []byte("updated shipped guide\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(library, testGuideSkill, testAssetsDir, testHelpFile), []byte("updated shipped help\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Serve invokes this same synchronizer at boot after a library upgrade.
	if err := skillssync.Run(t.Context(), cfg.Skills, cfg.RuntimeHome, zerolog.Nop()); err != nil {
		t.Fatal(err)
	}
	if readFileOrFail(t, guide) != "my personalized guide\n" || readFileOrFail(t, help) != "updated shipped help\n" {
		t.Fatal("serve sync must preserve edits and update untouched files after init")
	}
}

func TestInitialSkillCopyFailureKeepsCommittedConfig(t *testing.T) {
	installTestSkillLibrary(t.Context(), t)
	agent := t.TempDir()
	if err := os.WriteFile(filepath.Join(agent, testSkillsDir), []byte("existing file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := baseOptions(freshInterviewAnswer, true, &strings.Builder{})
	opts.ExplicitAgentDir = agent
	outcome, err := Run(t.Context(), opts)
	if err == nil || !outcome.Committed {
		t.Fatalf("copy failure must report the committed config: committed=%v err=%v", outcome.Committed, err)
	}
	if _, err := config.Load(t.Context(), outcome.Resolution.Path); err != nil {
		t.Fatalf("copy failure discarded the config: %v", err)
	}
	if readFileOrFail(t, filepath.Join(agent, testSkillsDir)) != "existing file\n" {
		t.Fatal("copy failure replaced pre-existing user data")
	}
}

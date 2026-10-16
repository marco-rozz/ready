package skills_test

// Regression test for criticalities-review-2026-09-24 H2: removeSkill took a
// pointer INTO the tracked slice, then shifted the slice — the element at the
// removed index was overwritten by the next definition, so the runtime
// removal targeted a still-valid neighbour. Trigger: the fsnotify watcher on
// a deleted skill directory.

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

// TestManager_RemoveCorrectSkillOnDirectoryDelete pins H2: deleting skill
// "skill-alpha" must remove skill-alpha from the runtime; skill-beta must
// remain fully loaded and addressable.
func TestManager_RemoveCorrectSkillOnDirectoryDeletion(t *testing.T) {
	tmpDir := t.TempDir()
	createTestSkillDir(t, tmpDir, "skill-alpha", "Alpha skill description.")
	createTestSkillDir(t, tmpDir, "skill-beta", "Beta skill description.")

	ctx := context.Background()
	cfg := &config.SkillsConfig{
		Path:      tmpDir,
		HotReload: true,
	}

	mgr, err := skills.NewManager(ctx, cfg, zerolog.New(io.Discard))
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer func() {
		_ = mgr.Close()
	}()

	alphaDir := createTestSkillDir(t, tmpDir, "skill-alpha", "Alpha skill description.")
	if err := os.RemoveAll(alphaDir); err != nil {
		t.Fatalf("remove skill-alpha dir: %v", err)
	}

	// Wait for the watcher to process the deletion.
	deadline := time.Now().Add(5 * time.Second)
	for {
		infos, listErr := mgr.ListSkills(ctx)
		if listErr != nil {
			t.Fatalf("ListSkills(): %v", listErr)
		}

		if len(infos) == 1 {
			if infos[0].Name != "skill-beta" {
				t.Fatalf("remaining skill = %q, want skill-beta (H2: the slice-shift re-based the removal pointer onto the still-valid neighbour)", infos[0].Name)
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("expected exactly skill-beta to remain, got %v", infos)
		}

		time.Sleep(50 * time.Millisecond)
	}

	// skill-beta must still be loadable after the neighbour's removal.
	sid, sidErr := mgr.NewSession(ctx)
	if sidErr != nil {
		t.Fatalf("NewSession(): %v", sidErr)
	}
	defer func() { _ = mgr.CloseSession(ctx, sid) }()

	if _, err := mgr.LoadSkill(ctx, sid, "skill-beta"); err != nil {
		t.Fatalf("LoadSkill(skill-beta) after removing skill-alpha: %v", err)
	}
}

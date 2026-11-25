package main

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/subagent"
)

// The boot catalog must carry the five subagent_* tools whenever the
// runtime is wired and the profile allows them: buildTools reads the
// shell's runtime, so the wiring must happen BEFORE the generation build
// (reproduced 2026-11-16: a fresh boot had no subagent tools — they only
// appeared after a Runtime reload rebuilt the catalog post-wiring).
func TestBuildTools_SubagentToolsRegisteredWhenRuntimeWired(t *testing.T) {
	ctx := context.Background()

	skillsMgr, err := skills.NewManager(ctx, &config.SkillsConfig{Path: t.TempDir()}, zerolog.Nop())
	if err != nil {
		t.Fatalf("skills.NewManager: %v", err)
	}
	sc, err := skillcache.New()
	if err != nil {
		t.Fatalf("skillcache.New: %v", err)
	}
	defer func() { _ = sc.Close() }()

	enabled := true
	runtime := subagent.New(subagent.Deps{Enabled: true}, subagent.NewDefinitions(nil))

	cfg := &config.Config{
		BuiltinTools: config.BuiltinToolsConfig{
			Profiles: map[string]config.OwnerProfileConfig{
				rejectedChatProfileName: {Allow: []string{"*"}},
			},
		},
		Profiles: map[string]config.ProfileConfig{rejectedChatProfileName: {}},
		Subagents: config.SubagentsConfig{
			Enabled: &enabled,
		},
	}

	tools, mcpRuntime, blocked, cfgMgr := buildTools(ctx, cfg, skillsMgr, sc, true, rejectedChatProfileName, nil, zerolog.Nop(), config.ProfileOverrideInputs{}, runtime, nil)
	if mcpRuntime != nil {
		defer func() { _ = mcpRuntime.Close() }()
	}
	t.Logf("blocked tools: %d, config-manager wired: %v", len(blocked), cfgMgr != nil)

	got := map[string]bool{}
	for _, tl := range tools {
		if info, ierr := tl.Info(ctx); ierr == nil {
			got[info.Name] = true
		}
	}

	for _, name := range []string{"subagent_spawn", "subagent_send", "subagent_wait", "subagent_list", "subagent_interrupt"} {
		if !got[name] {
			t.Errorf("wired runtime: %s missing from the built catalog", name)
		}
	}
}

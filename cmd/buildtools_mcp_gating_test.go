package main

import (
	"context"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/rs/zerolog"
)

const mcpGetStateTool = "homeassistant/get_state"

// TestBuildTools_MCPServerProfileGating verifies that under the
// "default" profile, mcp_servers.home_assistant.profiles.default.deny hides
// the mock HA homeassistant/get_state tool, while under "edit" it remains exposed. This
// exercises the same per-owner gating path used for a real MCP server.
func TestBuildTools_MCPServerProfileGating(t *testing.T) {
	ctx := context.Background()

	skillsDir := t.TempDir()
	skillsMgr, err := skills.NewManager(ctx, &config.SkillsConfig{Path: skillsDir}, zerolog.Nop())
	if err != nil {
		t.Fatalf("skills.NewManager: %v", err)
	}

	skillCache, err := skillcache.New()
	if err != nil {
		t.Fatalf("skillcache.New: %v", err)
	}
	defer func() { _ = skillCache.Close() }()

	const (
		profileDefault = "default"
		profileEdit    = "edit"
	)

	baseCfg := &config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"home_assistant": {
				Profiles: map[string]config.OwnerProfileConfig{
					profileDefault: {Deny: []string{mcpGetStateTool}},
					profileEdit:    {Allow: []string{"*"}},
				},
			},
		},
		BuiltinTools: config.BuiltinToolsConfig{
			Profiles: map[string]config.OwnerProfileConfig{
				profileDefault: {},
				profileEdit:    {},
			},
		},
	}

	for _, tc := range []struct {
		profile      string
		expectDenied bool
	}{
		{profileDefault, true},
		{profileEdit, false},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			tools, runtime, blocked := buildTools(ctx, baseCfg, skillsMgr, skillCache, true, tc.profile, nil, zerolog.Nop())
			if tc.expectDenied {
				foundBlocked := false
				for _, b := range blocked {
					if b.Name == mcpGetStateTool && b.Owner == "home_assistant" {
						foundBlocked = true
					}
				}
				if !foundBlocked {
					t.Errorf("expected homeassistant/get_state in the blocked set under %q, got %v", tc.profile, blocked)
				}
			} else if len(blocked) != 0 {
				t.Errorf("expected empty blocked set under %q, got %v", tc.profile, blocked)
			}
			if runtime != nil {
				defer func() { _ = runtime.Close() }()
			}

			found := false
			for _, tl := range tools {
				info, err := tl.Info(ctx)
				if err != nil {
					continue
				}
				if info.Name == mcpGetStateTool {
					found = true
				}
			}

			if tc.expectDenied && found {
				t.Errorf("expected homeassistant/get_state to be denied under %q, but it was exposed", tc.profile)
			}
			if !tc.expectDenied && !found {
				t.Errorf("expected homeassistant/get_state to be exposed under %q, but it was not found", tc.profile)
			}
		})
	}
}

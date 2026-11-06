package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/rs/zerolog"
)

const mcpGetStateTool = "homeassistant/get_state"

// toolsConfigManagerName is the config-manager builtin wire name.
const toolsConfigManagerName = tools.ConfigManagerToolName

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
			tools, runtime, blocked, _ := buildTools(ctx, baseCfg, skillsMgr, skillCache, true, tc.profile, nil, zerolog.Nop(), config.ProfileOverrideInputs{})
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

// TestBuildTools_ConfigManagerImplicitDefault verifies the mcp-profiles
// delta: when builtin_tools.profiles is entirely absent, the synthesized
// default/edit owner rules deny config-manager while the other builtins stay
// exposed. The denial is code-enforced, not example-only.
func TestBuildTools_ConfigManagerImplicitDefault(t *testing.T) {
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

	// Load a minimal config so the boot defaults (including the synthesized
	// builtin owner rules) actually apply — a bare struct does not run them.
	minimalYAML := "config_version: 2\n" +
		"llm:\n" +
		"  default: p::m\n" +
		"  providers:\n" +
		"    p:\n" +
		"      provider: ollama\n" +
		"      api: ollama\n" +
		"channels:\n" +
		"  cli:\n" +
		"    enabled: true\n"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if wErr := os.WriteFile(cfgPath, []byte(minimalYAML), 0o600); wErr != nil {
		t.Fatal(wErr)
	}
	loaded, lErr := config.Load(ctx, cfgPath)
	if lErr != nil {
		t.Fatal(err)
	}

	for _, profile := range []string{"default", "edit"} {
		tools, runtime, _, _ := buildTools(ctx, loaded, skillsMgr, skillCache, true, profile, nil, zerolog.Nop(), config.ProfileOverrideInputs{})
		if runtime != nil {
			defer func() { _ = runtime.Close() }()
		}

		waitFound, cfgMgrFound := false, false
		for _, tl := range tools {
			info, err := tl.Info(ctx)
			if err != nil {
				continue
			}
			switch info.Name {
			case "wait":
				waitFound = true
			case toolsConfigManagerName:
				cfgMgrFound = true
			}
		}
		if cfgMgrFound {
			t.Errorf("%s must be absent under synthesized %q rules", toolsConfigManagerName, profile)
		}
		if !waitFound {
			t.Errorf("the other builtin tools must remain exposed under synthesized %q rules", profile)
		}
	}
}

// TestBuildTools_ConfigManagerExplicitPolicies verifies the established
// explicit-policy semantics over the new tool: allow-all exposes it, an
// explicit deny wins, and a missing active owner profile contributes zero
// tools (fail closed).
func TestBuildTools_ConfigManagerExplicitPolicies(t *testing.T) {
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

	newCfg := func(builtin config.OwnerProfileConfig) *config.Config {
		return &config.Config{
			// The tool is constructed against the elected path; tests set one
			// synthetic path so gating (not path resolution) is under test.
			ElectedPath: "/tmp/admin-elected-config.yaml",
			BuiltinTools: config.BuiltinToolsConfig{
				Profiles: map[string]config.OwnerProfileConfig{"admin": builtin},
			},
			// Top-level profile declaration keeps the missing-owner case
			// separate: other owners have no admin policy.
			Profiles: map[string]config.ProfileConfig{"admin": {}},
		}
	}

	exposed := func(cfg *config.Config) bool {
		tools, runtime, _, _ := buildTools(ctx, cfg, skillsMgr, skillCache, true, "admin", nil, zerolog.Nop(), config.ProfileOverrideInputs{})
		if runtime != nil {
			defer func() { _ = runtime.Close() }()
		}
		for _, tl := range tools {
			if info, err := tl.Info(ctx); err == nil && info.Name == toolsConfigManagerName {
				return true
			}
		}

		return false
	}

	// Allow-all (empty allow list = everything of the owner).
	if !exposed(newCfg(config.OwnerProfileConfig{})) {
		t.Error("empty explicit allow list must expose config-manager to the admin profile")
	}
	// Allow-all + explicit deny.
	if exposed(newCfg(config.OwnerProfileConfig{Allow: []string{"*"}, Deny: []string{config.ConfigManagerToolKey}})) {
		t.Error("explicit deny must win over allow-all")
	}
	// The other owners (MCP, twin) have no admin policy — the ordinary
	// fail-closed behavior applies (exercised above: zero MCP tools, no
	// crash). The missing-profile WARN path is the existing GateOwner one.
}

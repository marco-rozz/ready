//nolint:goconst
package twin

import (
	"context"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

func newTestTwin(t *testing.T) *hatwin.Twin {
	t.Helper()

	tw, err := hatwin.New(hatwin.Options{URL: "ws://ha.local/api/websocket", Token: "test-token"})
	if err != nil {
		t.Fatalf("hatwin.New: %v", err)
	}

	return tw
}

func TestBuildTools_DisabledReturnsNoTools(t *testing.T) {
	tw := newTestTwin(t)

	tools, _ := BuildTools(context.Background(), config.HomeAssistantConfig{Enabled: false}, tw, "default", false, zerolog.Nop())
	if len(tools) != 0 {
		t.Errorf("expected zero tools when disabled, got %d", len(tools))
	}

	tools, _ = BuildTools(context.Background(), config.HomeAssistantConfig{Enabled: true}, nil, "default", false, zerolog.Nop())
	if len(tools) != 0 {
		t.Errorf("expected zero tools when twin is nil, got %d", len(tools))
	}
}

func TestBuildTools_DefaultProfilesWhenBlockAbsent(t *testing.T) {
	tw := newTestTwin(t)

	tools, _ := BuildTools(context.Background(), config.HomeAssistantConfig{Enabled: true}, tw, "default", false, zerolog.Nop())
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools (ha_state_query + ha_command) under the built-in default, got %d", len(tools))
	}

	names := make(map[string]bool, len(tools))
	for _, tl := range tools {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info: %v", err)
		}
		names[info.Name] = true
	}
	if !names["ha_state_query"] || !names["ha_command"] {
		t.Fatalf("wire names missing: %v", names)
	}
}

func TestBuildTools_FailClosedWarnWhenExplicitProfileMissingTool(t *testing.T) {
	tw := newTestTwin(t)

	cfg := config.HomeAssistantConfig{
		Enabled: true,
		Profiles: map[string]config.OwnerProfileConfig{
			"default": {Allow: []string{}}, // declared, but empty allow means allow-all — use deny instead
		},
	}
	cfg.Profiles["default"] = config.OwnerProfileConfig{Deny: []string{queryToolName, commandToolName}}

	var logs strings.Builder
	logger := zerolog.New(&logs)

	tools, _ := BuildTools(context.Background(), cfg, tw, "default", false, logger)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools when both twin tools are explicitly denied, got %d", len(tools))
	}

	// "edit" profile is not declared at all here -> fail-closed WARN.
	tools, _ = BuildTools(context.Background(), cfg, tw, "edit", false, logger)
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools for undeclared profile (fail-closed), got %d", len(tools))
	}

	if !strings.Contains(logs.String(), "fail-closed") {
		t.Errorf("expected a fail-closed WARN, got: %s", logs.String())
	}
}

// TestBuildTools_DenyWriteToolKeepsReadTool pins the per-owner deny-rule
// restriction shape (twin-native-actuation D10): denying ha_command removes
// only the write tool.
func TestBuildTools_DenyWriteToolKeepsReadTool(t *testing.T) {
	tw := newTestTwin(t)

	cfg := config.HomeAssistantConfig{
		Enabled: true,
		Profiles: map[string]config.OwnerProfileConfig{
			"default": {Deny: []string{commandToolName}},
		},
	}

	tools, _ := BuildTools(context.Background(), cfg, tw, "default", false, zerolog.Nop())
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (ha_state_query) when ha_command is denied, got %d", len(tools))
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != queryToolName {
		t.Fatalf("surviving tool = %q, want ha_state_query", info.Name)
	}
}

// TestBuildTools_UnknownCommandDomainWarned pins the boot WARN for unknown
// domains in home_assistant.commands.domains (D5, task 5.2).
func TestBuildTools_UnknownCommandDomainWarned(t *testing.T) {
	tw := newTestTwin(t)

	cfg := config.HomeAssistantConfig{
		Enabled:  true,
		Commands: config.HACommandsConfig{Domains: []string{"light", "vacuum"}},
	}

	var logs strings.Builder
	logger := zerolog.New(&logs)

	_, _ = BuildTools(context.Background(), cfg, tw, "default", false, logger)

	if !strings.Contains(logs.String(), "vacuum") {
		t.Errorf("expected a WARN naming vacuum, got: %s", logs.String())
	}

	// The active set is literal: light active, siren not (not listed).
	active := tw.ActiveCommandDomains()
	if len(active) != 1 || active[0] != "light" {
		t.Fatalf("active domains = %v, want [light]", active)
	}
}

// TestBuildTools_CommandDefaults pins the clamping/validation of the
// commands block (D12).
func TestBuildTools_CommandDefaults(t *testing.T) {
	tw := newTestTwin(t)

	// Defaults: all-but-siren active; 5s verify; 20 targets.
	_, _ = BuildTools(context.Background(), config.HomeAssistantConfig{Enabled: true}, tw, "default", false, zerolog.Nop())
	active := tw.ActiveCommandDomains()
	found := map[string]bool{}
	for _, d := range active {
		found[d] = true
	}
	if !found["light"] || found["siren"] {
		t.Fatalf("default active domains wrong: %v", active)
	}
	if !tw.IsCommandDomainActive("cover") || tw.IsCommandDomainActive("lock") {
		t.Fatal("lock must never be active; cover must be")
	}
}

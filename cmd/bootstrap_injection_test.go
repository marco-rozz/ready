package main

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

// TestBuildBootstrapCfg_HomeIntegrationInjection pins the twin-native-actuation
// D11 injection rule: home-integration is merged into the bootstrap injection
// of every enabled channel when (and only when) home_assistant.enabled.
func TestBuildBootstrapCfg_HomeIntegrationInjection(t *testing.T) {
	base := config.Config{
		Channels: config.ChannelsConfig{
			CLI: config.CLIChannelConfig{Enabled: true},
		},
	}

	// HA disabled: no home-integration injection.
	cfg := base
	got := buildBootstrapCfg(&cfg, nil)
	if list := got["cli"]; strings.Contains(strings.Join(list, ","), "home-integration") {
		t.Fatalf("HA disabled must not inject home-integration: %v", list)
	}

	// HA enabled: home-integration injected even with no inject_skills.
	cfg = base
	cfg.HomeAssistant.Enabled = true
	got = buildBootstrapCfg(&cfg, nil)
	list := got["cli"]
	if !containsSkill(list, "home-integration") {
		t.Fatalf("HA enabled must inject home-integration: %v", list)
	}

	// Merged with profile inject_skills, no duplicates.
	cfg = base
	cfg.HomeAssistant.Enabled = true
	got = buildBootstrapCfg(&cfg, []string{"home-provisioning"})
	list = got["cli"]
	if !containsSkill(list, "home-provisioning") || !containsSkill(list, "home-integration") {
		t.Fatalf("injection must merge with profile inject_skills: %v", list)
	}
	if countSkill(list, "home-integration") != 1 {
		t.Fatalf("home-integration duplicated: %v", list)
	}
}

func containsSkill(list []string, name string) bool {
	return countSkill(list, name) > 0
}

func countSkill(list []string, name string) int {
	n := 0
	for _, s := range list {
		if s == name {
			n++
		}
	}

	return n
}

package profile

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// Repeated glob patterns and tool names, hoisted to constants to satisfy
// goconst and keep the test cases readable.
const (
	patTurnOn  = "*turn_on"
	patSetStar = "*set_*"
	patConfig  = "*config_*"
	patSetYaml = "*set_yaml"

	toolLightTurnOn   = "home_assistant__light/turn_on"
	toolClimateSet    = "home_assistant__climate/set_temperature"
	toolConfigSetHlpr = "home_assistant__ha_config_set_helper"
	toolConfigSetYaml = "home_assistant__ha_config_set_yaml"
	wireLightTurnOn   = "light/turn_on"
	wireGetState      = "get_state"
	toolGetState      = "home_assistant__get_state"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{patTurnOn, toolLightTurnOn, true},
		{patTurnOn, "home_assistant__light/turn_off", false},
		{patSetStar, toolClimateSet, true},
		{patSetStar, toolLightTurnOn, false},
		{patConfig, toolConfigSetHlpr, true},
		{patSetYaml, toolConfigSetYaml, true},
		{patSetYaml, "home_assistant__ha_set_yaml_x", false},
		{wireLightTurnOn, wireLightTurnOn, true},
		{wireLightTurnOn, "light/turn_off", false},
		// `*` must cross `/` (unlike path.Match).
		{"home_assistant__*", toolLightTurnOn, true},
		// multiple stars
		{"*light*on", toolLightTurnOn, true},
		{"*x*y*z", "axbycz", true},
		{"*x*y*z", "axbycw", false},
	}

	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestAllowed_DefaultProfileHidesStructureMutating(t *testing.T) {
	allow := []string{patTurnOn, "*turn_off", "*toggle", patSetStar, "*press", "*select_*"}
	deny := []string{patConfig, patSetYaml, "*manage_*"}

	if !Allowed(toolLightTurnOn, allow, deny) {
		t.Error("action tool light/turn_on should be allowed under default")
	}

	if Allowed(toolConfigSetHlpr, allow, deny) {
		t.Error("ha_config_set_helper should be hidden under default")
	}

	if Allowed(toolConfigSetYaml, allow, deny) {
		t.Error("ha_config_set_yaml should be hidden under default")
	}
}

func TestAllowed_EditProfileExposesAll(t *testing.T) {
	allow := []string{"*"}
	var deny []string

	for _, name := range []string{toolLightTurnOn, toolConfigSetHlpr, toolConfigSetYaml} {
		if !Allowed(name, allow, deny) {
			t.Errorf("edit profile should expose %q", name)
		}
	}
}

func TestAllowed_DenyWinsOverAllow(t *testing.T) {
	// set_* would allow it, but config_* denies it.
	allow := []string{patSetStar}
	deny := []string{patConfig}

	if Allowed(toolConfigSetHlpr, allow, deny) {
		t.Error("deny must take precedence over allow")
	}
}

func TestAllowed_EmptyAllowMeansAllowAll(t *testing.T) {
	if !Allowed("anything", nil, nil) {
		t.Error("empty allow with empty deny should allow all")
	}

	if Allowed("home_assistant__manage_x", nil, []string{"*manage_*"}) {
		t.Error("empty allow should still honour deny")
	}
}

// stubTool is a minimal tool.BaseTool whose Info() returns a fixed name.
type stubTool struct {
	name    string
	infoErr error
}

func (s stubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}

	return &schema.ToolInfo{Name: s.name}, nil
}

var _ tool.BaseTool = stubTool{}

func TestFilter_PatternMatchingOnPrefixedNames(t *testing.T) {
	tools := []tool.BaseTool{
		stubTool{name: toolLightTurnOn},
		stubTool{name: toolClimateSet},
		stubTool{name: toolConfigSetHlpr},
		stubTool{name: toolConfigSetYaml},
	}

	allow := []string{patTurnOn, patSetStar}
	deny := []string{patConfig}

	res := Filter(context.Background(), tools, allow, deny, zerolog.Nop())
	kept, names := res.Kept, res.Names

	if len(kept) != 2 {
		t.Fatalf("expected 2 kept tools, got %d (%v)", len(kept), names)
	}

	want := map[string]bool{
		toolLightTurnOn: true,
		toolClimateSet:  true,
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected tool kept: %q", n)
		}
	}
}

func TestFilter_FailOpenOnInfoError(t *testing.T) {
	tools := []tool.BaseTool{
		stubTool{name: "", infoErr: context.Canceled},
		stubTool{name: toolConfigSetYaml},
	}

	res := Filter(context.Background(), tools, []string{patTurnOn}, nil, zerolog.Nop())
	kept, names := res.Kept, res.Names

	// The errored tool is kept (fail-open); the config tool is filtered out.
	if len(kept) != 1 {
		t.Fatalf("expected 1 kept tool (fail-open), got %d", len(kept))
	}

	if len(names) != 0 {
		t.Errorf("errored tool should not appear in names list, got %v", names)
	}
}

// wireStubTool simulates an MCP-bridged tool: its LLM-facing display name
// (Info().Name) carries a server prefix, but gating must match on the
// unprefixed wire name.
type wireStubTool struct {
	displayName string
	wireName    string
}

func (s wireStubTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: s.displayName}, nil
}

func (s wireStubTool) WireName() string { return s.wireName }

var (
	_ tool.BaseTool = wireStubTool{}
	_ wireNamed     = wireStubTool{}
)

func TestFilter_MatchesWireNameNotDisplayName(t *testing.T) {
	tools := []tool.BaseTool{
		wireStubTool{displayName: toolLightTurnOn, wireName: wireLightTurnOn},
		wireStubTool{displayName: toolGetState, wireName: wireGetState},
	}

	// Patterns are unprefixed wire names — this would never match under the
	// old prefixed-name semantics.
	res := Filter(context.Background(), tools, nil, []string{wireGetState}, zerolog.Nop())
	kept, names := res.Kept, res.Names

	if len(kept) != 1 {
		t.Fatalf("expected 1 kept tool, got %d", len(kept))
	}

	if len(names) != 1 || names[0] != toolLightTurnOn {
		t.Errorf("expected kept display name %s, got %v", toolLightTurnOn, names)
	}
}

func TestGateOwner_FailClosedWhenProfileMissing(t *testing.T) {
	tools := []tool.BaseTool{stubTool{name: wireLightTurnOn}}
	profiles := map[string]config.OwnerProfileConfig{
		"edit": {Allow: []string{"*"}},
	}

	var logs strings.Builder
	logger := zerolog.New(&logs)

	kept, blocked := GateOwner(context.Background(), "home_assistant", tools, profiles, "default", false, logger)

	if len(kept) != 0 {
		t.Fatalf("expected zero tools (fail-closed), got %d", len(kept))
	}

	// Fail-closed owners contribute no disclosure data (mcp-profiles spec).
	if len(blocked) != 0 {
		t.Errorf("expected empty blocked set on fail-closed, got %v", blocked)
	}

	if !strings.Contains(logs.String(), "fail-closed") {
		t.Errorf("expected a fail-closed WARN, got log: %s", logs.String())
	}
}

func TestGateOwner_FiltersWhenProfileDeclared(t *testing.T) {
	tools := []tool.BaseTool{
		stubTool{name: wireLightTurnOn},
		stubTool{name: wireGetState},
	}
	profiles := map[string]config.OwnerProfileConfig{
		"default": {Deny: []string{wireGetState}},
	}

	kept, blocked := GateOwner(context.Background(), "home_assistant", tools, profiles, "default", false, zerolog.Nop())

	if len(kept) != 1 {
		t.Fatalf("expected 1 kept tool, got %d", len(kept))
	}

	// The blocked set must carry the removed tool with its LLM-facing name,
	// owner, and matched pattern.
	if len(blocked) != 1 {
		t.Fatalf("expected 1 blocked tool, got %v", blocked)
	}
	if blocked[0].Name != wireGetState || blocked[0].Owner != "home_assistant" || blocked[0].Pattern != wireGetState {
		t.Errorf("unexpected blocked tool: %+v", blocked[0])
	}
}

func TestValidateOwnerPatterns_WarnsOnDeadPattern(t *testing.T) {
	tools := []tool.BaseTool{stubTool{name: wireLightTurnOn}}
	rules := config.OwnerProfileConfig{
		Allow: []string{"light/turn_on", "climate/*"}, // climate/* matches nothing
		Deny:  []string{"nonexistent_tool"},           // matches nothing
	}

	var logs strings.Builder
	logger := zerolog.New(&logs)

	ValidateOwnerPatterns(context.Background(), "home_assistant", tools, rules, zerolog.WarnLevel, logger)

	out := logs.String()
	if strings.Count(out, "dead pattern") != 2 {
		t.Errorf("expected exactly 2 dead-pattern log lines, got: %s", out)
	}
}

func TestLogDecisions_EmitsOneEntryPerTool(t *testing.T) {
	tools := []tool.BaseTool{
		stubTool{name: wireLightTurnOn},
		stubTool{name: wireGetState},
	}
	rules := config.OwnerProfileConfig{Deny: []string{wireGetState}}

	var logs strings.Builder
	logger := zerolog.New(&logs).Level(zerolog.DebugLevel)

	LogDecisions(context.Background(), "home_assistant", tools, rules, logger)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 decision log entries, got %d: %v", len(lines), lines)
	}
}

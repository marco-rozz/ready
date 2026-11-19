package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	dTypeExplore = "explore"
	dTypeGP      = "general-purpose"
	dTypeCurator = "catalog-curator"
)

func TestDefinitionValidate(t *testing.T) {
	base := Definition{Name: dTypeExplore, Description: "read-only explorer", File: "x.md"}

	mk := func(mut func(d *Definition)) Definition {
		d := base
		mut(&d)

		return d
	}

	cases := []struct {
		name    string
		def     Definition
		wantErr bool
	}{
		{"minimal ok", base, false},
		{"both tool fields", mk(func(d *Definition) { d.Tools = []string{"*"}; d.DisallowedTools = []string{"wait"} }), true},
		{"allowlist ok", mk(func(d *Definition) { d.Tools = []string{"ha_state_query", "wait"} }), false},
		{"denylist ok", mk(func(d *Definition) { d.DisallowedTools = []string{"ha_command"} }), false},
		{"empty tools entry", mk(func(d *Definition) { d.Tools = []string{" "} }), true},
		{"empty disallowed entry", mk(func(d *Definition) { d.DisallowedTools = []string{""} }), true},
		{"bad name", mk(func(d *Definition) { d.Name = "Bad Name" }), true},
		{"reserved name", mk(func(d *Definition) { d.Name = AddressMain }), true},
		{"no description", mk(func(d *Definition) { d.Description = "  " }), true},
		{"bad visibility", mk(func(d *Definition) { d.Visibility = "secret" }), true},
		{"internal ok", mk(func(d *Definition) { d.Visibility = VisibilityInternal }), false},
		{"inherit model", mk(func(d *Definition) { d.Model = ModelInherit }), false},
		{"pinned model", mk(func(d *Definition) { d.Model = "ollama-gemma::gemma4:31b" }), false},
		{"model without ::", mk(func(d *Definition) { d.Model = "gemma4:31b" }), true},
		{"negative iterations", mk(func(d *Definition) { d.MaxIterations = -1 }), true},
		{"negative timeout", mk(func(d *Definition) { d.Timeout = -time.Second }), true},
		{"empty skill", mk(func(d *Definition) { d.Skills = []string{""} }), true},
	}

	for _, tc := range cases {
		verr := tc.def.validate()
		if tc.wantErr && verr == nil {
			t.Errorf("%s: expected error", tc.name)
		}

		if !tc.wantErr && verr != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, verr)
		}
	}
}

func TestLoadDefinition(t *testing.T) {
	dir := t.TempDir()

	okFile := filepath.Join(dir, "explore.md")
	body := "---\nname: " + dTypeExplore + "\ndescription: Read-only explorer\ntools:\n  - ha_state_query\nmax_iterations: 10\ntimeout: 2m\n---\n\nYou are a read-only explorer.\n"
	if err := os.WriteFile(okFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := LoadDefinition(okFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if d.Name != dTypeExplore || d.MaxIterations != 10 || d.Timeout != 2*time.Minute {
		t.Fatalf("parsed = %+v", d)
	}

	if !strings.Contains(d.Body, "read-only explorer.") {
		t.Fatalf("body = %q", d.Body)
	}

	if d.EffectiveModel() != ModelInherit {
		t.Fatalf("model default = %q", d.EffectiveModel())
	}

	if d.Internal() {
		t.Fatal("default visibility should be public")
	}

	// Reserved fields parse without failing.
	reserved := filepath.Join(dir, "reserved.md")
	reservedBody := "---\nname: reserved\ndescription: has reserved fields\nthinking: high\nmcp_servers: [x]\nisolation: worktree\nmemory: true\n---\nbody\n"
	if err := os.WriteFile(reserved, []byte(reservedBody), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadDefinition(reserved); err != nil {
		t.Fatalf("reserved fields should be accepted: %v", err)
	}

	// An invalid file errors naming the file.
	bad := filepath.Join(dir, "bad.md")
	badBody := "---\nname: bad\ndescription: d\ntools: [a]\ndisallowed_tools: [b]\n---\n"
	if err := os.WriteFile(bad, []byte(badBody), 0o600); err != nil {
		t.Fatal(err)
	}

	_, verr := LoadDefinition(bad)
	if verr == nil || !strings.Contains(verr.Error(), bad) {
		t.Fatalf("expected error naming the file, got %v", verr)
	}

	// Not closed frontmatter.
	unclosed := filepath.Join(dir, "unclosed.md")
	if err := os.WriteFile(unclosed, []byte("---\nname: unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, uerr := LoadDefinition(unclosed); uerr == nil {
		t.Fatal("expected unclosed frontmatter error")
	}
}

func TestLoadDefinitionsMissingDir(t *testing.T) {
	defs, err := LoadDefinitions(filepath.Join(t.TempDir(), "missing"))
	if err != nil || defs != nil {
		t.Fatalf("missing dir: %v, %v", defs, err)
	}
}

func TestDefinitionsIndex(t *testing.T) {
	idx := NewDefinitions([]Definition{
		{Name: dTypeExplore, Description: "e"},
		{Name: dTypeGP, Description: "g"},
		{Name: dTypeCurator, Description: "c", Visibility: VisibilityInternal},
	})

	if _, ok := idx.Get(dTypeExplore); !ok {
		t.Fatal("explore missing")
	}

	if _, ok := idx.Get("nope"); ok {
		t.Fatal("nope should be absent")
	}

	pub := idx.PublicTypes()
	if len(pub) != 2 {
		t.Fatalf("public types = %d", len(pub))
	}

	for _, p := range pub {
		if p.Internal() {
			t.Fatal("internal definition leaked into public types")
		}
	}

	def, ok := idx.DefaultType()
	if !ok || def.Name != dTypeGP {
		t.Fatalf("default type = %q ok=%v", def.Name, ok)
	}
}

func TestEffectiveMaxIterations(t *testing.T) {
	if (Definition{}).EffectiveMaxIterations() != DefaultMaxIterations {
		t.Fatal("default iterations")
	}

	if (Definition{MaxIterations: 7}).EffectiveMaxIterations() != 7 {
		t.Fatal("explicit iterations")
	}
}

// TestSentinelsDistinct keeps the sentinel set distinct — a sanity net used
// by the tools mapping errors to fixed messages.
func TestSentinelsDistinct(t *testing.T) {
	sentinels := []error{
		ErrDepthLimit, ErrConcurrencyLimit, ErrMailboxFull, ErrNameConflict,
		ErrUnknownAgent, ErrAgentEnded, ErrFeatureDisabled, ErrModelPinOutsideChain,
		ErrJobsCap, ErrParentDispatchEnded, ErrNotRunning, ErrDefinitionUnknown,
	}

	seen := map[string]bool{}
	for _, s := range sentinels {
		if seen[s.Error()] {
			t.Fatalf("duplicate sentinel text: %v", s)
		}

		seen[s.Error()] = true
	}
}

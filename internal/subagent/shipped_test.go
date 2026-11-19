package subagent

import "testing"

// TestShippedDefinitionsParse guards the release bundle: the shipped library
// definitions (resources/subagents/) must stay valid.
func TestShippedDefinitionsParse(t *testing.T) {
	defs, err := LoadDefinitions("../../resources/subagents")
	if err != nil {
		t.Fatalf("shipped definitions invalid: %v", err)
	}

	if len(defs) != 2 {
		t.Fatalf("shipped defs = %d", len(defs))
	}

	idx := NewDefinitions(defs)

	def, ok := idx.DefaultType()
	if !ok || def.Name != "general-purpose" {
		t.Fatalf("default = %q ok=%v", def.Name, ok)
	}

	ex, ok := idx.Get("explore")
	if !ok || len(ex.Tools) == 0 || ex.DisallowedTools != nil {
		t.Fatalf("explore = %+v", ex)
	}
}

//nolint:goconst
package toolnames

import "testing"

func TestResolver_ExactMatch_NoRewrite(t *testing.T) {
	r := NewResolver([]string{"ha_state_query", "home_assistant__light/turn_on"})

	if _, ok := r.Resolve("ha_state_query"); ok {
		t.Fatalf("exact match must not be rewritten")
	}
	if _, ok := r.Resolve("home_assistant__light/turn_on"); ok {
		t.Fatalf("exact match must not be rewritten")
	}
}

func TestResolver_StripBogusPrefix(t *testing.T) {
	r := NewResolver([]string{"ha_state_query", "home_assistant__light/turn_on"})

	// LLM invented a prefix on an unprefixed tool.
	got, ok := r.Resolve("home_assistant__ha_state_query")
	if !ok || got != "ha_state_query" {
		t.Fatalf("expected rewrite to ha_state_query, got %q ok=%v", got, ok)
	}

	// Wrong prefix on a prefixed tool: strip to wire name and rematch.
	got, ok = r.Resolve("ha__light/turn_on")
	if !ok || got != "home_assistant__light/turn_on" {
		t.Fatalf("expected wire-name rematch, got %q ok=%v", got, ok)
	}
}

func TestResolver_MissingPrefix_UniqueWire(t *testing.T) {
	r := NewResolver([]string{"home_assistant__light/turn_on"})

	got, ok := r.Resolve("light/turn_on")
	if !ok || got != "home_assistant__light/turn_on" {
		t.Fatalf("expected rewrite by wire, got %q ok=%v", got, ok)
	}
}

func TestResolver_AmbiguousWire_NoRewrite(t *testing.T) {
	r := NewResolver([]string{
		"home_assistant__turn_on",
		"other_server__turn_on",
	})

	if _, ok := r.Resolve("turn_on"); ok {
		t.Fatalf("ambiguous wire must not be rewritten")
	}
	if _, ok := r.Resolve("ha__turn_on"); ok {
		t.Fatalf("ambiguous strip+rematch must not be rewritten")
	}
}

func TestResolver_SuffixMatch(t *testing.T) {
	r := NewResolver([]string{"home_assistant__light/turn_on"})

	got, ok := r.Resolve("turn_on")
	if !ok || got != "home_assistant__light/turn_on" {
		t.Fatalf("expected suffix rewrite, got %q ok=%v", got, ok)
	}
}

func TestResolver_UnknownName_NoRewrite(t *testing.T) {
	r := NewResolver([]string{"ha_state_query"})

	if _, ok := r.Resolve("completely_unknown_tool"); ok {
		t.Fatalf("unknown name must not be rewritten")
	}
}

func TestResolver_Empty(t *testing.T) {
	r := NewResolver(nil)
	if _, ok := r.Resolve("anything"); ok {
		t.Fatalf("empty resolver must never rewrite")
	}

	var nilR *Resolver
	if _, ok := nilR.Resolve("x"); ok {
		t.Fatalf("nil resolver must never rewrite")
	}
	if nilR.Known("x") {
		t.Fatalf("nil resolver Known must be false")
	}
}

func TestResolver_Known(t *testing.T) {
	r := NewResolver([]string{"a", "b__c"})
	if !r.Known("a") || !r.Known("b__c") {
		t.Fatalf("Known must report registered names")
	}
	if r.Known("nope") {
		t.Fatalf("Known must reject unregistered names")
	}
}

func TestResolveBlocked_ExactGhostCall(t *testing.T) {
	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{
		{Name: "home_assistant__ha_get_history", Owner: "home_assistant"},
	})

	b, ok := r.ResolveBlocked("home_assistant__ha_get_history")
	if !ok {
		t.Fatal("exact blocked name must resolve")
	}
	if b.Name != "home_assistant__ha_get_history" || b.Owner != "home_assistant" {
		t.Errorf("unexpected blocked tool: %+v", b)
	}
}

func TestResolveBlocked_PrefixCorrectedGhostCall(t *testing.T) {
	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{
		{Name: "home_assistant__ha_get_history", Owner: "home_assistant"},
	})

	// Model hallucinated a wrong prefix over the blocked wire name.
	b, ok := r.ResolveBlocked("wrongserver__ha_get_history")
	if !ok {
		t.Fatal("prefix-corrected blocked name must resolve")
	}
	if b.Name != "home_assistant__ha_get_history" {
		t.Errorf("expected canonical blocked name, got %+v", b)
	}
}

func TestResolveBlocked_BareWireGhostCall(t *testing.T) {
	r := NewResolver([]string{"home_assistant__light/turn_on"})
	r.LoadBlocked([]BlockedTool{
		{Name: "home_assistant__ha_get_history", Owner: "home_assistant"},
	})

	b, ok := r.ResolveBlocked("ha_get_history")
	if !ok {
		t.Fatal("bare wire name of a unique blocked tool must resolve")
	}
	if b.Name != "home_assistant__ha_get_history" {
		t.Errorf("expected canonical blocked name, got %+v", b)
	}
}

func TestResolveBlocked_AmbiguousGhostStaysUnknown(t *testing.T) {
	r := NewResolver(nil)
	r.LoadBlocked([]BlockedTool{
		{Name: "server_a__get_state", Owner: "server_a"},
		{Name: "server_b__get_state", Owner: "server_b"},
	})

	// Two blocked tools share the wire name: bare form stays unknown.
	if _, ok := r.ResolveBlocked("get_state"); ok {
		t.Fatal("ambiguous bare wire ghost must stay unknown")
	}
	// The bogus-prefix form over the shared wire is equally ambiguous.
	if _, ok := r.ResolveBlocked("x__get_state"); ok {
		t.Fatal("ambiguous prefix-corrected ghost must stay unknown")
	}

	// Exact names still resolve.
	if _, ok := r.ResolveBlocked("server_a__get_state"); !ok {
		t.Fatal("exact blocked name must resolve even when wire is ambiguous")
	}
}

func TestResolveBlocked_SuffixGhostCall(t *testing.T) {
	r := NewResolver(nil)
	r.LoadBlocked([]BlockedTool{
		{Name: "home_assistant__light/turn_on", Owner: "home_assistant"},
	})

	b, ok := r.ResolveBlocked("turn_on")
	if !ok {
		t.Fatal("unique suffix ghost must resolve")
	}
	if b.Name != "home_assistant__light/turn_on" {
		t.Errorf("expected canonical blocked name, got %+v", b)
	}
}

func TestResolveBlocked_KnobOffNoIndex(t *testing.T) {
	// Without LoadBlocked (disclose_denied_tools off) nothing resolves.
	r := NewResolver([]string{"home_assistant__light/turn_on"})

	if _, ok := r.ResolveBlocked("home_assistant__ha_get_history"); ok {
		t.Fatal("blocked resolution must stay disabled without a loaded ghost index")
	}
}

func TestResolveBlocked_UnknownNameStaysUnknown(t *testing.T) {
	r := NewResolver([]string{"a"})
	r.LoadBlocked([]BlockedTool{{Name: "b__x", Owner: "b"}})

	if _, ok := r.ResolveBlocked("totally_unknown"); ok {
		t.Fatal("unrelated names must not resolve as blocked")
	}
}

func TestLoadBlocked_SkipsRegisteredNames(t *testing.T) {
	r := NewResolver([]string{"a"})
	r.LoadBlocked([]BlockedTool{{Name: "a", Owner: "x"}, {Name: "", Owner: "y"}})

	// A blocked name that is also registered is skipped; empty names skipped.
	if _, ok := r.ResolveBlocked("a"); ok {
		t.Fatal("registered name must not enter the ghost index")
	}
}

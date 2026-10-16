//nolint:goconst
package hatwin

import (
	"errors"
	"testing"
)

func startFilterTwin(t *testing.T) (*Twin, func()) {
	t.Helper()

	twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
	stop := func() {
		cancel()
		twin.Close()
	}

	return twin, stop
}

func entityIDsOf(snaps []EntitySnapshot) []string {
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.EntityID)
	}

	return out
}

func TestFilterStateCriteria(t *testing.T) {
	t.Parallel()

	twin, stop := startFilterTwin(t)
	defer stop()

	// Semantic on/off via the shared table (D8): cover off → closed.
	res, err := twin.Filter(FilterQuery{Domain: "cover", State: "off"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "cover.tenda" {
		t.Fatalf("cover off → %v, want [cover.tenda]", entityIDsOf(res))
	}

	// Exact.
	res, err = twin.Filter(FilterQuery{State: "21.5"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "sensor.temp" {
		t.Fatalf("exact state → %v, want [sensor.temp]", entityIDsOf(res))
	}

	// Multi-value any-of.
	res, err = twin.Filter(FilterQuery{State: []any{"21.5", "unavailable"}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("any-of → %v, want 2 entities", entityIDsOf(res))
	}

	// Numeric comparison: non-numeric states never match.
	res, err = twin.Filter(FilterQuery{State: map[string]any{">": 25.0}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("state > 25 → %v, want none (21.5 < 25; non-numeric excluded)", entityIDsOf(res))
	}

	res, err = twin.Filter(FilterQuery{State: map[string]any{"<": 25.0}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "sensor.temp" {
		t.Fatalf("state < 25 → %v, want [sensor.temp]", entityIDsOf(res))
	}
}

func TestFilterAttributesCriterion(t *testing.T) {
	t.Parallel()

	twin, stop := startFilterTwin(t)
	defer stop()

	// Equality on raw attributes, without verbose.
	res, err := twin.Filter(FilterQuery{Domain: "sensor", Attributes: map[string]any{"device_class": "temperature"}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "sensor.temp" {
		t.Fatalf("attribute equality → %v, want [sensor.temp]", entityIDsOf(res))
	}
	if res[0].Attributes != nil {
		t.Fatalf("non-verbose response carries attributes: %v", res[0].Attributes)
	}

	// Numeric comparison on attributes.
	res, err = twin.Filter(FilterQuery{Attributes: map[string]any{"brightness": map[string]any{">=": 100.0}}})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("numeric attribute → %v, want none", entityIDsOf(res))
	}

	// Unsupported comparison form fails fast with a guided error.
	_, err = twin.Filter(FilterQuery{Attributes: map[string]any{"brightness": map[string]any{"!=": 0.0}}})
	if !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("err = %v, want ErrInvalidFilter", err)
	}

	// AND composition.
	res, err = twin.Filter(FilterQuery{
		Domain:     "light",
		Attributes: map[string]any{"friendly_name": "Luce Cucina"},
		State:      "off",
	})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "light.cucina" {
		t.Fatalf("AND composition → %v, want [light.cucina]", entityIDsOf(res))
	}
}

func TestFilterNameDiacriticInsensitive(t *testing.T) {
	t.Parallel()

	twin, stop := startFilterTwin(t)
	defer stop()

	res, err := twin.Filter(FilterQuery{Name: "luce"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 4 { // Luce Cucina, Luce Salotto, Luce Spenta, Luce Morta (registry fallback)
		t.Fatalf("name=luce → %v, want 4", entityIDsOf(res))
	}
}

func TestFilterRegistryNameFallback(t *testing.T) {
	t.Parallel()

	// light.morta has no friendly_name attribute; its registry entry carries
	// "Luce Morta" — the name match must use the registry fallback.
	twin, stop := startFilterTwin(t)
	defer stop()

	res, err := twin.Filter(FilterQuery{Name: "morta"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].EntityID != "light.morta" {
		t.Fatalf("registry-name fallback → %v, want [light.morta]", entityIDsOf(res))
	}
}

func TestFilterDeterministicOrderAndSort(t *testing.T) {
	t.Parallel()

	twin, stop := startFilterTwin(t)
	defer stop()

	// Default order: entity_id ascending, stable across repeats.
	first, err := twin.Filter(FilterQuery{Domain: "light"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	second, err := twin.Filter(FilterQuery{Domain: "light"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	a, b := entityIDsOf(first), entityIDsOf(second)
	if len(a) == 0 {
		t.Fatal("no results")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("default order not stable: %v vs %v", a, b)
		}
	}
	for i := 1; i < len(a); i++ {
		if a[i-1] >= a[i] {
			t.Fatalf("default order not ascending by entity_id: %v", a)
		}
	}

	// State sort: numeric-aware, numbers before strings.
	res, err := twin.Filter(FilterQuery{State: map[string]any{"<": 1000.0}, Sort: "state"})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	got := entityIDsOf(res)
	if len(got) == 0 || got[0] != "sensor.temp" {
		t.Fatalf("state sort (numeric first) → %v, want sensor.temp first", got)
	}

	// Descending entity_id.
	res, err = twin.Filter(FilterQuery{Domain: "cover", Sort: "entity_id", SortDesc: true})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("cover desc → %v", entityIDsOf(res))
	}

	// Unknown sort key fails with guidance.
	_, err = twin.Filter(FilterQuery{Sort: "area"})
	if !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("err = %v, want ErrInvalidFilter", err)
	}
}

func TestFilterVerboseReturnsAttributes(t *testing.T) {
	t.Parallel()

	twin, stop := startFilterTwin(t)
	defer stop()

	res, err := twin.Filter(FilterQuery{EntityID: "sensor.temp", Verbose: true})
	if err != nil {
		t.Fatalf("Filter: %v", err)
	}
	if len(res) != 1 || res[0].Attributes == nil || res[0].Attributes["device_class"] != "temperature" {
		t.Fatalf("verbose attributes missing: %+v", res)
	}
}

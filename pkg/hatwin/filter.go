//nolint:goconst
package hatwin

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Filter returns all entities from the twin that match q, deterministically
// ordered (default entity_id ascending; see FilterQuery.Sort). The library
// API is unbounded — result limits are an adapter concern (D9).
// Returns ErrNotReady if the initial sync has not yet completed and
// ErrInvalidFilter for malformed criteria (unsupported comparison form,
// unknown sort key).
func (t *Twin) Filter(q FilterQuery) ([]EntitySnapshot, error) {
	if !t.ready.Load() {
		return nil, ErrNotReady
	}
	if q.Sort != "" && !validSortKey(q.Sort) {
		return nil, fmt.Errorf("%w: sort must be one of entity_id, name, state, last_changed (got %q)", ErrInvalidFilter, q.Sort)
	}
	if err := validateAttributeFilter(q.Attributes); err != nil {
		return nil, err
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	// Snapshots are built verbosely so the attributes criterion can evaluate
	// raw attributes regardless of the returned projection (D9); Attributes
	// are stripped at the end unless q.Verbose.
	var out []EntitySnapshot
	for entityID, state := range t.states {
		snap := t.buildSnapshot(entityID, state, true)
		if t.matchesQuery(q, snap, state) {
			out = append(out, snap)
		}
	}

	sortSnapshots(q, out, t)

	if !q.Verbose {
		for i := range out {
			out[i].Attributes = nil
		}
	}

	return out, nil
}

// GetState returns the EntitySnapshot for a single entity.
// Returns (zero, false, nil) when the entity is not found.
// Returns (zero, false, ErrNotReady) if the twin is not ready.
func (t *Twin) GetState(entityID string) (EntitySnapshot, bool, error) {
	if !t.ready.Load() {
		return EntitySnapshot{}, false, ErrNotReady
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	state, ok := t.states[entityID]
	if !ok {
		return EntitySnapshot{}, false, nil
	}

	return t.buildSnapshot(entityID, state, true), true, nil
}

// displayNameRaw returns the friendly name for an entity, falling back to
// the registry display name (D9 name-match source). Must be called under at
// least a read lock on t.mu.
func (t *Twin) displayNameRaw(entityID string, attrs map[string]any) string {
	if n := pickStr(attrs, "friendly_name"); n != "" {
		return n
	}
	if ent, ok := t.entities[entityID]; ok {
		if n := pickStr(ent, "name", "original_name"); n != "" {
			return n
		}
	}

	return ""
}

// buildSnapshot constructs an EntitySnapshot from raw state and registry data.
// Must be called under at least a read lock on t.mu.
func (t *Twin) buildSnapshot(entityID string, state map[string]any, verbose bool) EntitySnapshot {
	attrs := attrsOf(state)
	areaID := t.entityAreaID(entityID)

	snap := EntitySnapshot{
		EntityID:     entityID,
		Domain:       domainOf(entityID),
		State:        pickStr(state, "state"),
		DeviceClass:  pickStr(attrs, "device_class"),
		AreaID:       areaID,
		AreaName:     t.areaName(areaID),
		FriendlyName: t.displayNameRaw(entityID, attrs),
		LastChanged:  pickStr(state, "last_changed"),
		LastUpdated:  pickStr(state, "last_updated"),
	}
	if verbose {
		snap.Attributes = attrs
	}

	return snap
}

// matchesQuery returns true if snap satisfies all non-zero fields of q.
// rawState is the raw state object (attribute filters evaluate raw
// attributes regardless of verbose). Must be called under a read lock.
func (t *Twin) matchesQuery(q FilterQuery, snap EntitySnapshot, rawState map[string]any) bool {
	if q.Area != "" {
		lower := strings.ToLower(q.Area)
		if !strings.EqualFold(snap.AreaID, q.Area) &&
			!strings.Contains(strings.ToLower(snap.AreaName), lower) {
			return false
		}
	}
	if q.Domain != "" && snap.Domain != q.Domain {
		return false
	}
	if q.DeviceClass != "" && snap.DeviceClass != q.DeviceClass {
		return false
	}
	if q.EntityID != "" && !matchEntityPattern(q.EntityID, snap.EntityID) {
		return false
	}
	if q.OnlyProblem && !isProblem(snap.State) {
		return false
	}
	if q.State != nil && !stateMatches(q.State, snap) {
		return false
	}
	if len(q.Attributes) > 0 && !attributesMatch(q.Attributes, attrsOf(rawState)) {
		return false
	}
	if q.Name != "" && !nameMatches(q.Name, snap) {
		return false
	}

	return true
}

// domainOf extracts the HA domain from an entity_id (prefix before the first ".").
func domainOf(entityID string) string {
	if idx := strings.IndexByte(entityID, '.'); idx > 0 {
		return entityID[:idx]
	}

	return entityID
}

// attrsOf safely returns the attributes map from a raw state object.
func attrsOf(state map[string]any) map[string]any {
	if state == nil {
		return map[string]any{}
	}
	if attrs, ok := state["attributes"].(map[string]any); ok {
		return attrs
	}

	return map[string]any{}
}

// matchEntityPattern matches an entity_id against a glob pattern (when the
// pattern contains * or ?) or a case-sensitive substring otherwise.
func matchEntityPattern(pattern, entityID string) bool {
	if pattern == "" {
		return true
	}
	if strings.ContainsAny(pattern, "*?[") {
		matched, _ := filepath.Match(pattern, entityID)

		return matched
	}

	return strings.Contains(entityID, pattern)
}

// isProblem returns true when the entity state indicates a degraded condition.
func isProblem(state string) bool {
	return state == "unavailable" || state == "unknown"
}

// --- extended criteria (twin-native-actuation D9) ---

// stateMatches evaluates the `state` criterion: exact string, list (any of),
// semantic on/off via the shared table (D8), or a numeric comparison object
// ({">": 25}). A non-numeric state never matches a numeric comparison.
func stateMatches(spec any, snap EntitySnapshot) bool {
	switch v := spec.(type) {
	case string:
		return singleStateMatch(v, snap)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && singleStateMatch(s, snap) {
				return true
			}
		}

		return false
	case []string:
		for _, s := range v {
			if singleStateMatch(s, snap) {
				return true
			}
		}

		return false
	case map[string]any:
		op, val, ok := comparisonOf(v)
		if !ok {
			return false
		}

		return compareNumeric(snap.State, op, val)
	default:
		return false
	}
}

// singleStateMatch matches one state value: "on"/"off" resolve semantically
// per domain (D8); other values match literally.
func singleStateMatch(value string, snap EntitySnapshot) bool {
	if value == "on" || value == "off" {
		return matchesSemantic(snap.Domain, snap.State, value)
	}

	return strings.EqualFold(snap.State, value)
}

// comparisonOf extracts the single operator/value pair of a comparison
// object. Supported operators: > < >= <=.
func comparisonOf(m map[string]any) (op string, val float64, ok bool) {
	if len(m) != 1 {
		return "", 0, false
	}
	for k, v := range m {
		switch k {
		case ">", "<", ">=", "<=":
			op = k
		default:
			return "", 0, false
		}
		num, err := toFloat(v)
		if err != nil {
			return "", 0, false
		}

		return op, num, true
	}

	return "", 0, false
}

// toFloat converts common JSON numeric scalars (or numeric strings) to
// float64.
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	default:
		return 0, fmt.Errorf("not a number")
	}
}

// compareNumeric applies op to a state string and a numeric bound. A
// non-numeric state never matches.
func compareNumeric(state, op string, val float64) bool {
	num, err := strconv.ParseFloat(strings.TrimSpace(state), 64)
	if err != nil {
		return false
	}
	switch op {
	case ">":
		return num > val
	case "<":
		return num < val
	case ">=":
		return num >= val
	case "<=":
		return num <= val
	}

	return false
}

// validateAttributeFilter rejects unsupported comparison forms up front with
// a guided error (fail fast, not silently-no-match).
func validateAttributeFilter(attrs map[string]any) error {
	for name, spec := range attrs {
		switch v := spec.(type) {
		case string, bool, nil:
		case map[string]any:
			if _, _, ok := comparisonOf(v); !ok {
				return fmt.Errorf("%w: attribute %q uses an unsupported comparison form; supported: exact value (\"heating\"), numeric comparison object ({\">\": 25}, {\"<\": 5}, {\">=\": 0}, {\"<=\": 100})", ErrInvalidFilter, name)
			}
		default:
			return fmt.Errorf("%w: attribute %q uses an unsupported comparison form; supported: exact value (\"heating\"), numeric comparison object ({\">\": 25}, {\"<\": 5}, {\">=\": 0}, {\"<=\": 100})", ErrInvalidFilter, name)
		}
	}

	return nil
}

// attributesMatch evaluates the attributes criterion on raw attributes:
// equality (string/bool/number) or numeric comparison; entries AND-composed.
func attributesMatch(spec, attrs map[string]any) bool {
	for name, want := range spec {
		have, present := attrs[name]
		switch w := want.(type) {
		case map[string]any:
			op, val, ok := comparisonOf(w)
			if !ok {
				return false
			}
			haveNum, err := toFloat(have)
			if err != nil || !compareNumericFromFloat(haveNum, op, val) {
				return false
			}
		case nil:
			if present {
				return false
			}
		default:
			if !present || !looseEqual(have, w) {
				return false
			}
		}
	}

	return true
}

func compareNumericFromFloat(num float64, op string, val float64) bool {
	switch op {
	case ">":
		return num > val
	case "<":
		return num < val
	case ">=":
		return num >= val
	case "<=":
		return num <= val
	}

	return false
}

// looseEqual compares JSON scalars with numeric cross-type equality.
func looseEqual(a, b any) bool {
	if a == b {
		return true
	}
	if an, aerr := toFloat(a); aerr == nil {
		if bn, berr := toFloat(b); berr == nil {
			return an == bn
		}
	}

	return false
}

// nameMatches matches a case- and diacritic-insensitive substring against
// the friendly name (falling back to the registry name).
func nameMatches(query string, snap EntitySnapshot) bool {
	name := snap.FriendlyName
	if name == "" {
		return false
	}

	return strings.Contains(foldName(name), foldName(query))
}

// foldName lowercases and strips diacritics (NFD then removing Mn marks) so
// "caffè" matches "caffe". Uses golang.org/x/text (already a dependency).
func foldName(s string) string {
	decomposed := norm.NFD.String(strings.ToLower(s))
	var b strings.Builder
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}

	return b.String()
}

// --- ordering (D9) ---

// validSortKey reports whether key is a supported sort field.
func validSortKey(key string) bool {
	switch key {
	case "", "entity_id", "name", "state", "last_changed":
		return true
	}

	return false
}

// sortSnapshots orders snapshots in place: entity_id (default, asc), name,
// state (numeric-aware: numbers before strings), last_changed; asc/desc.
func sortSnapshots(q FilterQuery, snaps []EntitySnapshot, t *Twin) {
	key := q.Sort
	if key == "" {
		key = "entity_id"
	}

	sort.SliceStable(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		cmp := 0
		switch key {
		case "name":
			cmp = strings.Compare(foldName(t.displayName(a)), foldName(t.displayName(b)))
		case "state":
			cmp = compareStateValues(a.State, b.State)
		case "last_changed":
			cmp = strings.Compare(a.LastChanged, b.LastChanged)
		default:
			cmp = strings.Compare(a.EntityID, b.EntityID)
		}
		if q.SortDesc {
			return cmp > 0
		}

		return cmp < 0
	})
}

// displayName returns the friendly name (registry fallback already resolved
// into snapshots) for name sorting.
func (t *Twin) displayName(snap EntitySnapshot) string {
	if snap.FriendlyName != "" {
		return snap.FriendlyName
	}

	return snap.EntityID
}

// compareStateValues three-way compares two state strings, numerically when
// both parse as numbers, numbers before strings.
func compareStateValues(a, b string) int {
	an, aerr := strconv.ParseFloat(strings.TrimSpace(a), 64)
	bn, berr := strconv.ParseFloat(strings.TrimSpace(b), 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	case aerr == nil:
		return -1 // numbers before strings
	case berr == nil:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

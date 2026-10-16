//nolint:goconst
package hatwin

import (
	"fmt"
	"sort"
	"strings"
)

// TargetSpec selects the entities a command applies to: either explicit
// entity_ids, or a selector of a mandatory domain plus any of area, pattern,
// name (D6).
type TargetSpec struct {
	// Domain is mandatory for selectors, ignored with explicit EntityIDs.
	Domain string
	// Area matches the entity's area_id (exact, case-insensitive) or area
	// name (substring), like the read filter.
	Area string
	// Pattern is a glob/substring against entity_id.
	Pattern string
	// Name is a case- and diacritic-insensitive friendly-name substring.
	Name string
	// EntityIDs, when non-empty, selects explicit ids (validated; other
	// selector fields are ignored).
	EntityIDs []string
}

// resolveTargets resolves spec against the twin's registries (D6, in order):
// explicit-id validation, disabled-entity exclusion, group/member de-dup,
// unavailable reporting, max_targets cap, zero-match refusal. The returned
// slice is ordered by entity_id for determinism.
//
// The barrier is checked by the caller (ExecuteCommand) before arming the
// collector; resolution happens under one read lock so the resolved view is
// internally consistent.
func (t *Twin) resolveTargets(spec TargetSpec, maxTargets int) (targets, unavailable []string, err error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if len(spec.EntityIDs) > 0 {
		return t.resolveExplicitIDs(spec.EntityIDs)
	}

	if spec.Domain == "" {
		return nil, nil, fmt.Errorf("%w: pass a domain (e.g. \"light\") or explicit entity_ids; selectors without a domain are refused", ErrDomainRequired)
	}

	// Selector path.
	var matches []string
	for entityID := range t.states {
		if domainOf(entityID) != spec.Domain {
			continue
		}
		if ent, ok := t.entities[entityID]; ok && pickStr(ent, "disabled_by") != "" {
			// Registry-disabled entities are excluded from selector matches.
			continue
		}
		snap := t.buildSnapshot(entityID, t.states[entityID], false)
		sel := TargetSpec{Area: spec.Area, Pattern: spec.Pattern, Name: spec.Name}
		if !t.selectorMatches(sel, snap) {
			continue
		}
		matches = append(matches, entityID)
	}

	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("%w: no %s entity matches area=%q pattern=%q name=%q; check the spelling against ha_state_query results", ErrNoMatch, spec.Domain, spec.Area, spec.Pattern, spec.Name)
	}

	sort.Strings(matches)

	// Group de-duplication: drop a group entity when any of its members is
	// also selected (a group selected alone is kept).
	matchSet := make(map[string]bool, len(matches))
	for _, id := range matches {
		matchSet[id] = true
	}
	selected := make([]string, 0, len(matches))
	for _, id := range matches {
		if isGroupEntity(t.states[id]) && anyMemberSelected(t.states[id], matchSet) {
			continue
		}
		selected = append(selected, id)
	}

	// Unavailable targets are reported, not sent.
	var sendable []string
	for _, id := range selected {
		if pickStr(t.states[id], "state") == "unavailable" {
			unavailable = append(unavailable, id)

			continue
		}
		sendable = append(sendable, id)
	}

	if len(sendable) == 0 {
		return nil, unavailable, fmt.Errorf("%w: all %d matching %s entities are unavailable", ErrNoMatch, len(selected), spec.Domain)
	}

	if maxTargets > 0 && len(sendable) > maxTargets {
		return nil, unavailable, fmt.Errorf("%w: %d %s entities match, above the cap of %d; narrow the selector (area, pattern, name) or raise home_assistant.commands.max_targets",
			ErrOverCap, len(sendable), spec.Domain, maxTargets)
	}

	return sendable, unavailable, nil
}

// resolveExplicitIDs validates explicit entity ids against the twin. Any
// unknown id refuses the whole call (HA silently ignores unknown ids, so this
// is the only guard). Groups named explicitly are kept; unavailable ones are
// reported.
func (t *Twin) resolveExplicitIDs(ids []string) ([]string, []string, error) {
	var (
		sendable []string
		unavail  []string
		unknown  []string
	)
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true

		state, inStates := t.states[id]
		_, inRegistry := t.entities[id]
		if !inStates && !inRegistry {
			unknown = append(unknown, id)

			continue
		}
		if pickStr(state, "state") == "unavailable" {
			unavail = append(unavail, id)

			continue
		}
		sendable = append(sendable, id)
	}
	if len(unknown) > 0 {
		return nil, nil, fmt.Errorf("%w: %s not found in the twin; resolve entity ids with ha_state_query before commanding them", ErrUnknownEntity, strings.Join(unknown, ", "))
	}
	sort.Strings(sendable)

	return sendable, unavail, nil
}

// selectorMatches evaluates the non-domain selector fields against a
// snapshot. Must be called under at least a read lock.
func (t *Twin) selectorMatches(spec TargetSpec, snap EntitySnapshot) bool {
	if spec.Area != "" {
		lower := strings.ToLower(spec.Area)
		if !strings.EqualFold(snap.AreaID, spec.Area) &&
			!strings.Contains(strings.ToLower(snap.AreaName), lower) {
			return false
		}
	}
	if spec.Pattern != "" && !matchEntityPattern(spec.Pattern, snap.EntityID) {
		return false
	}
	if spec.Name != "" && !nameMatches(spec.Name, snap) {
		return false
	}

	return true
}

// isGroupEntity reports whether a raw state object carries a list of member
// entity ids (HA light/switch groups expose attributes.entity_id).
func isGroupEntity(state map[string]any) bool {
	members := groupMembersOf(state)

	return len(members) > 0
}

// groupMembersOf extracts attributes.entity_id as a string slice.
func groupMembersOf(state map[string]any) []string {
	attrs := attrsOf(state)
	raw, ok := attrs["entity_id"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}

	return out
}

// anyMemberSelected reports whether any member of the group's entity list is
// also in matchSet (members other than the group itself).
func anyMemberSelected(groupState map[string]any, matchSet map[string]bool) bool {
	for _, m := range groupMembersOf(groupState) {
		if m != groupSelfRef(groupState) && matchSet[m] {
			return true
		}
	}

	return false
}

// groupSelfRef returns the entity_id the group state belongs to, from the
// state object itself (HA includes entity_id in state objects).
func groupSelfRef(state map[string]any) string {
	return pickStr(state, "entity_id")
}

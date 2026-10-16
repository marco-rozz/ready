//nolint:goconst
package hatwin

import "strings"

// The semantic state table (twin-native-actuation D8): one shared, pure
// mapping giving per-domain meaning to the on/off shorthands and the
// expected state of actuation outcomes. It serves both the read filters
// (ha_state_query `state: on|off`) and the command evidence evaluation.
//
// Transitional states (opening, closing, …) belong to no side: they match
// neither on nor off, and own-context transitional events yield `pending`.

// transitionalStates are mid-flight states that are neither on nor off.
var transitionalStates = map[string]bool{ //nolint:gochecknoglobals // pure static table (D8)
	"opening":    true,
	"closing":    true,
	"buffering":  true,
	"connecting": true,
	"starting":   true,
	"stopping":   true,
}

// onOffDomains map the literal on/off states directly.
var onOffLiteralDomains = map[string]bool{ //nolint:gochecknoglobals // pure static table (D8)
	"light":         true,
	"switch":        true,
	"fan":           true,
	"input_boolean": true,
	"siren":         true,
	"humidifier":    true,
	"automation":    true,
	"binary_sensor": true,
}

// openClosedDomains map on→open, off→closed.
var openClosedDomains = map[string]bool{ //nolint:gochecknoglobals // pure static table (D8)
	"cover": true,
	"valve": true,
}

// mediaPlayerOffStates are the states treated as "off" for media_player.
var mediaPlayerOffStates = map[string]bool{ //nolint:gochecknoglobals // pure static table (D8)
	"off":     true,
	"standby": true,
}

// unavailableLikeStates never count as the expected state.
var unavailableLikeStates = map[string]bool{ //nolint:gochecknoglobals // pure static table (D8)
	"unavailable": true,
	"unknown":     true,
}

// isTransitionalState reports whether state is a mid-flight transitional
// state (neither on nor off).
func isTransitionalState(state string) bool {
	return transitionalStates[strings.ToLower(state)]
}

// matchesSemantic resolves the "on"/"off" shorthand for a domain against a
// concrete state (D8). Domains without a mapping match the literal value
// only; transitional states match neither.
func matchesSemantic(domain, state, semantic string) bool {
	state = strings.ToLower(state)
	if isTransitionalState(state) {
		return false
	}

	switch {
	case onOffLiteralDomains[domain]:
		return state == semantic
	case openClosedDomains[domain]:
		if semantic == "on" {
			return state == "open"
		}

		return state == "closed"
	case domain == "climate":
		if semantic == "off" {
			return state == "off"
		}

		return state != "off" && !unavailableLikeStates[state]
	case domain == "media_player":
		off := mediaPlayerOffStates[state] || unavailableLikeStates[state]
		if semantic == "off" {
			return off
		}

		return !off
	default:
		// Literal value only.
		return state == semantic
	}
}

// stateExpectation is the predicate an entity's new state must satisfy to
// count as evidence that a command was applied.
type stateExpectation func(state string) bool

// expectationState builds a predicate for a single expected literal state.
func expectationState(expected string) stateExpectation {
	return func(state string) bool {
		return strings.EqualFold(state, expected)
	}
}

// expectationToggle builds the inverse-of-current predicate for single-target
// toggles.
func expectationToggle(domain, current string) stateExpectation {
	if matchesSemantic(domain, current, "on") {
		return expectationState("off")
	}

	return expectationState("on")
}

// expectedStateForAction returns the expected post-command state predicate
// for a domain/action/params triple (D3/D8). ok=false means the action has no
// expected state (applied on any own-context event).
func expectedStateForAction(domain, action string, params map[string]any) (expect stateExpectation, ok bool) {
	switch domain {
	case "light", "switch", "fan", "siren", "input_boolean", "humidifier", "automation":
		switch action {
		case "on":
			return expectationState("on"), true
		case "off":
			return expectationState("off"), true
		case "set":
			// A parametric set implies the entity ends up on (brightness,
			// position-like attributes change but the entity is on).
			return expectationState("on"), true
		}

		return nil, false

	case "cover", "valve":
		switch action {
		case "open":
			return expectationState("open"), true
		case "close":
			return expectationState("closed"), true
		}

		return nil, false

	case "climate":
		switch action {
		case "off":
			return expectationState("off"), true
		case "set":
			if mode := pickStr(params, "hvac_mode"); mode != "" {
				return expectationState(mode), true
			}
			// A pure temperature change alters attributes, not the state.
			return nil, false
		}

		return nil, false

	case "media_player":
		switch action {
		case "on":
			return func(state string) bool {
				return matchesSemantic(domain, state, "on")
			}, true
		case "off":
			return func(state string) bool {
				return matchesSemantic(domain, state, "off")
			}, true
		case "play":
			return expectationState("playing"), true
		case "pause":
			return expectationState("paused"), true
		}

		return nil, false

	default:
		// scene activate, script run, button press, media next/previous,
		// cover stop: no expected state.
		return nil, false
	}
}

// isTransitional reports whether a state is transitional (pending evidence).
func isTransitional(state string) bool { return isTransitionalState(state) }

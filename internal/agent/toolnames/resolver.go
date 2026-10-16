// Package toolnames provides a defensive layer that normalises tool-call names
// emitted by any LLM before they reach Eino's toolsNode.
//
// Rationale: mixed naming schemes (some tools prefixed as
// "<owner>__<wire_name>", others exposed bare) cause even strong models to
// hallucinate. Concretely, the co-existence of "home_assistant__light/turn_on"
// (from the MCP server) and "ha_state_query" (from the twin) causes models to
// invent "home_assistant__ha_state_query", which fails with
// "tool ... not found in toolsNode indexes".
//
// This resolver is model-agnostic: it inspects the tool name emitted by the
// LLM and, when it does not match any registered name, applies a small set of
// deterministic normalisations (strip/add "<prefix>__", suffix match). On a
// unique match the name is rewritten; on ambiguity or no match the original
// name is preserved so the underlying error surfaces normally.
package toolnames

import "strings"

// Sep is the delimiter used between an owner prefix and the wire name in
// display tool names (e.g. "home_assistant__light/turn_on").
const Sep = "__"

// Resolver maps a possibly-hallucinated tool name to a registered one.
// It is safe for concurrent use once constructed (read-only).
type Resolver struct {
	// exact holds the set of canonical registered display names.
	exact map[string]struct{}
	// byWire indexes registered names by the part after the last "__" (the
	// wire name), used to add a missing prefix. Multi-value entries indicate
	// ambiguity and disable the rewrite for that wire.
	byWire map[string][]string
	// ghostExact indexes profile-blocked tools by LLM-facing display name.
	// Populated only when the profile's disclose_denied_tools knob is on.
	ghostExact map[string]BlockedTool
	// ghostByWire indexes blocked tools by wire name; multi-value entries
	// (two blocked tools sharing a wire name) stay unresolvable — ambiguous
	// ghost names remain unknown, exactly like ambiguous registered ones.
	ghostByWire map[string][]BlockedTool
}

// BlockedTool identifies one tool removed from the agent's surface by the
// active profile's gating rules. Name is the tool's LLM-facing display name
// (the prefixed form the model sees); Owner is the policy owner whose rules
// blocked it.
type BlockedTool struct {
	Name  string
	Owner string
}

// NewResolver builds a resolver from the list of registered display names
// (i.e. the names Eino's toolsNode indexes).
func NewResolver(registered []string) *Resolver {
	exact := make(map[string]struct{}, len(registered))
	byWire := make(map[string][]string, len(registered))

	for _, n := range registered {
		if n == "" {
			continue
		}
		exact[n] = struct{}{}

		wire := wirePart(n)
		byWire[wire] = append(byWire[wire], n)
	}

	return &Resolver{exact: exact, byWire: byWire}
}

// LoadBlocked populates the resolver's ghost index with the profile-blocked
// tools, enabling blocked-call classification (ResolveBlocked). When the
// disclose_denied_tools knob is off the caller never invokes this and the
// resolver behaves exactly as before. A blocked name that is also registered
// is skipped (gating removed it, so this indicates drift — the registered
// entry wins and the call flows through the normal path).
func (r *Resolver) LoadBlocked(blocked []BlockedTool) {
	if r == nil || len(blocked) == 0 {
		return
	}

	if r.ghostExact == nil {
		r.ghostExact = make(map[string]BlockedTool, len(blocked))
		r.ghostByWire = make(map[string][]BlockedTool)
	}

	for _, b := range blocked {
		if b.Name == "" {
			continue
		}
		if _, registered := r.exact[b.Name]; registered {
			continue
		}

		r.ghostExact[b.Name] = b

		wire := wirePart(b.Name)
		r.ghostByWire[wire] = append(r.ghostByWire[wire], b)
	}
}

// wirePart returns the substring after the last Sep occurrence, or the input
// unchanged when Sep is absent.
func wirePart(name string) string {
	if idx := strings.LastIndex(name, Sep); idx >= 0 {
		return name[idx+len(Sep):]
	}

	return name
}

// Resolve returns (canonical, true) when the requested name maps
// unambiguously to a registered display name (either directly or via a
// prefix strip/add). Returns ("", false) when the requested name is already
// registered as-is, when no candidate exists, or when the match is ambiguous.
//
// The boolean second return distinguishes "no rewrite needed" (unknown but
// unambiguous strategy failed) from "rewrite applied": callers should log the
// rewrite when true and leave the name untouched otherwise.
func (r *Resolver) Resolve(called string) (string, bool) {
	if called == "" || r == nil {
		return "", false
	}

	// Already registered: no rewrite.
	if _, ok := r.exact[called]; ok {
		return "", false
	}

	// Case 1: caller added a bogus prefix. Strip everything up to and
	// including the last "__" and see if the remainder is a unique
	// registered name (either exact or by wire lookup).
	if idx := strings.LastIndex(called, Sep); idx >= 0 {
		stripped := called[idx+len(Sep):]
		if _, ok := r.exact[stripped]; ok {
			return stripped, true
		}
		if candidates := r.byWire[stripped]; len(candidates) == 1 {
			return candidates[0], true
		}
	}

	// Case 2: caller omitted the prefix. Look up the wire name; if exactly
	// one registered name shares that wire, rewrite to it.
	if candidates := r.byWire[called]; len(candidates) == 1 {
		return candidates[0], true
	}

	// Case 3: caller passed a suffix of a registered name (e.g. "turn_on"
	// vs registered "home_assistant__light/turn_on"). Only rewrite on a
	// single unambiguous suffix match to avoid picking arbitrarily.
	var suffixMatches []string
	for name := range r.exact {
		if strings.HasSuffix(name, Sep+called) || strings.HasSuffix(name, "/"+called) {
			suffixMatches = append(suffixMatches, name)
		}
	}
	if len(suffixMatches) == 1 {
		return suffixMatches[0], true
	}

	return "", false
}

// Known returns true when the exact name is registered. Useful for tests and
// for callers that want to short-circuit without invoking Resolve.
func (r *Resolver) Known(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.exact[name]

	return ok
}

// ResolveBlocked classifies a called tool name against the profile-blocked
// (ghost) index, reusing Resolve's three unambiguous-match cases and the
// unique-only rule: (1) the caller added a bogus prefix — strip it and look
// the remainder up exactly or by wire; (2) the caller omitted the prefix —
// wire lookup; (3) the caller passed a suffix of a blocked name. Returns
// (blocked tool, true) only on a single unambiguous match; an ambiguous
// ghost name stays unknown so the call keeps the current failure behavior.
// Always returns false when the ghost index was never loaded (knob off).
func (r *Resolver) ResolveBlocked(called string) (BlockedTool, bool) {
	if r == nil || called == "" || r.ghostExact == nil {
		return BlockedTool{}, false
	}

	// Exact ghost match: the called name IS a blocked LLM-facing name.
	if b, ok := r.ghostExact[called]; ok {
		return b, true
	}

	// Case 1: caller added a bogus prefix over a blocked name.
	if idx := strings.LastIndex(called, Sep); idx >= 0 {
		stripped := called[idx+len(Sep):]
		if b, ok := r.ghostExact[stripped]; ok {
			return b, true
		}
		if candidates := r.ghostByWire[stripped]; len(candidates) == 1 {
			return candidates[0], true
		}
	}

	// Case 2: caller omitted the prefix.
	if candidates := r.ghostByWire[called]; len(candidates) == 1 {
		return candidates[0], true
	}

	// Case 3: caller passed a suffix of a blocked name.
	var suffixMatches []BlockedTool
	for name, b := range r.ghostExact {
		if strings.HasSuffix(name, Sep+called) || strings.HasSuffix(name, "/"+called) {
			suffixMatches = append(suffixMatches, b)
		}
	}
	if len(suffixMatches) == 1 {
		return suffixMatches[0], true
	}

	return BlockedTool{}, false
}

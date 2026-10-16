// Package profile implements READY-side MCP tool gating. Each policy owner
// (an MCP server, an internal Eino-tool package such as the HA digital twin,
// or the built-in tools bucket) filters its own tool slice through its own
// allow/deny glob rules for the active profile before the tools are
// registered with the agent.
//
// Gating is purely an exposure filter: it returns a subset of the input slice
// and never mutates wire names, prefixes, or routing. Patterns match the
// owner's own wire name (unprefixed), never the LLM-facing prefixed name, so
// rules are portable across tool_prefix configuration.
package profile

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// matchGlob reports whether name matches a glob pattern in which `*` is the
// only metacharacter and matches any (possibly empty) sequence of characters,
// including the `/` that appears in MCP wire names (e.g. "light/turn_on").
// This is deliberately not path.Match, whose `*` does not cross `/`.
func matchGlob(pattern, name string) bool {
	// Iterative wildcard match with backtracking on the last `*`.
	var (
		p, n         int // current indices into pattern, name
		star         = -1
		starMatchEnd int
	)

	for n < len(name) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star = p
			starMatchEnd = n
			p++
		case p < len(pattern) && pattern[p] == name[n]:
			p++
			n++
		case star >= 0:
			// Backtrack: let the last `*` absorb one more character.
			p = star + 1
			starMatchEnd++
			n = starMatchEnd
		default:
			return false
		}
	}

	// Consume any trailing `*` in the pattern.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}

	return p == len(pattern)
}

// Allowed reports whether a tool name passes the allow/deny rules. Deny takes
// precedence over allow. An empty allow list means "allow everything"; deny
// still applies.
func Allowed(name string, allow, deny []string) bool {
	for _, d := range deny {
		if matchGlob(d, name) {
			return false
		}
	}

	if len(allow) == 0 {
		return true
	}

	for _, a := range allow {
		if matchGlob(a, name) {
			return true
		}
	}

	return false
}

// decide returns the effect ("allow" or "deny") and the pattern responsible
// for it. When no pattern matched, the pattern is reported as "*(implicit)".
func decide(name string, allow, deny []string) (effect, pattern string) {
	for _, d := range deny {
		if matchGlob(d, name) {
			return "deny", d
		}
	}

	if len(allow) == 0 {
		return "allow", "*(implicit)"
	}

	for _, a := range allow {
		if matchGlob(a, name) {
			return "allow", a
		}
	}

	return "deny", "*(implicit)"
}

// wireNamed is implemented by tools that expose a distinct wire name distinct
// from their LLM-facing display name (currently *mcp.BridgedTool, whose
// display name may carry a server prefix). Gating always matches on the wire
// name so rules stay portable across tool_prefix configuration. Tools that
// don't implement this interface (built-in tools, the Twin's query tool) are
// matched on their Info().Name, which is already unprefixed.
type wireNamed interface {
	WireName() string
}

// toolName resolves the name a gating decision should match against: the
// wire name when the tool implements wireNamed, otherwise Info().Name.
func toolName(ctx context.Context, t tool.BaseTool) (name, displayName string, err error) {
	info, err := t.Info(ctx)
	if err != nil {
		return "", "", err
	}

	if wn, ok := t.(wireNamed); ok {
		return wn.WireName(), info.Name, nil
	}

	return info.Name, info.Name, nil
}

// BlockedTool describes one tool removed from the agent's surface by the
// active profile's gating rules. Name is the tool's LLM-facing display name
// (the prefixed form the model sees, e.g. "home_assistant__light/turn_on");
// Owner is the policy owner whose rules removed it; Pattern is the allow/deny
// glob responsible for the removal.
type BlockedTool struct {
	Name    string
	Owner   string
	Pattern string
}

// FilterResult carries the outcome of one owner's gating pass: the tools
// kept under the active profile (with their LLM-facing display names for
// boot-time logging) and the tools the profile removed. Filter leaves
// BlockedTool.Owner empty — it does not know the owner name; GateOwner
// stamps it before returning.
type FilterResult struct {
	Kept    []tool.BaseTool
	Names   []string
	Blocked []BlockedTool
}

// Filter returns the subset of tools that pass the allow/deny rules,
// preserving order, alongside the kept tools' LLM-facing display names (for
// boot-time logging) and the blocked set: the LLM-facing name and matched
// rule pattern of every tool the rules removed. Tools whose Info() call
// fails are kept (fail-open) and a warning is logged, so a transient
// introspection error never silently hides a tool.
func Filter(ctx context.Context, tools []tool.BaseTool, allow, deny []string, logger zerolog.Logger) FilterResult {
	result := FilterResult{
		Kept:  make([]tool.BaseTool, 0, len(tools)),
		Names: make([]string, 0, len(tools)),
	}

	for _, t := range tools {
		name, displayName, err := toolName(ctx, t)
		if err != nil {
			logger.Warn().Err(err).Msg("profile gating: tool Info() failed — keeping tool (fail-open)")
			result.Kept = append(result.Kept, t)

			continue
		}

		if Allowed(name, allow, deny) {
			result.Kept = append(result.Kept, t)
			result.Names = append(result.Names, displayName)

			continue
		}

		_, pattern := decide(name, allow, deny)
		result.Blocked = append(result.Blocked, BlockedTool{
			Name:    displayName,
			Pattern: pattern,
		})
	}

	return result
}

// ValidateOwnerPatterns logs one WARN (or, when level is Debug, one DEBUG
// entry) per allow/deny pattern in rules that matches zero tools in the
// owner's catalog ("dead pattern"). Intended to run once per owner at boot
// (WarnLevel) and again after an MCP reconnect (DebugLevel, since the
// operator is already aware of catalog drift from the reconnect log).
func ValidateOwnerPatterns(
	ctx context.Context,
	ownerName string,
	tools []tool.BaseTool,
	rules config.OwnerProfileConfig,
	level zerolog.Level,
	logger zerolog.Logger,
) {
	names := make([]string, 0, len(tools))

	for _, t := range tools {
		name, _, err := toolName(ctx, t)
		if err != nil {
			continue
		}

		names = append(names, name)
	}

	checkDead := func(patterns []string, kind string) {
		for _, p := range patterns {
			matched := false

			for _, n := range names {
				if matchGlob(p, n) {
					matched = true

					break
				}
			}

			if !matched {
				logger.WithLevel(level).
					Str("owner", ownerName).
					Str("pattern", p).
					Str("kind", kind).
					Msg("profile gating: pattern matches zero tools in owner catalog (dead pattern)")
			}
		}
	}

	checkDead(rules.Allow, "allow")
	checkDead(rules.Deny, "deny")
}

// LogDecisions emits one DEBUG log entry per tool naming the owner, wire
// name, matched pattern, and effect (allow/deny). Called only when
// logging.profile_decisions is true.
func LogDecisions(ctx context.Context, ownerName string, tools []tool.BaseTool, rules config.OwnerProfileConfig, logger zerolog.Logger) {
	for _, t := range tools {
		name, _, err := toolName(ctx, t)
		if err != nil {
			continue
		}

		effect, pattern := decide(name, rules.Allow, rules.Deny)
		logger.Debug().
			Str("owner", ownerName).
			Str("tool", name).
			Str("pattern", pattern).
			Str("effect", effect).
			Msg("profile gating: decision")
	}
}

// GateOwner resolves the active profile's rules for one policy owner from its
// profiles map and returns the filtered tool slice alongside the blocked set
// (LLM-facing name + owner + matched pattern) that the same pass removed.
// If the owner has not declared rules for activeProfileName at all, it fails
// closed: zero tools, one WARN naming the owner and the missing profile (and,
// when decisionLog is true, one DEBUG "fail-closed" entry per input tool), and
// an empty blocked set — a fail-closed owner contributes no disclosure data.
// Otherwise, it filters, runs ValidateOwnerPatterns at WarnLevel, and — when
// decisionLog is true — logs per-tool decisions. Gating decisions and boot
// logging are unchanged by the blocked-set return.
func GateOwner(
	ctx context.Context,
	ownerName string,
	tools []tool.BaseTool,
	profiles map[string]config.OwnerProfileConfig,
	activeProfileName string,
	decisionLog bool,
	logger zerolog.Logger,
) ([]tool.BaseTool, []BlockedTool) {
	rules, ok := profiles[activeProfileName]
	if !ok {
		logger.Warn().
			Str("owner", ownerName).
			Str("profile", activeProfileName).
			Str("outcome", "fail-closed").
			Msg("policy owner does not declare the active profile — exposing zero tools from this owner")

		if decisionLog {
			for _, t := range tools {
				name, _, err := toolName(ctx, t)
				if err != nil {
					continue
				}

				logger.Debug().
					Str("owner", ownerName).
					Str("tool", name).
					Str("effect", "fail-closed").
					Msg("profile gating: decision")
			}
		}

		return nil, nil
	}

	res := Filter(ctx, tools, rules.Allow, rules.Deny, logger)
	for i := range res.Blocked {
		res.Blocked[i].Owner = ownerName
	}

	ValidateOwnerPatterns(ctx, ownerName, tools, rules, zerolog.WarnLevel, logger)

	if decisionLog {
		LogDecisions(ctx, ownerName, tools, rules, logger)
	}

	return res.Kept, res.Blocked
}

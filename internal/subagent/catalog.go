// catalog.go — the child tool catalog (openspec change subagent-runtime,
// task 4.2, design D12): owner catalog (post-profile) → definition
// restriction (glob patterns on the wire name, same matcher as the profile
// allow/deny, applied after the profile filter, only restricting) →
// structural exclusions no definition can lift (the configuration-editing
// tools, secret entry, runtime reload, and — at the depth cap — every
// subagent_* tool).
package subagent

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/profile"
)

// structurallyExcludedToolNames are the tools a child NEVER inherits, under
// any definition: they would let a child mutate the daemon's configuration
// or handle credentials. Single-sourced here; the config-manager tool is
// the current member (secret entry and runtime reload are ZMQ control
// request types, never tools — the globs below keep the class honest if
// such tools ever appear).
//
//nolint:gochecknoglobals // the structural-exclusion single source
var structurallyExcludedToolNames = map[string]bool{
	"config_manager": true,
}

// structurallyExcludedToolGlobs catch the tool-name shapes of the same
// class.
//
//nolint:gochecknoglobals // the structural-exclusion single source
var structurallyExcludedToolGlobs = []string{
	"secret*",
	"oauth_login",
	"runtime_reload",
}

// subagentToolGlob matches the runtime's own tool surface.
const subagentToolGlob = "subagent_*"

// childCatalog applies the three restriction layers. atDepthCap strips the
// whole subagent_* surface (depth 1 default: children never nest). Dropped
// tool names are logged (the boot-time denied-tool logging of task 4.2).
func childCatalog(ctx context.Context, ownerTools []tool.BaseTool, def Definition, atDepthCap bool, logger zerolog.Logger) []tool.BaseTool {
	// Layer 1: the definition restriction via the profile matcher. The
	// definition fields are XOR-validated, so exactly one shape applies.
	// Both can only RESTRICT: an allow-list is an allowlist (deny wins
	// inside it), a deny-list without allow means allow-everything-except.
	var allow, deny []string
	if len(def.Tools) > 0 {
		allow = def.Tools
	}
	if len(def.DisallowedTools) > 0 {
		deny = def.DisallowedTools
	}

	kept := ownerTools
	if allow != nil || deny != nil {
		res := profile.Filter(ctx, kept, allow, deny, logger)
		kept = res.Kept
	}

	// Layer 2: structural exclusions by wire name.
	out := make([]tool.BaseTool, 0, len(kept))
	for _, t := range kept {
		name, err := toolNameOf(ctx, t)
		if err != nil || name == "" {
			continue // unresolvable tools never enter a child catalog
		}

		if structurallyExcluded(name) || (atDepthCap && profile.MatchGlob(subagentToolGlob, name)) {
			logger.Debug().Str("tool", name).Str("subagent", def.Name).
				Msg("tool excluded from a child catalog")

			continue
		}

		out = append(out, t)
	}

	return out
}

// structurallyExcluded reports a structural exclusion by name.
func structurallyExcluded(name string) bool {
	if structurallyExcludedToolNames[name] {
		return true
	}

	for _, g := range structurallyExcludedToolGlobs {
		if profile.MatchGlob(g, name) {
			return true
		}
	}

	return false
}

// toolNameOf resolves a tool's wire name (the gating decisions match
// against it).
func toolNameOf(ctx context.Context, t tool.BaseTool) (string, error) {
	info, err := t.Info(ctx)
	if err != nil || info == nil {
		return "", err
	}

	return info.Name, nil
}

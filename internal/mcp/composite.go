package mcp

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rs/zerolog"
)

// CompositeEntry groups a Runtime with its optional HealthMonitor and the
// pre-built tool list for that server. Callers populate this struct and pass a
// slice of entries to NewCompositeRuntime.
type CompositeEntry struct {
	Runtime Runtime
	Monitor *HealthMonitor
	Tools   []tool.BaseTool
}

// CompositeRuntime aggregates N MCP runtimes under the mcp.Runtime interface.
// It provides "any" semantics for Ping and Connected: the composite is
// considered connected when at least one entry is, and Ping succeeds when at
// least one entry responds. ToolCount is the sum of all entries. Close
// propagates to every runtime and stops all health monitors.
type CompositeRuntime struct {
	entries []CompositeEntry
	tools   []tool.BaseTool
	logger  zerolog.Logger
}

// Compile-time assertion that CompositeRuntime implements Runtime.
var _ Runtime = (*CompositeRuntime)(nil)

// NewCompositeRuntime builds a CompositeRuntime from the supplied entries.
// After aggregating the tool lists it runs collision detection (warns on
// duplicate display names) and returns the ready runtime.
func NewCompositeRuntime(ctx context.Context, entries []CompositeEntry, logger zerolog.Logger) *CompositeRuntime {
	var allTools []tool.BaseTool
	for _, e := range entries {
		allTools = append(allTools, e.Tools...)
	}

	cr := &CompositeRuntime{
		entries: entries,
		tools:   allTools,
		logger:  logger,
	}

	detectToolNameCollisions(ctx, allTools, logger)

	return cr
}

// StartMonitors starts the HealthMonitor (if any) for each entry, bound to
// the supplied context. Call this after NewCompositeRuntime, before the first
// agent turn.
func (cr *CompositeRuntime) StartMonitors(ctx context.Context) {
	for _, e := range cr.entries {
		if e.Monitor != nil {
			e.Monitor.Start(ctx)
		}
	}
}

// Tools returns the aggregated, ordered list of tools from all entries.
func (cr *CompositeRuntime) Tools() []tool.BaseTool {
	return cr.tools
}

// Close calls Close on every runtime and Stop on every health monitor.
// All errors are collected and returned as a single joined error.
func (cr *CompositeRuntime) Close() error {
	var errs []error

	// Monitors are stopped BEFORE their runtimes — a monitor still running
	// while its runtime closes would ping a closed connection.
	for _, e := range cr.entries {
		if e.Monitor != nil {
			e.Monitor.Stop()
		}
	}

	for _, e := range cr.entries {
		if err := e.Runtime.Close(); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Ping returns nil if at least one runtime responds successfully; returns an
// error only when every runtime fails. An empty composite always returns nil.
func (cr *CompositeRuntime) Ping(ctx context.Context) error {
	var lastErr error

	for _, e := range cr.entries {
		if err := e.Runtime.Ping(ctx); err == nil {
			return nil
		} else { //nolint:revive // intentional: capture lastErr before continuing
			lastErr = err
		}
	}

	return lastErr
}

// Connected returns true if at least one runtime reports a live connection.
func (cr *CompositeRuntime) Connected() bool {
	for _, e := range cr.entries {
		if e.Runtime.Connected() {
			return true
		}
	}

	return false
}

// ToolCount returns the sum of ToolCount() across all entries.
func (cr *CompositeRuntime) ToolCount() int {
	n := 0

	for _, e := range cr.entries {
		n += e.Runtime.ToolCount()
	}

	return n
}

// detectToolNameCollisions warns when two or more tools in the aggregated list
// share the same display name. Collisions do not block startup — the operator
// must resolve them via tool_prefix configuration.
func detectToolNameCollisions(ctx context.Context, tools []tool.BaseTool, logger zerolog.Logger) {
	seen := make(map[string]struct{}, len(tools))

	for _, t := range tools {
		info, err := t.Info(ctx)
		if err != nil {
			continue
		}

		if _, exists := seen[info.Name]; exists {
			logger.Warn().
				Str("tool", info.Name).
				Msg("duplicate tool display name detected in aggregated MCP tool list — consider using tool_prefix to disambiguate")
		} else {
			seen[info.Name] = struct{}{}
		}
	}
}

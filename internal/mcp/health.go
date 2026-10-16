package mcp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// HealthStatus is a snapshot of the MCP client connectivity.
type HealthStatus struct {
	Connected  bool
	LastPingAt time.Time
	ToolCount  int
	ErrorCount int
	Uptime     time.Duration
}

// HealthCheck is the read-only API exposed by the client to the monitor.
// Both *Client and *ReconnectingClient implement it.
type HealthCheck interface {
	Ping(ctx context.Context) error
	Connected() bool
	ToolCount() int
}

// Runtime is the minimal interface main.go uses to talk to either the simple
// *Client (mock path) or the production *ReconnectingClient. It bundles the
// tool list, the close hook and the HealthCheck surface so the wiring code
// does not branch on the concrete type.
type Runtime interface {
	HealthCheck
	Tools() []tool.BaseTool
	Close() error
}

// Compile-time assertions: both clients satisfy Runtime.
var (
	_ Runtime = (*Client)(nil)
	_ Runtime = (*ReconnectingClient)(nil)
)

// Ping verifies connectivity by issuing a ListTools call. It returns the first
// error encountered, classified via classifyError.
func (c *Client) Ping(ctx context.Context) error {
	pingCtx, cancel := withTimeout(ctx, c.timeout)
	defer cancel()

	_, err := c.session.ListTools(pingCtx, nil)

	return classifyError(err)
}

// Connected always reports true for *Client — the type has no reconnection
// state. Use ReconnectingClient for production deployments.
func (c *Client) Connected() bool { return c.session != nil }

// ToolCount returns the number of discovered tools.
func (c *Client) ToolCount() int { return len(c.tools) }

// Ping issues a ListTools call against the live underlying client. Disconnect
// state is propagated to the wrapper so the next CallTool fails fast.
func (rc *ReconnectingClient) Ping(ctx context.Context) error {
	rc.mu.RLock()
	sess := rc.session
	rc.mu.RUnlock()

	if sess == nil {
		return fmt.Errorf("%w: client is reconnecting", ErrMCPConnection)
	}

	pingCtx, cancel := withTimeout(ctx, rc.clientCfg.Timeout)
	defer cancel()

	if _, err := sess.ListTools(pingCtx, nil); err != nil {
		classified := classifyError(err)

		// See CallTool's identical guard (the design doc, task 6.7): without
		// this, health checking — enabled by default at 30s — closes the
		// session half a minute after the credential dies and undoes the
		// CallTool-side fix entirely.
		if shouldDropSession(classified) {
			rc.markDisconnected()
		}

		return classified
	}

	return nil
}

// ToolCount returns the size of the stable tool list.
func (rc *ReconnectingClient) ToolCount() int { return len(rc.tools) }

// HealthMonitor runs periodic Ping checks against an MCP client and, on
// failure, drives a Reconnector to restore connectivity. It tracks an error
// counter and an uptime baseline so operators can observe stability.
type HealthMonitor struct {
	client    HealthCheck
	reconnect Reconnector
	interval  time.Duration
	logger    zerolog.Logger
	started   time.Time
	// running tracks whether Start() launched the monitor loop
	// (criticalities-remediation M5: Stop before Start must not block).
	running    atomic.Bool
	lastPingAt atomic.Int64 // unix nano
	errorCount atomic.Int64

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// inAuthCondition tracks whether the last check found a permanent
	// authorization condition (the design doc/6.8), so check can log the
	// transition into and out of it once instead of on every tick.
	inAuthCondition atomic.Bool
}

// Reconnector is the subset of ReconnectingClient used by the monitor.
type Reconnector interface {
	Reconnect(ctx context.Context) error
}

// nopReconnector is used when no reconnect action is desired (e.g., tests).
type nopReconnector struct{}

func (nopReconnector) Reconnect(context.Context) error { return nil }

// NewHealthMonitor builds a monitor with the given health-check interval. Use
// nil reconnect to skip auto-reconnect on ping failure.
func NewHealthMonitor(client HealthCheck, reconnect Reconnector, interval time.Duration) *HealthMonitor {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	if reconnect == nil {
		reconnect = nopReconnector{}
	}

	return &HealthMonitor{
		client:    client,
		reconnect: reconnect,
		interval:  interval,
		logger:    zerolog.Nop(),
		started:   time.Now(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// WithLogger returns a copy of the monitor with the given logger set. The
// component=mcp label is applied automatically.
func (h *HealthMonitor) WithLogger(l zerolog.Logger) *HealthMonitor {
	h.logger = l.With().Str("component", "mcp").Logger()

	return h
}

// NewHealthMonitorFromConfig is a convenience constructor that reads the
// interval from the YAML config and disables the monitor when not enabled.
// Returns nil when health checks are disabled.
func NewHealthMonitorFromConfig(
	client HealthCheck,
	reconnect Reconnector,
	cfg config.MCPHealthCheckConfig,
) *HealthMonitor {
	if !cfg.Enabled {
		return nil
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	return NewHealthMonitor(client, reconnect, interval)
}

// Start runs the monitor loop in a new goroutine. It returns immediately.
func (h *HealthMonitor) Start(ctx context.Context) {
	h.running.Store(true)
	go h.run(ctx)
}

// Stop signals the monitor to terminate and waits for the goroutine to exit.
// criticalities-remediation (M5): safe to call before Start — construction-
// time error paths must be able to clean up without deadlock (the
// component-lifecycle spec's "Stop before Start is safe" requirement).
func (h *HealthMonitor) Stop() {
	h.stopOnce.Do(func() {
		close(h.stop)
	})

	if !h.running.Load() {
		return
	}

	<-h.done
}

// Status returns a snapshot of the monitor's view of the connection.
func (h *HealthMonitor) Status() HealthStatus {
	last := time.Time{}
	if v := h.lastPingAt.Load(); v > 0 {
		last = time.Unix(0, v)
	}

	return HealthStatus{
		Connected:  h.client.Connected(),
		LastPingAt: last,
		ToolCount:  h.client.ToolCount(),
		ErrorCount: int(h.errorCount.Load()),
		Uptime:     time.Since(h.started),
	}
}

func (h *HealthMonitor) run(ctx context.Context) {
	defer close(h.done)

	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stop:
			return
		case <-ticker.C:
			h.check(ctx)
		}
	}
}

// check executes a single health probe. On failure it increments the error
// counter, logs the failure, and drives the reconnect path — except for a
// permanent authorization condition (the design doc/6.8), where it deliberately
// does neither on every tick: Reconnect would immediately short-circuit on
// the same sentinel (task 6.10), and logging a WARN plus an ERROR every
// interval on a daemon that may route logging.file_path to disk would bury
// the one actionable message under thousands of duplicates a day. The ping
// itself is left to keep running: with the session preserved (CallTool and
// Ping's shouldDropSession guards), each ping consults the token source, so
// it is what detects recovery after an out-of-band `ready login` — the
// state transition is logged once in either direction (task 6.9).
func (h *HealthMonitor) check(ctx context.Context) {
	err := h.client.Ping(ctx)
	h.lastPingAt.Store(time.Now().UnixNano())

	if err == nil {
		h.logger.Trace().Int("tools", h.client.ToolCount()).Msg("MCP health check ok")

		if h.inAuthCondition.CompareAndSwap(true, false) {
			h.logger.Info().Msg("MCP health check recovered after a permanent authorization condition")
		}

		return
	}

	h.errorCount.Add(1)

	if isPermanentAuthCondition(err) {
		if h.inAuthCondition.CompareAndSwap(false, true) {
			h.logger.Warn().
				Str("error", err.Error()).
				Int64("error_count", h.errorCount.Load()).
				Msg("MCP health check failed: permanent authorization condition, run `ready login`; will keep pinging silently until it recovers")
		}

		return
	}

	h.inAuthCondition.Store(false)
	h.logger.Warn().
		Str("error", err.Error()).
		Int64("error_count", h.errorCount.Load()).
		Msg("MCP health check failed")

	if reconnectErr := h.reconnect.Reconnect(ctx); reconnectErr != nil {
		h.logger.Error().Str("error", reconnectErr.Error()).Msg("MCP reconnect failed")
	}
}

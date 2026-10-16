package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// ReconnectConfig holds the exponential-backoff reconnection policy.
type ReconnectConfig struct {
	MaxRetries     int           // 0 = infinite
	InitialBackoff time.Duration // e.g., 1s
	MaxBackoff     time.Duration // e.g., 60s
	BackoffFactor  float64       // e.g., 2.0
}

// DefaultReconnectConfig returns sensible defaults.
func DefaultReconnectConfig() ReconnectConfig {
	return ReconnectConfig{
		MaxRetries:     0,
		InitialBackoff: time.Second,
		MaxBackoff:     60 * time.Second,
		BackoffFactor:  2.0,
	}
}

// ReconnectingClient wraps an MCP client session and transparently
// reconnects on connection loss with exponential backoff. The tool list it
// exposes to the agent is stable across reconnects — each BridgedTool
// delegates CallTool to the ReconnectingClient itself, which routes to the
// live underlying session.
//
// httpClient and client are constructed once and are immutable for the
// lifetime of the ReconnectingClient (P2/D5): only the transport and the
// session are recreated on (re)connect. This split is what lets D1/D2 append
// a shared OAuthHandler later without a second lifecycle refactor.
type ReconnectingClient struct {
	serverCfg   *config.MCPServerConfig
	clientCfg   ClientConfig
	reconnectCf ReconnectConfig
	logger      zerolog.Logger
	serverName  string // configuration map key, used for logging
	toolPrefix  string // effective prefix for display names

	httpClient   *http.Client      // immutable: carries the header RoundTripper (P1)
	oauthHandler auth.OAuthHandler // immutable: nil unless the server declares an oauth block
	client       *mcp.Client       // immutable: constructed once

	mu      sync.RWMutex
	session *mcp.ClientSession // current live session; nil while disconnected
	tools   []tool.BaseTool    // built once on first connect

	connected   atomic.Bool
	onReconnect atomic.Pointer[func()]
}

// Compile-time assertion that ReconnectingClient satisfies toolInvoker so it
// can be passed to NewBridgedTool.
var _ toolInvoker = (*ReconnectingClient)(nil)

// NewReconnectingClient connects to the MCP server and returns a wrapper that
// will transparently reconnect on failure. The initial connect retries inline
// via ClientConfig; subsequent failures during operation are handled by the
// background reconnect goroutine.
// serverName is the config map key (e.g. "home_assistant"); toolPrefix is the
// effective prefix for tool display names (from MCPServerConfig.EffectiveToolPrefix).
func NewReconnectingClient(
	ctx context.Context,
	serverCfg *config.MCPServerConfig,
	clientCfg ClientConfig,
	reconnectCfg ReconnectConfig,
	serverName string,
	toolPrefix string,
) (*ReconnectingClient, error) {
	if clientCfg.Timeout == 0 {
		clientCfg.Timeout = defaultRequestTimeout
	}

	httpClient := clientCfg.HTTPClient
	if httpClient == nil {
		httpClient = NewScopedHTTPClient(serverCfg)
	}

	rc := &ReconnectingClient{
		serverCfg:    serverCfg,
		clientCfg:    clientCfg,
		reconnectCf:  reconnectCfg,
		logger:       clientCfg.Logger.With().Str("component", "mcp").Logger(),
		serverName:   serverName,
		toolPrefix:   toolPrefix,
		httpClient:   httpClient,
		oauthHandler: clientCfg.OAuthHandler,
	}
	rc.client = mcp.NewClient(&mcp.Implementation{Name: homeAgent, Version: "1.0.0"}, nil)

	session, err := connectSession(ctx, rc.client, newClientTransport(rc.serverCfg, rc.httpClient, rc.oauthHandler), clientCfg.Timeout)
	if err != nil {
		return nil, err
	}

	// List tools via the live session, then wrap them so they delegate
	// CallTool through rc (i.e., always hit the *current* connection).
	listCtx, cancel := withTimeout(ctx, clientCfg.Timeout)
	defer cancel()

	toolsResult, err := session.ListTools(listCtx, nil)
	if err != nil {
		_ = session.Close()

		return nil, fmt.Errorf("failed to list MCP tools: %w", classifyError(err))
	}

	tools := make([]tool.BaseTool, 0, len(toolsResult.Tools))
	for _, mcpTool := range toolsResult.Tools {
		displayName := buildDisplayName(rc.toolPrefix, mcpTool.Name)
		tools = append(tools, NewBridgedTool(rc.logger, rc, mcpTool, rc.serverName, displayName))
	}

	rc.logger.Info().Int("count", len(tools)).Str("server", rc.serverName).Msg("MCP tools discovered")

	for _, t := range toolsResult.Tools {
		rc.logger.Info().
			Str("name", t.Name).
			Str("description", t.Description).
			Int("params", len(schemaProperties(t.InputSchema))).
			Msg("MCP tool")
	}

	warnMissingExpectedTools(rc.logger, toolsResult.Tools, clientCfg.ExpectedTools)

	rc.session = session
	rc.tools = tools
	rc.connected.Store(true)

	return rc, nil
}

// Tools returns the discovered Eino tools. The slice is stable across reconnects.
func (rc *ReconnectingClient) Tools() []tool.BaseTool {
	return rc.tools
}

// Connected reports whether the wrapper currently holds a live connection.
func (rc *ReconnectingClient) Connected() bool {
	return rc.connected.Load()
}

// OnReconnect registers a callback fired after each successful reconnect.
// The callback runs synchronously on the reconnect goroutine — keep it fast.
func (rc *ReconnectingClient) OnReconnect(fn func()) {
	rc.onReconnect.Store(&fn)
}

// CallTool implements toolInvoker. It routes the request through the live
// underlying session. When the session is nil (disconnected), it returns
// ErrMCPConnection wrapped so the agent can surface a user-friendly message.
func (rc *ReconnectingClient) CallTool(
	ctx context.Context,
	params *mcp.CallToolParams,
) (*mcp.CallToolResult, error) {
	rc.mu.RLock()
	sess := rc.session
	rc.mu.RUnlock()

	if sess == nil {
		// criticalities-remediation (M10): with health_check disabled there
		// is no monitor to drive reconnection — a dropped session would
		// permanently brick this server's tools. Attempt ONE bounded inline
		// reconnect before surfacing the classified error.
		if rc.inlineReconnect() {
			if reconnectErr := rc.inlineReconnectAttempt(ctx); reconnectErr == nil {
				if sess2 := rc.currentSession(); sess2 != nil {
					result, retryErr := sess2.CallTool(ctx, params)
					if retryErr == nil {
						return result, nil
					}

					return nil, fmt.Errorf("%w: %s", ErrMCPConnection, retryErr.Error())
				}
			}
		}

		return nil, fmt.Errorf("%w: client is reconnecting", ErrMCPConnection)
	}

	result, err := sess.CallTool(ctx, params)
	if err != nil {
		classified := classifyError(err)

		// A permanent authorization failure does not mean the transport is
		// broken: drop the connection for every other error so
		// the next reconnect cycle picks it up, but leave it alone here so
		// the token source keeps being consulted on every subsequent call.
		if shouldDropSession(classified) {
			rc.markDisconnected()
		}

		return nil, classified
	}

	return result, nil
}

// inlineReconnect reports whether CallTool should attempt a bounded inline
// reconnect when the session is nil — i.e. when no health monitor is
// configured to drive reconnection (criticalities-remediation M10).
func (rc *ReconnectingClient) inlineReconnect() bool {
	return rc.serverCfg != nil && !rc.serverCfg.HealthCheck.Enabled
}

// currentSession returns the live session under the read lock (nil when
// disconnected).
func (rc *ReconnectingClient) currentSession() *mcp.ClientSession {
	rc.mu.RLock()
	defer rc.mu.RUnlock()

	return rc.session
}

// inlineReconnectAttempt performs a single bounded reconnect for the
// CallTool path: the full exponential-backoff loop is bypassed in favour of
// one attempt under a short deadline so a failed server does not turn the
// tool call into an unbounded block.
func (rc *ReconnectingClient) inlineReconnectAttempt(ctx context.Context) error {
	// Derived from the caller's context so cancellation aborts the attempt;
	// an explicit deadline replaces the (possibly absent) agent deadline.
	attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := rc.Reconnect(attemptCtx); err != nil {
		rc.logger.Warn().Err(err).Msg("MCP inline reconnect failed")

		return err
	}

	return nil
}

// Reconnect runs the exponential-backoff reconnect loop until a connection is
// established or ctx is cancelled / MaxRetries is exhausted. until a connection is
// established or ctx is cancelled / MaxRetries is exhausted. On success it
// rediscovers tools (logging any drift) and fires the OnReconnect callback.
// A new transport is built for every attempt; the immutable *mcp.Client and
// *http.Client are reused (P2).
func (rc *ReconnectingClient) Reconnect(ctx context.Context) error {
	policy := rc.reconnectCf

	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = time.Second
	}
	if policy.MaxBackoff <= 0 {
		policy.MaxBackoff = 60 * time.Second
	}
	if policy.BackoffFactor <= 1 {
		policy.BackoffFactor = 2.0
	}

	var lastErr error
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		session, err := connectSession(ctx, rc.client, newClientTransport(rc.serverCfg, rc.httpClient, rc.oauthHandler), rc.clientCfg.Timeout)
		if err == nil {
			if rediscoverErr := rc.rediscoverAndSwap(ctx, session); rediscoverErr != nil {
				_ = session.Close()
				lastErr = rediscoverErr
			} else {
				rc.logger.Info().Int("attempt", attempt+1).Msg("MCP reconnect succeeded")

				if cb := rc.onReconnect.Load(); cb != nil && *cb != nil {
					(*cb)()
				}

				return nil
			}
		} else {
			lastErr = err

			// Both permanent-authorization sentinels short-circuit with a
			// distinct message (the design doc/6.10): neither fixes itself with
			// retries, and for ErrMCPOAuthLoginRequired specifically, this
			// path only runs at (re)connect time — the running-session case
			// is handled by shouldDropSession, which never calls Reconnect
			// in the first place.
			switch {
			case errors.Is(err, ErrMCPAuth):
				rc.logger.Error().Msg("MCP reconnect aborted: authentication failed")

				return err
			case errors.Is(err, ErrMCPOAuthLoginRequired):
				rc.logger.Error().Msg("MCP reconnect aborted: no usable oauth credential, run `ready login`")

				return err
			}
		}

		attempt++
		rc.logger.Warn().
			Int("attempt", attempt).
			Str("error", lastErr.Error()).
			Msg("MCP reconnect attempt failed")

		if policy.MaxRetries > 0 && attempt >= policy.MaxRetries {
			return fmt.Errorf("MCP reconnect exhausted after %d attempts: %w", attempt, lastErr)
		}

		backoff := computeBackoff(policy, attempt)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// rediscoverAndSwap lists tools on the new session, logs any catalog drift
// relative to the original tool list, then atomically swaps the live
// session. The swap happens under mu.Lock(); the previous session is closed
// outside the lock (P2).
func (rc *ReconnectingClient) rediscoverAndSwap(ctx context.Context, newSession *mcp.ClientSession) error {
	listCtx, cancel := withTimeout(ctx, rc.clientCfg.Timeout)
	defer cancel()

	toolsResult, err := newSession.ListTools(listCtx, nil)
	if err != nil {
		return fmt.Errorf("failed to list MCP tools after reconnect: %w", classifyError(err))
	}

	rc.logToolDrift(toolsResult.Tools)

	rc.mu.Lock()
	old := rc.session
	rc.session = newSession
	rc.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}

	rc.connected.Store(true)

	return nil
}

// logToolDrift compares the current tool catalog against the new one and emits
// a warning per added/removed entry. The live tool list itself is not mutated.
func (rc *ReconnectingClient) logToolDrift(newTools []*mcp.Tool) {
	have := make(map[string]struct{}, len(rc.tools))
	for _, t := range rc.tools {
		info, err := t.Info(context.Background())
		if err != nil {
			continue
		}
		have[info.Name] = struct{}{}
	}

	now := make(map[string]struct{}, len(newTools))
	for _, t := range newTools {
		now[t.Name] = struct{}{}
	}

	for name := range now {
		if _, ok := have[name]; !ok {
			rc.logger.Warn().Str("name", name).Msg("MCP tool appeared after reconnect (agent still uses original catalog)")
		}
	}
	for name := range have {
		if _, ok := now[name]; !ok {
			rc.logger.Warn().Str("name", name).Msg("MCP tool disappeared after reconnect")
		}
	}
}

// markDisconnected flips the connected flag and clears the live session so
// the next CallTool fails fast and the health monitor / explicit caller can
// trigger Reconnect. The swap happens under mu.Lock(); Close runs outside it.
func (rc *ReconnectingClient) markDisconnected() {
	rc.connected.Store(false)

	rc.mu.Lock()
	old := rc.session
	rc.session = nil
	rc.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
}

// Close releases the live session and prevents further use.
func (rc *ReconnectingClient) Close() error {
	rc.connected.Store(false)

	rc.mu.Lock()
	sess := rc.session
	rc.session = nil
	rc.mu.Unlock()

	if sess == nil {
		return nil
	}

	return sess.Close()
}

// computeBackoff returns the delay for the given attempt (1-based) clamped to
// [InitialBackoff, MaxBackoff].
func computeBackoff(p ReconnectConfig, attempt int) time.Duration {
	if attempt <= 0 {
		return p.InitialBackoff
	}

	factor := math.Pow(p.BackoffFactor, float64(attempt-1))
	delay := time.Duration(float64(p.InitialBackoff) * factor)

	if delay > p.MaxBackoff {
		return p.MaxBackoff
	}

	if delay < p.InitialBackoff {
		return p.InitialBackoff
	}

	return delay
}

// ReconnectConfigFromConfig translates the YAML config struct to the runtime
// ReconnectConfig, applying defaults to fields left at zero.
func ReconnectConfigFromConfig(cfg config.MCPReconnectConfig) ReconnectConfig {
	out := DefaultReconnectConfig()

	if cfg.MaxRetries != 0 {
		out.MaxRetries = cfg.MaxRetries
	}
	if cfg.InitialBackoff > 0 {
		out.InitialBackoff = cfg.InitialBackoff
	}
	if cfg.MaxBackoff > 0 {
		out.MaxBackoff = cfg.MaxBackoff
	}
	if cfg.BackoffFactor > 0 {
		out.BackoffFactor = cfg.BackoffFactor
	}

	return out
}

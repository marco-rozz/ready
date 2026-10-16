package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// ErrMCPConnection is returned when the MCP server is unreachable.
var ErrMCPConnection = errors.New("MCP connection failed")

// defaultRequestTimeout bounds a single MCP request when the caller did not
// configure one via ClientConfig.Timeout.
const defaultRequestTimeout = 30 * time.Second

const homeAgent = "home-agent"

// ClientConfig holds configuration for the MCP client.
type ClientConfig struct {
	// MaxRetries is the maximum number of connection attempts.
	MaxRetries int
	// RetryBackoff is the initial backoff duration between retries.
	RetryBackoff time.Duration
	// Timeout bounds individual MCP requests (Connect, ListTools, CallTool).
	// Zero means defaultRequestTimeout.
	Timeout time.Duration
	// ExpectedTools, if non-empty, triggers a warning log entry for each name
	// in the list that is not present after discovery. Discovery never fails
	// because of a missing expected tool.
	ExpectedTools []string
	// Logger is used for connection and discovery log lines. Zero value is silent.
	Logger zerolog.Logger
	// HTTPClient, when non-nil, is used for every HTTP request issued on
	// behalf of the server instead of building one internally. Callers that
	// need to share a single origin-scoped client across multiple consumers
	// (e.g. mcp-oauth-authorization's discovery/registration/token requests)
	// construct it once via NewScopedHTTPClient and pass it here. Nil
	// preserves today's behaviour: a client is built internally.
	HTTPClient *http.Client
	// OAuthHandler, when non-nil, is threaded into the Streamable HTTP
	// transport so it can authorize requests via OAuth 2.0
	// (mcp-oauth-authorization the design doc). It has no effect on the SSE
	// transport: mcp.SSEClientTransport exposes no OAuthHandler field, and
	// config validation rejects an oauth block on transport: sse before this
	// is ever reached. Nil preserves today's unauthenticated/static-header
	// behaviour.
	OAuthHandler auth.OAuthHandler
}

// DefaultClientConfig returns sensible defaults for the MCP client.
func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		MaxRetries:   3,
		RetryBackoff: time.Second,
		Timeout:      defaultRequestTimeout,
	}
}

// Client wraps an MCP client session and provides Eino-compatible tools.
type Client struct {
	session     *mcp.ClientSession
	closeServer func() error // non-nil only for in-process clients (P4): closes the server-side session
	tools       []tool.BaseTool
	logger      zerolog.Logger
	timeout     time.Duration
}

// NewClient connects to an MCP server and discovers available tools.
// It converts MCP tools to Eino BaseTool instances via the Tool bridge.
// Pass a logger via opts to enable log output; omit for a silent client.
func NewClient(ctx context.Context, cfg *config.MCPServerConfig, opts ...ClientConfig) (*Client, error) {
	clientCfg := DefaultClientConfig()
	if len(opts) > 0 {
		clientCfg = opts[0]
	}

	if clientCfg.Timeout == 0 {
		clientCfg.Timeout = defaultRequestTimeout
	}

	logger := clientCfg.Logger

	var session *mcp.ClientSession
	var err error

	// Retry logic for connection.
	for attempt := range clientCfg.MaxRetries {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ErrMCPConnection, ctx.Err())
		default:
		}

		session, err = connectToServer(ctx, cfg, clientCfg.Timeout, clientCfg.HTTPClient, clientCfg.OAuthHandler)
		if err == nil {
			break
		}

		// Authentication and authorization errors are non-retryable: no
		// number of retries fixes rejected static credentials or a missing
		// OAuth login (the design doc/6.11 — the two "permanent" sentinels must
		// not diverge).
		if isPermanentAuthCondition(err) {
			return nil, err
		}

		logger.Warn().
			Int("attempt", attempt+1).
			Int("max_retries", clientCfg.MaxRetries).
			Str("error", err.Error()).
			Msg("MCP connection attempt failed")

		if attempt < clientCfg.MaxRetries-1 {
			backoff := clientCfg.RetryBackoff * time.Duration(attempt+1)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("%w: %w", ErrMCPConnection, ctx.Err())
			case <-time.After(backoff):
			}
		}
	}

	if err != nil {
		return nil, fmt.Errorf("%w: after %d attempts: %w", ErrMCPConnection, clientCfg.MaxRetries, err)
	}

	tools, rawTools, err := discoverTools(ctx, session, clientCfg.Timeout, logger, "", "")
	if err != nil {
		return nil, err
	}

	warnMissingExpectedTools(logger, rawTools, clientCfg.ExpectedTools)

	return &Client{
		session: session,
		tools:   tools,
		logger:  logger,
		timeout: clientCfg.Timeout,
	}, nil
}

// WithLogger returns a copy of cfg with the given logger set.
// Use it when constructing a ClientConfig for NewClient.
func (cfg ClientConfig) WithLogger(l zerolog.Logger) ClientConfig {
	cfg.Logger = l

	return cfg
}

// discoverTools lists MCP tools and converts them to Eino BaseTool instances.
// serverName is the configuration key of the server (used for logging and the
// server field on each BridgedTool). toolPrefix is the effective prefix from
// MCPServerConfig.EffectiveToolPrefix; an empty string means no prefix.
// Each tool is logged at info level (name + description) so operators can see
// the catalog at startup. Schema details are logged at debug level.
func discoverTools(
	ctx context.Context,
	session *mcp.ClientSession,
	timeout time.Duration,
	logger zerolog.Logger,
	serverName string,
	toolPrefix string,
) ([]tool.BaseTool, []*mcp.Tool, error) {
	listCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	toolsResult, err := session.ListTools(listCtx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list MCP tools: %w", classifyError(err))
	}

	einoTools := make([]tool.BaseTool, 0, len(toolsResult.Tools))
	for _, mcpTool := range toolsResult.Tools {
		displayName := buildDisplayName(toolPrefix, mcpTool.Name)
		einoTools = append(einoTools, NewBridgedTool(logger, session, mcpTool, serverName, displayName))
	}

	logger.Info().Int("count", len(einoTools)).Str("server", serverName).Msg("MCP tools discovered")

	for _, t := range toolsResult.Tools {
		logger.Info().
			Str("name", t.Name).
			Str("description", t.Description).
			Int("params", len(schemaProperties(t.InputSchema))).
			Msg("MCP tool")
	}

	return einoTools, toolsResult.Tools, nil
}

// warnMissingExpectedTools logs a warning for every expected tool name not
// found in the discovered set. Comparison is done against wire names (the
// original MCP tool names without any prefix) so that expected_tools in config
// remains stable regardless of the tool_prefix setting.
// It never fails — HA tool catalogs change across versions and missing tools
// should not block startup.
func warnMissingExpectedTools(logger zerolog.Logger, rawTools []*mcp.Tool, expected []string) {
	if len(expected) == 0 {
		return
	}

	have := make(map[string]struct{}, len(rawTools))
	for _, t := range rawTools {
		have[t.Name] = struct{}{}
	}

	for _, name := range expected {
		if _, ok := have[name]; !ok {
			logger.Warn().Str("name", name).Msg("expected MCP tool not exposed by server")
		}
	}
}

// withTimeout returns a derived context with timeout when timeout > 0;
// otherwise it returns the parent context with a no-op cancel.
func withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}

	return context.WithTimeout(parent, timeout)
}

// newClientFromSession creates a Client from an already-connected
// *mcp.ClientSession. Useful for testing with in-process transports. No
// server prefix is applied; tool names are exposed to the LLM exactly as
// reported by the server. Private: the in-process path is fully encapsulated
// by MockServer.NewClient (P4).
func newClientFromSession(ctx context.Context, session *mcp.ClientSession) (*Client, error) {
	nop := zerolog.Nop()

	toolsResult, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to list MCP tools: %w", err)
	}

	einoTools := make([]tool.BaseTool, 0, len(toolsResult.Tools))
	for _, mcpTool := range toolsResult.Tools {
		// No server name / prefix for in-process clients (mock / test path).
		einoTools = append(einoTools, NewBridgedTool(nop, session, mcpTool, "", mcpTool.Name))
	}

	return &Client{
		session: session,
		tools:   einoTools,
		logger:  nop,
	}, nil
}

// newInProcessClient connects an in-process client/server pair over
// mcp.NewInMemoryTransports and returns a Client whose Close shuts down both
// the client and the server session (P4). srv names no type outside this
// package's own callers (MockServer.NewClient).
func newInProcessClient(ctx context.Context, srv *mcp.Server) (*Client, error) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect in-process MCP server: %w", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: homeAgent, Version: implementationVersion}, nil)

	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()

		return nil, fmt.Errorf("failed to connect in-process MCP client: %w", err)
	}

	c, err := newClientFromSession(ctx, clientSession)
	if err != nil {
		_ = clientSession.Close()
		_ = serverSession.Close()

		return nil, err
	}

	c.closeServer = serverSession.Close

	return c, nil
}

// Tools returns the discovered MCP tools as Eino BaseTools.
func (c *Client) Tools() []tool.BaseTool {
	return c.tools
}

// Close disconnects from the MCP server. For an in-process client (P4) it
// also closes the server-side session of the in-memory transport pair.
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}

	err := c.session.Close()

	if c.closeServer != nil {
		if srvErr := c.closeServer(); srvErr != nil && err == nil {
			err = srvErr
		}
	}

	return err
}

// headerRoundTripper applies the configured headers to an outgoing HTTP
// request only when the request stays within the configured server's origin
// (P1). Neither StreamableClientTransport nor SSEClientTransport exposes a
// headers field, so this is the single mechanism that serves both. It is
// stateless beyond that scoping and must never log header values or request
// bodies: configured headers typically carry credentials.
type headerRoundTripper struct {
	headers   map[string]string
	serverURL *url.URL // nil when the configured URL failed to parse: fail closed
	base      http.RoundTripper
}

// newHeaderRoundTripper parses cfg.URL once so RoundTrip never re-parses it
// per request. An unparseable URL leaves serverURL nil, which inScope treats
// as "never in scope" (fail closed, D1/1.6).
func newHeaderRoundTripper(cfg *config.MCPServerConfig) *headerRoundTripper {
	serverURL, err := url.Parse(cfg.URL)
	if err != nil {
		serverURL = nil
	}

	return &headerRoundTripper{headers: cfg.Headers, serverURL: serverURL}
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}

	if len(rt.headers) == 0 || !rt.inScope(req.URL) {
		return base.RoundTrip(req)
	}

	cloned := req.Clone(req.Context())
	for k, v := range rt.headers {
		if cloned.Header.Get(k) != "" {
			continue // D2: never replace a header already present
		}
		cloned.Header.Set(k, v)
	}

	return base.RoundTrip(cloned)
}

// inScope reports whether u is within the configured server's origin: same
// hostname or a subdomain of it (port-insensitive, mirroring net/http's own
// redirect-header policy), and not a scheme downgrade from the configured
// scheme (D1).
func (rt *headerRoundTripper) inScope(u *url.URL) bool {
	if rt.serverURL == nil {
		return false
	}

	if !isDomainOrSubdomain(u.Hostname(), rt.serverURL.Hostname()) {
		return false
	}

	return !isSchemeDowngrade(rt.serverURL.Scheme, u.Scheme)
}

// isDomainOrSubdomain reports whether sub is the parent hostname or a
// subdomain of it, matching the predicate net/http applies when deciding
// whether to carry sensitive headers across a redirect.
func isDomainOrSubdomain(sub, parent string) bool {
	sub = strings.ToLower(sub)
	parent = strings.ToLower(parent)

	if sub == parent {
		return true
	}

	if !strings.HasSuffix(sub, ".") {
		sub += "."
	}
	if !strings.HasSuffix(parent, ".") {
		parent += "."
	}

	return strings.HasSuffix(sub, "."+parent)
}

// isSchemeDowngrade reports whether requestScheme is weaker than
// configuredScheme. The only downgrade this package recognises is https to
// http; a configured http:// server (e.g. Home Assistant on a LAN) makes this
// vacuously false, so its behaviour is unchanged (D1).
func isSchemeDowngrade(configuredScheme, requestScheme string) bool {
	return strings.EqualFold(configuredScheme, "https") && !strings.EqualFold(requestScheme, "https")
}

// NewScopedHTTPClient builds an *http.Client whose Transport applies cfg's
// configured headers only to requests within cfg's origin (headerRoundTripper).
// This is the single construction site for the header-scoping policy so a
// caller that needs the same policy elsewhere — e.g. mcp-oauth-authorization's
// discovery, registration and token requests — builds the client once here
// and shares it via ClientConfig.HTTPClient instead of re-implementing the
// predicate (D3).
func NewScopedHTTPClient(cfg *config.MCPServerConfig) *http.Client {
	return &http.Client{Transport: newHeaderRoundTripper(cfg)}
}

// newClientTransport builds the SDK transport for cfg. It is called once per
// connection attempt (transports are cheap struct values, recreated on every
// (re)connect per P2) and reuses the immutable httpClient carrying the header
// RoundTripper. MaxRetries: -1 disables the Streamable HTTP transport's own
// retry loop: ReconnectingClient is the sole retry authority (D3).
// oauthHandler is threaded into the Streamable HTTP transport only:
// mcp.SSEClientTransport has no OAuthHandler field, so it is silently unused
// for the "sse" case — config validation rejects an oauth block on that
// transport before this function is ever reached with a non-nil handler.
func newClientTransport(cfg *config.MCPServerConfig, httpClient *http.Client, oauthHandler auth.OAuthHandler) mcp.Transport {
	switch cfg.Transport {
	case "sse":
		// Legacy SSE transport: opens a persistent GET stream.
		return &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient}
	default:
		// Streamable HTTP transport (default): uses POST for all messages.
		// Required by Home Assistant MCP endpoint and the 2025-03-26 spec.
		return &mcp.StreamableClientTransport{
			Endpoint:     cfg.URL,
			HTTPClient:   httpClient,
			MaxRetries:   -1,
			OAuthHandler: oauthHandler,
		}
	}
}

// connectSession establishes a session over t via client.Connect, which
// performs the initialize handshake; there is no separate Initialize call.
func connectSession(ctx context.Context, client *mcp.Client, t mcp.Transport, timeout time.Duration) (*mcp.ClientSession, error) {
	connectCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	session, err := client.Connect(connectCtx, t, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MCP server: %w", classifyError(err))
	}

	return session, nil
}

// connectToServer establishes a one-shot connection to the MCP server: used
// by the simple *Client path (mock / short-lived connections). The
// production path (ReconnectingClient) builds its own immutable httpClient
// and *mcp.Client once and calls connectSession directly on every (re)connect
// (P2). httpClient, when nil, is built internally via NewScopedHTTPClient.
func connectToServer(ctx context.Context, cfg *config.MCPServerConfig, timeout time.Duration, httpClient *http.Client, oauthHandler auth.OAuthHandler) (*mcp.ClientSession, error) {
	if httpClient == nil {
		httpClient = NewScopedHTTPClient(cfg)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: homeAgent, Version: implementationVersion}, nil)

	return connectSession(ctx, client, newClientTransport(cfg, httpClient, oauthHandler), timeout)
}

// toolInvoker is the minimal surface required to execute an MCP tool call.
// It is satisfied by *mcp.ClientSession and by ReconnectingClient — the
// latter indirects through a swappable underlying session so BridgedTool
// keeps working transparently across reconnects (P3).
type toolInvoker interface {
	CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error)
}

// BridgedTool wraps a single MCP tool as an Eino InvokableTool.
type BridgedTool struct {
	client      toolInvoker
	mcpTool     *mcp.Tool
	logger      zerolog.Logger
	serverName  string // configuration key of the owning server (e.g. "home_assistant")
	displayName string // name exposed to the LLM (may include a server prefix)
}

// Compile-time assertion that Tool implements InvokableTool.
var _ tool.InvokableTool = (*BridgedTool)(nil)

// NewBridgedTool creates an Eino tool wrapper around an MCP tool.
// serverName is the configuration key for logging (e.g. "home_assistant").
// displayName is the name exposed to the LLM; when toolPrefix is non-empty it
// is built as "<toolPrefix>__<mcpTool.Name>", otherwise it equals mcpTool.Name.
func NewBridgedTool(logger zerolog.Logger, client toolInvoker, mcpTool *mcp.Tool, serverName, displayName string) *BridgedTool {
	return &BridgedTool{
		client:      client,
		mcpTool:     mcpTool,
		logger:      logger,
		serverName:  serverName,
		displayName: displayName,
	}
}

// buildDisplayName returns the name the LLM will see for a tool. When
// toolPrefix is non-empty the result is "<toolPrefix>__<wireName>"; otherwise
// wireName is returned unchanged (no prefix).
func buildDisplayName(toolPrefix, wireName string) string {
	if toolPrefix == "" {
		return wireName
	}

	return toolPrefix + "__" + wireName
}

// WireName returns the tool's name on the wire (unprefixed), as declared by
// the MCP server. Profile gating matches against this rather than the
// LLM-facing display name, so allow/deny rules stay portable across
// tool_prefix configuration.
func (t *BridgedTool) WireName() string {
	return t.mcpTool.Name
}

// Info returns the Eino ToolInfo derived from the MCP tool definition.
// The Name field reflects the display name (which may carry a server prefix)
// so the LLM uses the prefixed form. The wire name (mcpTool.Name) is only
// used inside InvokableRun when sending the CallToolParams to the server.
func (t *BridgedTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	// Convert MCP tool schema to Eino ParameterInfo.
	params := convertMCPSchemaToParams(t.logger, t.mcpTool.Name, t.mcpTool.InputSchema)

	info := &schema.ToolInfo{
		Name: t.displayName,
		Desc: t.mcpTool.Description,
	}

	if len(params) > 0 {
		info.ParamsOneOf = schema.NewParamsOneOfByParams(params)
	}

	return info, nil
}

// InvokableRun executes the MCP tool via the MCP client.
func (t *BridgedTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args map[string]any
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	}

	normalizedArgs := normalizeObjectArguments(args, t.mcpTool.InputSchema)
	normalizedArgs = omitEmptyOptionalStringArguments(normalizedArgs, t.mcpTool.InputSchema)

	// criticalities-remediation (M20): tool call arguments are user-derived
	// content — logged at DEBUG, not at the default INFO level. Log the exact
	// values handed to the MCP SDK, after boundary normalization.
	t.logger.Debug().Str("tool", t.mcpTool.Name).Str("server", t.serverName).Interface("args", normalizedArgs).Msg("MCP call")

	result, err := t.client.CallTool(ctx, &mcp.CallToolParams{
		Name:      t.mcpTool.Name,
		Arguments: normalizedArgs,
	})
	if err != nil {
		// Avoid double-classification: if the error already wraps a sentinel
		// (e.g., from ReconnectingClient.CallTool), use it as-is.
		classified := err
		if !isAlreadyClassified(err) {
			classified = classifyError(err)
		}

		t.logger.Info().Str("tool", t.mcpTool.Name).Str("server", t.serverName).Str("error", classified.Error()).Msg("MCP call ERR")

		return "", fmt.Errorf("MCP tool %q execution failed: %w", t.mcpTool.Name, classified)
	}

	if result.IsError {
		event := t.logger.Info().Str("tool", t.mcpTool.Name).Str("server", t.serverName).Str("error", "tool returned error result")
		if detail := extractTextContent(result); detail != "" {
			event = event.Str("error_detail", detail)
		}
		event.Msg("MCP call ERR")

		return extractTextContent(result), fmt.Errorf("%w: tool %q", ErrMCPToolExecution, t.mcpTool.Name)
	}

	t.logger.Info().Str("tool", t.mcpTool.Name).Str("server", t.serverName).Msg("MCP call OK")

	text := extractTextContent(result)
	if banner := unverifiedWriteBanner(text); banner != "" {
		t.logger.Warn().Str("tool", t.mcpTool.Name).Str("server", t.serverName).Msg("MCP tool reported success with unverified side effect")
		text = banner + "\n" + text
	}

	return text, nil
}

// unverifiedWriteBanner inspects an MCP tool JSON response and returns a
// non-empty banner when the payload advertises success but simultaneously
// carries warnings that the side effect could not be verified. This pattern
// is a generic anti-signal — some MCP servers accept a call against an
// invalid target (unknown identifier, offline resource, ...), dispatch it,
// and only surface the missing confirmation as a warning while still
// reporting success:true. Without this hint the LLM would take the response
// at face value and claim to the user that the action succeeded. Returning
// "" means "no banner needed"; the check is defensive and only triggers on
// well-formed JSON objects.
func unverifiedWriteBanner(text string) string {
	if text == "" || text[0] != '{' {
		return ""
	}

	var payload struct {
		Success  *bool    `json:"success"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return ""
	}
	if payload.Success == nil || !*payload.Success || len(payload.Warnings) == 0 {
		return ""
	}

	for _, w := range payload.Warnings {
		lw := strings.ToLower(w)
		if (strings.Contains(lw, "could not be verified") || strings.Contains(lw, "not verified")) ||
			(strings.Contains(lw, "not confirmed") || strings.Contains(lw, "unconfirmed")) {
			return "[UNVERIFIED] The tool reported nominal success but its side effect could NOT be confirmed. Treat this as a failure to act, not a success. Re-check the arguments you passed (especially any identifier) against the authoritative source declared by the relevant skill, verify the current state with a read tool, and if the target is still wrong report the ambiguity to the user instead of claiming the action was performed."
		}
	}

	return ""
}

// extractTextContent concatenates all text content from a CallToolResult.
func extractTextContent(result *mcp.CallToolResult) string {
	var text string

	for _, content := range result.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			if text != "" {
				text += "\n"
			}
			text += tc.Text
		}
	}

	return text
}

// normalizeObjectArguments repairs the common LLM representation of an
// optional, open-ended object as an empty string or a JSON-encoded object.
// MCP schemas describe the wire type, so this conversion belongs at the
// boundary and leaves already-correct arguments untouched.
func normalizeObjectArguments(args map[string]any, inputSchema any) map[string]any {
	// The SDK normally exposes schemas received from a server as
	// map[string]any, but it also permits json.RawMessage and typed schema
	// values. Round-trip non-map representations so normalization behaves the
	// same for every transport/server implementation.
	root, ok := inputSchema.(map[string]any)
	if !ok {
		raw, err := json.Marshal(inputSchema)
		if err != nil || json.Unmarshal(raw, &root) != nil {
			return args
		}
	}
	if root == nil {
		return args
	}

	properties, ok := root[jsonKeyProperties].(map[string]any)
	if !ok {
		return args
	}

	for name, property := range properties {
		propertySchema, ok := property.(map[string]any)
		if !ok || !schemaAcceptsObject(propertySchema) {
			continue
		}

		value, exists := args[name]
		if !exists {
			continue
		}

		typed, ok := value.(string)
		if !ok {
			continue
		}
		if typed == "" {
			args[name] = map[string]any{}

			continue
		}

		var object map[string]any
		if err := json.Unmarshal([]byte(typed), &object); err == nil && object != nil {
			args[name] = object
		}
	}

	return args
}

// omitEmptyOptionalStringArguments treats an empty string generated for an
// optional string parameter as an omitted value. LLMs commonly populate every
// advertised property with its zero value, while MCP servers generally
// distinguish an absent optional parameter from a present-but-invalid "".
// Required strings and enums that explicitly admit "" remain untouched.
func omitEmptyOptionalStringArguments(args map[string]any, inputSchema any) map[string]any {
	root, err := unmarshalToolSchema(inputSchema)
	if err != nil {
		return args
	}

	required := make(map[string]bool, len(root.Required))
	for _, name := range root.Required {
		required[name] = true
	}

	for name, property := range root.Properties {
		if required[name] || args[name] != "" {
			continue
		}

		stringSchema := property
		if alternative := nullableAlternative(property); alternative != nil {
			stringSchema = alternative
		}

		typ, _ := resolveType(stringSchema)
		values, stringEnumOK := stringEnum(stringSchema.Enum)
		if typ != jsonTypeString && (typ != "" || !stringEnumOK || len(values) == 0) {
			continue
		}

		if stringEnumOK {
			emptyAllowed := false
			for _, value := range values {
				if value == "" {
					emptyAllowed = true

					break
				}
			}
			if emptyAllowed {
				continue
			}
		}

		delete(args, name)
	}

	return args
}

// schemaAcceptsObject recognizes both a direct object declaration and the
// nullable object unions emitted by Pydantic/FastMCP for dict | None.
func schemaAcceptsObject(s map[string]any) bool {
	if s[jsonKeyType] == jsonTypeObject {
		return true
	}

	if types, ok := s[jsonKeyType].([]any); ok {
		return nullableTypeListContainsObject(types)
	}

	for _, keyword := range []string{jsonKeyAnyOf, jsonKeyOneOf} {
		alternatives, ok := s[keyword].([]any)
		if !ok || len(alternatives) != 2 {
			continue
		}

		var objects, nulls int
		for _, alternative := range alternatives {
			m, ok := alternative.(map[string]any)
			if !ok {
				break
			}
			switch m[jsonKeyType] {
			case jsonTypeObject:
				objects++
			case string(schema.Null):
				nulls++
			}
		}
		if objects == 1 && nulls == 1 {
			return true
		}
	}

	return false
}

func nullableTypeListContainsObject(types []any) bool {
	if len(types) != 2 {
		return false
	}

	var objects, nulls int
	for _, typ := range types {
		switch typ {
		case jsonTypeObject:
			objects++
		case string(schema.Null):
			nulls++
		}
	}

	return objects == 1 && nulls == 1
}

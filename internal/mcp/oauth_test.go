package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	homeoauth "github.com/marco-rozz/ready/internal/oauth"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
)

const (
	testOAuthClientID    = "test-client"
	testOAuthRedirectURL = "http://127.0.0.1:7601/callback"
)

// countingOAuthHandler wraps a real auth.OAuthHandler and counts calls, so
// tests can assert the handler actually reaches the transport (task 6.1)
// without depending on SDK internals.
type countingOAuthHandler struct {
	inner            sdkauth.OAuthHandler
	tokenSourceCalls atomic.Int32
	authorizeCalls   atomic.Int32
}

func (h *countingOAuthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.tokenSourceCalls.Add(1)

	return h.inner.TokenSource(ctx)
}

func (h *countingOAuthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	h.authorizeCalls.Add(1)

	return h.inner.Authorize(ctx, req, resp)
}

// newOAuthTestServer hosts mockServer over real HTTP via the SDK's own
// Streamable HTTP server handler, so tests exercise the genuine SDK request
// path (transport, OAuthHandler hooks, error wrapping) rather than a
// fabricated error chain.
func newOAuthTestServer(t *testing.T, mockServer *MockServer) *httptest.Server {
	t.Helper()

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mockServer.server }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv
}

// shortLivedNoRefreshCredential returns a Credential valid for ttl with no
// refresh token, so TokenSource.Token() starts usable and then fails
// structurally once ttl elapses — no network I/O, no invalid_grant
// round trip.
func shortLivedNoRefreshCredential(serverKey string, ttl time.Duration) *homeoauth.Credential {
	cred := homeoauth.NewCredential(serverKey)
	cred.ClientID = testOAuthClientID
	cred.AccessToken = "initial-access-token"
	cred.Expiry = time.Now().Add(ttl)

	return cred
}

// TestOAuthHandler_ReachesTransport connects through a real Streamable HTTP
// round trip with an OAuthHandler mounted, and asserts the transport
// actually consults it (task 6.1/6.2): TokenSource is called before
// requests are sent.
func TestOAuthHandler_ReachesTransport(t *testing.T) {
	mock := NewMockServer(nil)
	srv := newOAuthTestServer(t, mock)

	store := newTempOAuthStore(t)
	cred := shortLivedNoRefreshCredential("srv", time.Hour)
	if err := store.Save(context.Background(), cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	inner, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: cred.ClientID},
		RedirectURL:              testOAuthRedirectURL,
		AuthorizationCodeFetcher: homeoauth.NonInteractiveFetcher,
		InitialTokenSource:       homeoauth.NewTokenSourceFromCredential(store, srv.Client(), cred),
	})
	if err != nil {
		t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}

	counting := &countingOAuthHandler{inner: inner}

	cfg := &config.MCPServerConfig{URL: srv.URL, Timeout: 5 * time.Second}
	rc, err := NewReconnectingClient(context.Background(), cfg,
		ClientConfig{HTTPClient: srv.Client(), OAuthHandler: counting, Timeout: 5 * time.Second},
		ReconnectConfig{}, "srv", "")
	if err != nil {
		t.Fatalf("NewReconnectingClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if counting.tokenSourceCalls.Load() == 0 {
		t.Fatal("expected the transport to call OAuthHandler.TokenSource at least once")
	}
}

// TestLoginRequired_SurvivesFullSDKErrorChain drives a real 401 response
// through the SDK's own error wrapping (jsonrpc2.ErrRejected, etc.) and
// confirms the login-required sentinel survives errors.Is all the way to
// classifyError's output — and is not misclassified as ErrMCPConnection
// (task 6.14).
func TestLoginRequired_SurvivesFullSDKErrorChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	handler := &alwaysLoginRequiredHandler{}

	cfg := &config.MCPServerConfig{URL: srv.URL}

	_, err := connectToServer(context.Background(), cfg, 2*time.Second, srv.Client(), handler)
	if err == nil {
		t.Fatal("expected a connection error")
	}

	if !errors.Is(err, ErrMCPOAuthLoginRequired) {
		t.Fatalf("expected ErrMCPOAuthLoginRequired, got %v", err)
	}
	if errors.Is(err, ErrMCPConnection) {
		t.Fatalf("login-required must not be classified as ErrMCPConnection, got %v", err)
	}
}

// alwaysLoginRequiredHandler is a minimal auth.OAuthHandler standing in for
// internal/oauth's loginRequiredHandler (unexported, so re-implemented here
// against the same contract: a failing, non-nil token source, and Authorize
// returning oauth.ErrLoginRequired).
type alwaysLoginRequiredHandler struct{}

func (alwaysLoginRequiredHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return failingTokenSourceStub{}, nil
}

func (alwaysLoginRequiredHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	_ = resp.Body.Close()

	return homeoauth.ErrLoginRequired
}

type failingTokenSourceStub struct{}

func (failingTokenSourceStub) Token() (*oauth2.Token, error) {
	return nil, homeoauth.ErrLoginRequired
}

// toggleTokenSource wraps a real oauth2.TokenSource and, once armed via
// kill(), fails every subsequent call with homeoauth.ErrLoginRequired — the
// same sentinel internal/oauth's TokenSource produces structurally (design
// D9) when a credential expires with no refresh token. This lets the test
// drive the exact moment a credential "dies" deterministically, instead of
// waiting out oauth2.Token's unconfigurable 10s expiry skew, while every
// other part of the stack (transport, OAuthHandler, classifyError,
// shouldDropSession) remains the genuine production path.
type toggleTokenSource struct {
	inner oauth2.TokenSource
	dead  atomic.Bool
}

func (t *toggleTokenSource) Token() (*oauth2.Token, error) {
	if t.dead.Load() {
		return nil, homeoauth.ErrLoginRequired
	}

	return t.inner.Token()
}

func (t *toggleTokenSource) kill() { t.dead.Store(true) }

// TestSessionSurvivesAuthFailure_CallToolAndPing exercises the design doc's core
// promise: an authorization failure — from a tool call or from a health
// ping — never tears down the session.
func TestSessionSurvivesAuthFailure_CallToolAndPing(t *testing.T) {
	mock := NewMockServer(nil)
	srv := newOAuthTestServer(t, mock)

	store := newTempOAuthStore(t)
	cred := shortLivedNoRefreshCredential("srv", time.Hour)
	if err := store.Save(context.Background(), cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := &toggleTokenSource{inner: homeoauth.NewTokenSourceFromCredential(store, srv.Client(), cred)}

	handler, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: cred.ClientID},
		RedirectURL:              testOAuthRedirectURL,
		AuthorizationCodeFetcher: homeoauth.NonInteractiveFetcher,
		InitialTokenSource:       ts,
	})
	if err != nil {
		t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}

	cfg := &config.MCPServerConfig{URL: srv.URL, Timeout: 5 * time.Second}
	rc, err := NewReconnectingClient(context.Background(), cfg,
		ClientConfig{HTTPClient: srv.Client(), OAuthHandler: handler, Timeout: 5 * time.Second},
		ReconnectConfig{}, "srv", "")
	if err != nil {
		t.Fatalf("NewReconnectingClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if !rc.Connected() {
		t.Fatal("expected Connected() == true right after a successful connect")
	}

	// The credential "dies" (the design doc's structural login-required case).
	ts.kill()

	_, callErr := rc.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      toolLightTurnOn,
		Arguments: map[string]any{attrEntityID: "light.test"},
	})
	if !errors.Is(callErr, ErrMCPOAuthLoginRequired) {
		t.Fatalf("CallTool error = %v, want ErrMCPOAuthLoginRequired", callErr)
	}
	if !rc.Connected() {
		t.Fatal("expected the session to survive an authorization failure from CallTool")
	}

	pingErr := rc.Ping(context.Background())
	if !errors.Is(pingErr, ErrMCPOAuthLoginRequired) {
		t.Fatalf("Ping error = %v, want ErrMCPOAuthLoginRequired", pingErr)
	}
	if !rc.Connected() {
		t.Fatal("expected the session to survive an authorization failure from Ping")
	}
}

// diesThenRecoversViaStore returns a fixed valid token until killed, then
// delegates to recovery — a real, store-backed internal/oauth.TokenSource —
// so the "recovers via a store change" half of the test exercises the
// genuine reload-on-failure logic, not a fabricated success.
type diesThenRecoversViaStore struct {
	dead     atomic.Bool
	valid    *oauth2.Token
	recovery oauth2.TokenSource
}

func (d *diesThenRecoversViaStore) Token() (*oauth2.Token, error) {
	if !d.dead.Load() {
		return d.valid, nil
	}

	return d.recovery.Token()
}

// TestHealthMonitor_PicksUpStoreChangeWithoutToolCall exercises the D5/D8
// combination task 6.14 asks for: with health checking enabled at a short
// interval, a credential written to the store by an out-of-band `READY
// login` is picked up by the next periodic ping alone — no tool call
// required — and the monitor logs the recovery transition once.
func TestHealthMonitor_PicksUpStoreChangeWithoutToolCall(t *testing.T) {
	mock := NewMockServer(nil)
	srv := newOAuthTestServer(t, mock)

	store := newTempOAuthStore(t)

	// The credential the health ping will fall back to once "dead": no
	// access token, no refresh token — structurally unusable until the
	// store gets a newer record.
	deadCred := homeoauth.NewCredential("srv")
	deadCred.ClientID = testOAuthClientID

	recovery := homeoauth.NewTokenSourceFromCredential(store, srv.Client(), deadCred)
	hybrid := &diesThenRecoversViaStore{
		valid:    &oauth2.Token{AccessToken: "initial-access-token", Expiry: time.Now().Add(time.Hour)},
		recovery: recovery,
	}

	handler, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      &oauthex.ClientCredentials{ClientID: testOAuthClientID},
		RedirectURL:              testOAuthRedirectURL,
		AuthorizationCodeFetcher: homeoauth.NonInteractiveFetcher,
		InitialTokenSource:       hybrid,
	})
	if err != nil {
		t.Fatalf("NewAuthorizationCodeHandler: %v", err)
	}

	cfg := &config.MCPServerConfig{URL: srv.URL, Timeout: 5 * time.Second}
	rc, err := NewReconnectingClient(context.Background(), cfg,
		ClientConfig{HTTPClient: srv.Client(), OAuthHandler: handler, Timeout: 5 * time.Second},
		ReconnectConfig{}, "srv", "")
	if err != nil {
		t.Fatalf("NewReconnectingClient: %v", err)
	}
	defer func() { _ = rc.Close() }()

	var buf syncBuffer

	hm := NewHealthMonitor(rc, rc, 10*time.Millisecond)
	hm.logger = zerolog.New(&buf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hm.Start(ctx)

	// The credential "dies"; the next ping(s) hit the permanent
	// authorization condition.
	hybrid.dead.Store(true)
	time.Sleep(50 * time.Millisecond)

	if !rc.Connected() {
		t.Fatal("expected the session to survive the permanent authorization condition")
	}
	if !strings.Contains(buf.String(), "permanent authorization condition") {
		t.Fatal("expected the monitor to have logged the permanent authorization condition")
	}

	// `ready login` runs out of band and writes a fresh credential — no
	// tool call, no explicit reconnect.
	freshCred := homeoauth.NewCredential("srv")
	freshCred.ClientID = testOAuthClientID
	freshCred.AccessToken = "fresh-access-token"
	freshCred.Expiry = time.Now().Add(time.Hour)
	freshCred.UpdatedAt = time.Now()
	if err := store.Save(context.Background(), freshCred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "recovered after a permanent authorization condition") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	hm.Stop()

	if !strings.Contains(buf.String(), "recovered after a permanent authorization condition") {
		t.Fatalf("expected the monitor to log recovery after the store change, log:\n%s", buf.String())
	}
	if recoveryCount := strings.Count(buf.String(), "recovered after a permanent authorization condition"); recoveryCount != 1 {
		t.Fatalf("logged the recovery transition %d times, want exactly 1", recoveryCount)
	}
}

// newTempOAuthStore returns a file-backed oauth.Store rooted at a fresh temp
// directory, for tests that need a real Store to satisfy
// NewTokenSourceFromCredential's signature.
func newTempOAuthStore(t *testing.T) homeoauth.Store {
	t.Helper()

	store, err := homeoauth.NewFileStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	return store
}

// TestReconnect_BothSentinelsAbortWithoutRetrying verifies task 6.10: both
// ErrMCPAuth and ErrMCPOAuthLoginRequired short-circuit Reconnect on the
// first attempt with distinct log messages (message content is exercised in
// internal/oauth's own tests; here we assert the abort-without-retry
// behaviour, which is this package's responsibility).
func TestReconnect_BothSentinelsAbortWithoutRetrying(t *testing.T) {
	tests := []struct {
		name    string
		srv     func() *httptest.Server
		handler sdkauth.OAuthHandler
		want    error
	}{
		{
			name: "ErrMCPAuth (static header rejected)",
			srv: func() *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
				}))
			},
			handler: nil,
			want:    ErrMCPAuth,
		},
		{
			name: "ErrMCPOAuthLoginRequired (no usable credential)",
			srv: func() *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
				}))
			},
			handler: alwaysLoginRequiredHandler{},
			want:    ErrMCPOAuthLoginRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := tt.srv()
			defer srv.Close()

			rc := newTestReconnectingClient(&ReconnectingClient{
				serverCfg:    &config.MCPServerConfig{URL: srv.URL},
				clientCfg:    ClientConfig{Timeout: 2 * time.Second, HTTPClient: srv.Client(), OAuthHandler: tt.handler},
				oauthHandler: tt.handler,
				reconnectCf: ReconnectConfig{
					InitialBackoff: 5 * time.Second, // large: a retry would make the test time out
					MaxBackoff:     5 * time.Second,
					BackoffFactor:  2.0,
					MaxRetries:     0,
				},
				logger: discardLogger(),
			})

			done := make(chan error, 1)
			go func() { done <- rc.Reconnect(context.Background()) }()

			select {
			case err := <-done:
				if !errors.Is(err, tt.want) {
					t.Fatalf("Reconnect error = %v, want %v", err, tt.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Reconnect did not short-circuit — it appears to be retrying")
			}
		})
	}
}

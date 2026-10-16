package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/rs/zerolog"
)

const (
	testClientID     = "cid"
	testClientSecret = "secret"
)

func newTestOAuthStore(t *testing.T) oauth.Store {
	t.Helper()

	store, err := oauth.NewFileStore(context.Background(), filepath.Join(t.TempDir(), "oauth"))
	if err != nil {
		t.Fatalf("oauth.NewFileStore: %v", err)
	}

	return store
}

func TestHasOAuthServer(t *testing.T) {
	if hasOAuthServer(map[string]config.MCPServerConfig{
		"a": {},
		"b": {OAuth: nil},
	}) {
		t.Error("expected false when no server declares an oauth block")
	}

	if !hasOAuthServer(map[string]config.MCPServerConfig{
		"a": {},
		"b": {OAuth: &config.MCPOAuthConfig{}},
	}) {
		t.Error("expected true when a server declares an oauth block")
	}
}

func TestResolveDaemonGrant(t *testing.T) {
	tests := []struct {
		name                string
		oa                  *config.MCPOAuthConfig
		hasStoredCredential bool
		want                string
		wantErr             bool
	}{
		{
			name: "explicit authorization_code",
			oa:   &config.MCPOAuthConfig{Grant: "authorization_code"},
			want: oauth.GrantAuthorizationCode,
		},
		{
			name: "explicit client_credentials",
			oa:   &config.MCPOAuthConfig{Grant: oauth.GrantClientCredentials},
			want: oauth.GrantClientCredentials,
		},
		{
			name:                "auto with stored credential is always authorization_code",
			oa:                  &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecret: testClientSecret},
			hasStoredCredential: true,
			want:                oauth.GrantAuthorizationCode,
		},
		{
			name: "auto with client_id and secret and no stored credential resolves client_credentials",
			oa:   &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecret: testClientSecret},
			want: oauth.GrantClientCredentials,
		},
		{
			name: "auto with no client credentials falls back to authorization_code",
			oa:   &config.MCPOAuthConfig{Grant: grantAuto},
			want: oauth.GrantAuthorizationCode,
		},
		{
			name: "empty grant behaves like auto",
			oa:   &config.MCPOAuthConfig{},
			want: oauth.GrantAuthorizationCode,
		},
		{
			name:    "unreadable client_secret_file surfaces an error",
			oa:      &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecretFile: filepath.Join(t.TempDir(), "missing-secret")},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDaemonGrant(tc.oa, tc.hasStoredCredential)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}

				return
			}
			if err != nil {
				t.Fatalf("resolveDaemonGrant: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveDaemonGrant() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildDaemonOAuthHandler_NoStoredCredential verifies the design doc's second
// row: with no stored credential, the daemon mounts loginRequiredHandler,
// whose TokenSource fails rather than being nil — so callers get
// oauth.ErrLoginRequired instead of a network round trip against an
// unauthenticated request.
func TestBuildDaemonOAuthHandler_NoStoredCredential(t *testing.T) {
	ctx := context.Background()
	store := newTestOAuthStore(t)

	srv := config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{Grant: grantAuto}}

	handler, err := buildDaemonOAuthHandler(ctx, store, "no-cred-server", srv, &http.Client{})
	if err != nil {
		t.Fatalf("buildDaemonOAuthHandler: %v", err)
	}

	ts, err := handler.TokenSource(ctx)
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if ts == nil {
		t.Fatal("expected a non-nil (failing) token source, got nil — a nil source would let the transport send an unauthenticated request")
	}

	if _, err := ts.Token(); err == nil {
		t.Error("expected Token() to fail with oauth.ErrLoginRequired, got nil error")
	}
}

// TestBuildDaemonOAuthHandler_StoredCredential verifies the design doc's first
// row: a usable stored Credential is wired as InitialTokenSource and produces
// a working token with no network I/O.
func TestBuildDaemonOAuthHandler_StoredCredential(t *testing.T) {
	ctx := context.Background()
	store := newTestOAuthStore(t)

	cred := oauth.NewCredential("stored-server")
	cred.ClientID = testClientID
	cred.RedirectURI = "http://127.0.0.1:7601/callback"
	cred.AccessToken = "at"
	cred.Expiry = time.Now().Add(time.Hour)

	if err := store.Save(ctx, cred); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	srv := config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{Grant: grantAuto}}

	handler, err := buildDaemonOAuthHandler(ctx, store, "stored-server", srv, &http.Client{})
	if err != nil {
		t.Fatalf("buildDaemonOAuthHandler: %v", err)
	}

	ts, err := handler.TokenSource(ctx)
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if ts == nil {
		t.Fatal("expected a non-nil token source for a usable stored credential")
	}

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok.AccessToken != "at" {
		t.Errorf("Token().AccessToken = %q, want %q", tok.AccessToken, "at")
	}
}

// TestBuildDaemonOAuthHandler_ExpiredWithoutRefreshToken verifies that a
// stored credential whose access token has expired and carries no refresh
// token behaves the same as an absent credential (task 8.5): Token() fails
// with oauth.ErrLoginRequired rather than attempting a network refresh.
func TestBuildDaemonOAuthHandler_ExpiredWithoutRefreshToken(t *testing.T) {
	ctx := context.Background()
	store := newTestOAuthStore(t)

	cred := oauth.NewCredential("expired-server")
	cred.ClientID = testClientID
	cred.RedirectURI = "http://127.0.0.1:7601/callback"
	cred.AccessToken = "at"
	cred.Expiry = time.Now().Add(-time.Hour)

	if err := store.Save(ctx, cred); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	srv := config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{Grant: grantAuto}}

	handler, err := buildDaemonOAuthHandler(ctx, store, "expired-server", srv, &http.Client{})
	if err != nil {
		t.Fatalf("buildDaemonOAuthHandler: %v", err)
	}

	ts, err := handler.TokenSource(ctx)
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}

	if _, err := ts.Token(); err == nil {
		t.Error("expected Token() to fail for an expired credential with no refresh token")
	}
}

// TestBuildDaemonOAuthHandler_ClientCredentials verifies the daemon connects
// autonomously for grant: client_credentials, with no stored-credential gate
// : the handler builds successfully purely from the
// configured client_id/secret.
func TestBuildDaemonOAuthHandler_ClientCredentials(t *testing.T) {
	ctx := context.Background()
	store := newTestOAuthStore(t)

	srv := config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{
		Grant:        oauth.GrantClientCredentials,
		ClientID:     testClientID,
		ClientSecret: testClientSecret,
	}}

	handler, err := buildDaemonOAuthHandler(ctx, store, "cc-server", srv, &http.Client{})
	if err != nil {
		t.Fatalf("buildDaemonOAuthHandler: %v", err)
	}

	if _, err := handler.TokenSource(ctx); err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
}

// TestBuildDaemonOAuthHandler_ClientCredentialsMissingSecret verifies a
// misconfigured client_credentials server (no resolvable secret) surfaces an
// explicit error rather than silently falling back to authorization_code.
func TestBuildDaemonOAuthHandler_ClientCredentialsMissingSecret(t *testing.T) {
	ctx := context.Background()
	store := newTestOAuthStore(t)

	srv := config.MCPServerConfig{OAuth: &config.MCPOAuthConfig{
		Grant:    oauth.GrantClientCredentials,
		ClientID: testClientID,
	}}

	if _, err := buildDaemonOAuthHandler(ctx, store, "cc-server", srv, &http.Client{}); err == nil {
		t.Error("expected an error for client_credentials with no resolvable secret")
	}
}

// TestConnectAllMCP_OAuthLoginRequiredIsBestEffortSkipped exercises
// connectAllMCP end to end for an OAuth server with no stored credential: the
// best-effort boot loop must recognise ErrMCPOAuthLoginRequired and skip the
// server without panicking or aborting the other servers (task 8.3), and the
// resulting composite runtime must expose no tools for it.
func TestConnectAllMCP_OAuthLoginRequiredIsBestEffortSkipped(t *testing.T) {
	ctx := context.Background()

	cfg := &config.Config{
		OAuth: config.OAuthConfig{Path: filepath.Join(t.TempDir(), "oauth")},
		MCPServers: map[string]config.MCPServerConfig{
			"oauth-server": {
				URL:       "https://127.0.0.1:0/mcp",
				Transport: "streamable_http",
				Timeout:   time.Second,
				OAuth:     &config.MCPOAuthConfig{Grant: grantAuto},
			},
		},
	}

	runtime, _, err := connectAllMCP(ctx, cfg, false, "default", false, zerolog.Nop())
	if err != nil {
		t.Fatalf("connectAllMCP: %v", err)
	}

	if runtime != nil {
		defer func() { _ = runtime.Close() }()

		if got := len(runtime.Tools()); got != 0 {
			t.Errorf("expected no tools for a server skipped for login-required, got %d", got)
		}
	}

	_ = homemcp.ErrMCPOAuthLoginRequired // referenced by connectAllMCP; guards against accidental removal
}

package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	homemcp "github.com/marco-rozz/ready/internal/mcp"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/rs/zerolog"
)

const testClientID = "cid"

// testClientSecretRef is a reference; the value is irrelevant to the grant decision.
var testClientSecretRef = config.SecretRef{Kind: config.SecretRefName, Name: "mcp_s_client_secret"} //nolint:gochecknoglobals // immutable fixture

func newTestOAuthStore(t *testing.T) oauth.Store {
	t.Helper()

	dir := t.TempDir()

	return oauth.NewSecretStore(testSecretStore(t, dir), filepath.Join(dir, "oauth"))
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
			oa:                  &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecret: testClientSecretRef},
			hasStoredCredential: true,
			want:                oauth.GrantAuthorizationCode,
		},
		{
			name: "auto with client_id and secret and no stored credential resolves client_credentials",
			oa:   &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecret: testClientSecretRef},
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
			// Structural: the decision never touches the secret store or any
			// file, so an unresolvable reference still selects the grant.
			name: "auto decides on the reference alone, with no secret I/O",
			oa:   &config.MCPOAuthConfig{Grant: grantAuto, ClientID: testClientID, ClientSecret: config.SecretRef{Kind: config.SecretRefFile, Path: filepath.Join(t.TempDir(), "missing-secret")}},
			want: oauth.GrantClientCredentials,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveDaemonGrant(tc.oa, tc.hasStoredCredential)
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

	handler, err := buildDaemonOAuthHandler(ctx, store, nil, "no-cred-server", srv, &http.Client{}, "")
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

	handler, err := buildDaemonOAuthHandler(ctx, store, nil, "stored-server", srv, &http.Client{}, "")
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

	handler, err := buildDaemonOAuthHandler(ctx, store, nil, "expired-server", srv, &http.Client{}, "")
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
		ClientSecret: testSecretFileRef(t, "secret"),
	}}

	handler, err := buildDaemonOAuthHandler(ctx, store, config.NewSecretSource(nil), "cc-server", srv, &http.Client{}, "")
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

	if _, err := buildDaemonOAuthHandler(ctx, store, config.NewSecretSource(nil), "cc-server", srv, &http.Client{}, ""); err == nil {
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

	agentDir := t.TempDir()
	cfg := &config.Config{
		SecretValues: config.NewSecretSource(testSecretStore(t, agentDir)),
		OAuth:        config.OAuthConfig{Path: filepath.Join(agentDir, "oauth")},
		MCPServers: map[string]config.MCPServerConfig{
			"oauth-server": {
				URL:       config.SecretString{Value: "https://127.0.0.1:0/mcp"},
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

// testSecretFileRef returns a `!secret_file` reference to a temporary file.
func testSecretFileRef(t *testing.T, value string) config.SecretRef {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}

	return config.SecretRef{Kind: config.SecretRefFile, Path: path}
}

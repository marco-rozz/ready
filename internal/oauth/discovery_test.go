package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
)

// fakeAuthServer serves the minimal RFC 8414 metadata document, RFC 7591
// registration, and RFC 7592 deregistration endpoints needed to exercise
// discovery and registration end-to-end.
type fakeAuthServer struct {
	*httptest.Server
	registrations   int32
	deregistrations int32
	grantTypes      []string
	nextClientID    int32
}

func newFakeAuthServer(t *testing.T, grantTypes []string) *fakeAuthServer {
	t.Helper()

	fas := &fakeAuthServer{grantTypes: grantTypes}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           fas.URL,
			"authorization_endpoint":           fas.URL + "/authorize",
			"token_endpoint":                   fas.URL + "/token",
			"registration_endpoint":            fas.URL + "/register",
			"response_types_supported":         []string{"code"},
			"grant_types_supported":            fas.grantTypes,
			"code_challenge_methods_supported": []string{"S256"},
		})
	})

	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&fas.registrations, 1)
		id := atomic.AddInt32(&fas.nextClientID, 1)
		clientID := "client-" + strconv.Itoa(int(id))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                 clientID,
			"client_secret":             "secret-" + strconv.Itoa(int(id)),
			"registration_access_token": "rat-" + strconv.Itoa(int(id)),
			"registration_client_uri":   fas.URL + "/register/" + clientID,
		})
	})

	mux.HandleFunc("/register/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}
		atomic.AddInt32(&fas.deregistrations, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := r.ParseForm(); err == nil && r.Form.Get("refresh_token") == "revoked-registration" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": errorCodeInvalidClient})

			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token",
			"token_type":   tokenTypeBearer,
		})
	})

	fas.Server = httptest.NewServer(mux)

	return fas
}

// fakeResourceServer serves a Protected Resource Metadata document pointing
// at asIssuer.
func fakeResourceServer(t *testing.T, asIssuer string) *httptest.Server {
	t.Helper()

	var srv *httptest.Server
	mux := http.NewServeMux()

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, srv.URL))
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              srv.URL + "/mcp",
			"authorization_servers": []string{asIssuer},
		})
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv = httptest.NewServer(mux)

	return srv
}

func TestProbe_NoAuthorizationRequired(t *testing.T) {
	srv := fakeResourceServer(t, "https://as.example.com")
	defer srv.Close()

	_, err := Probe(context.Background(), srv.Client(), srv.URL+"/ok")
	if err != ErrNoAuthorizationRequired { //nolint:errorlint // exact sentinel expected, not wrapped
		t.Fatalf("error = %v, want ErrNoAuthorizationRequired", err)
	}
}

func TestProbe_ReturnsChallenges(t *testing.T) {
	srv := fakeResourceServer(t, "https://as.example.com")
	defer srv.Close()

	challenges, err := Probe(context.Background(), srv.Client(), srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(challenges) == 0 || challenges[0].Scheme != schemeBearer {
		t.Fatalf("challenges = %+v, want a bearer challenge", challenges)
	}
}

func TestDiscovery_FullChain(t *testing.T) {
	fas := newFakeAuthServer(t, []string{GrantAuthorizationCode, paramRefreshTokenJSON})
	defer fas.Close()

	rsrv := fakeResourceServer(t, fas.URL)
	defer rsrv.Close()

	ctx := context.Background()
	client := rsrv.Client()

	challenges, err := Probe(ctx, client, rsrv.URL+"/mcp")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	prm, err := DiscoverProtectedResource(ctx, client, challenges, rsrv.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverProtectedResource: %v", err)
	}
	if len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != fas.URL {
		t.Fatalf("prm.AuthorizationServers = %v, want [%s]", prm.AuthorizationServers, fas.URL)
	}

	asm, err := DiscoverAuthServer(ctx, client, prm)
	if err != nil {
		t.Fatalf("DiscoverAuthServer: %v", err)
	}
	if asm.TokenEndpoint != fas.URL+"/token" {
		t.Fatalf("TokenEndpoint = %q, want %q", asm.TokenEndpoint, fas.URL+"/token")
	}
	if asm.RegistrationEndpoint != fas.URL+"/register" {
		t.Fatalf("RegistrationEndpoint = %q, want %q", asm.RegistrationEndpoint, fas.URL+"/register")
	}
}

func TestRegisterClient_EachCallRegistersAFreshClient(t *testing.T) {
	fas := newFakeAuthServer(t, []string{GrantAuthorizationCode, paramRefreshTokenJSON})
	defer fas.Close()

	ctx := context.Background()
	client := fas.Client()

	first, err := RegisterClient(ctx, client, fas.URL+"/register", "http://127.0.0.1:7601/callback")
	if err != nil {
		t.Fatalf("RegisterClient (first): %v", err)
	}
	if first.ClientID == "" || first.RegistrationAccessToken == "" || first.RegistrationClientURI == "" {
		t.Fatalf("first registration missing fields: %+v", first)
	}

	second, err := RegisterClient(ctx, client, fas.URL+"/register", "http://127.0.0.1:7601/callback")
	if err != nil {
		t.Fatalf("RegisterClient (second): %v", err)
	}

	// RegisterClient is not idempotent: calling it again registers a new,
	// distinct client. Callers must check for an existing
	// client_id first and reuse it rather than calling this again.
	if first.ClientID == second.ClientID {
		t.Fatalf("expected distinct client IDs across calls, got %q twice", first.ClientID)
	}
	if atomic.LoadInt32(&fas.registrations) != 2 {
		t.Fatalf("registrations = %d, want 2", fas.registrations)
	}
}

func TestDeregister(t *testing.T) {
	fas := newFakeAuthServer(t, []string{GrantAuthorizationCode, paramRefreshTokenJSON})
	defer fas.Close()

	ctx := context.Background()
	client := fas.Client()

	reg, err := RegisterClient(ctx, client, fas.URL+"/register", "http://127.0.0.1:7601/callback")
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	if err := Deregister(ctx, client, reg.RegistrationClientURI, reg.RegistrationAccessToken); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if atomic.LoadInt32(&fas.deregistrations) != 1 {
		t.Fatalf("deregistrations = %d, want 1", fas.deregistrations)
	}
}

func TestIsInvalidClient(t *testing.T) {
	retrieveErr := &oauth2.RetrieveError{ErrorCode: "invalid_client"}
	if !IsInvalidClient(retrieveErr) {
		t.Fatal("expected IsInvalidClient to recognise invalid_client")
	}

	// Wrapped errors must still be recognised structurally (errors.As), not
	// by matching the error string.
	wrapped := fmt.Errorf("token refresh failed: %w", retrieveErr)
	if !IsInvalidClient(wrapped) {
		t.Fatal("expected IsInvalidClient to see through error wrapping")
	}

	otherErr := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	if IsInvalidClient(otherErr) {
		t.Fatal("IsInvalidClient should not match invalid_grant")
	}

	if IsInvalidClient(errors.New("some unrelated error")) {
		t.Fatal("IsInvalidClient should not match a non-RetrieveError")
	}
}

// TestInvalidClientRecovery exercises the full loop the design doc requires: a
// refresh that fails with invalid_client is recognised structurally
// (IsInvalidClient), the stale registration is cleared from the Credential,
// and a fresh RegisterClient + refresh against the new registration
// succeeds.
func TestInvalidClientRecovery(t *testing.T) {
	fas := newFakeAuthServer(t, []string{GrantAuthorizationCode, paramRefreshTokenJSON})
	defer fas.Close()

	ctx := context.Background()
	client := fas.Client()

	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = "repudiated-client"
	cred.TokenEndpoint = fas.URL + "/token"
	cred.RefreshToken = "revoked-registration"
	cred.Expiry = time.Now().Add(-time.Hour)
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, client, cred)

	_, err = ts.Token()
	if !IsInvalidClient(err) {
		t.Fatalf("expected an invalid_client error, got %v", err)
	}

	// The registration was repudiated: register a fresh client and persist
	// a Credential built around it, exactly as `ready login` would after
	// detecting invalid_client.
	reg, err := RegisterClient(ctx, client, fas.URL+"/register", "http://127.0.0.1:7601/callback")
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	fresh := NewCredential(testServerKey)
	fresh.ClientID = reg.ClientID
	fresh.ClientSecret = reg.ClientSecret
	fresh.TokenEndpoint = fas.URL + "/token"
	fresh.RefreshToken = "good-refresh-token"
	fresh.Expiry = time.Now().Add(-time.Hour)
	if err = store.Save(ctx, fresh); err != nil {
		t.Fatalf("Save fresh: %v", err)
	}

	ts2 := NewTokenSourceFromCredential(store, client, fresh)

	tok, err := ts2.Token()
	if err != nil {
		t.Fatalf("Token after re-registration: %v", err)
	}
	if tok.AccessToken != "access-token" {
		t.Fatalf("AccessToken = %q, want %q", tok.AccessToken, "access-token")
	}
}

func TestResolveGrant(t *testing.T) {
	tests := []struct {
		name                                string
		configuredGrant                     string
		hasClientCredentials                bool
		authServerSupportsClientCredentials bool
		want                                string
	}{
		{"explicit authorization_code wins", GrantAuthorizationCode, true, true, GrantAuthorizationCode},
		{"explicit client_credentials wins", GrantClientCredentials, false, false, GrantClientCredentials},
		{"auto with creds and AS support -> client_credentials", grantAuto, true, true, GrantClientCredentials},
		{"auto with creds but no AS support -> authorization_code", grantAuto, true, false, GrantAuthorizationCode},
		{"auto without creds -> authorization_code", grantAuto, false, true, GrantAuthorizationCode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveGrant(tt.configuredGrant, tt.hasClientCredentials, tt.authServerSupportsClientCredentials)
			if got != tt.want {
				t.Errorf("ResolveGrant(%q, %v, %v) = %q, want %q",
					tt.configuredGrant, tt.hasClientCredentials, tt.authServerSupportsClientCredentials, got, tt.want)
			}
		})
	}
}

func TestLogIfNoRefreshToken(t *testing.T) {
	var buf bytes.Buffer

	logger := zerolog.New(&buf)

	LogIfNoRefreshToken(logger, testServerKey, &oauth2.Token{AccessToken: "a"})
	if buf.Len() == 0 {
		t.Fatal("expected a log line when RefreshToken is empty")
	}

	buf.Reset()
	LogIfNoRefreshToken(logger, testServerKey, &oauth2.Token{AccessToken: "a", RefreshToken: "r"})
	if buf.Len() != 0 {
		t.Fatal("expected no log line when RefreshToken is present")
	}
}

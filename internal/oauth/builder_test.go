package oauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func TestBuildHandler_NoUsableCredentialYieldsLoginRequired(t *testing.T) {
	h, err := BuildHandler(HandlerConfig{ServerKey: testServerKey, Grant: GrantAuthorizationCode})
	if err != nil {
		t.Fatalf("BuildHandler: %v", err)
	}

	assertWrappedInChallengeAwareness(t, h)

	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if _, tokErr := ts.Token(); !errors.Is(tokErr, ErrLoginRequired) {
		t.Fatalf("Token() error = %v, want ErrLoginRequired", tokErr)
	}
}

func TestBuildHandler_UsableCredentialYieldsAuthorizationCodeHandler(t *testing.T) {
	store, err := NewFileStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = testClientID
	cred.AccessToken = testAccessToken
	cred.Expiry = time.Now().Add(time.Hour)

	h, err := BuildHandler(HandlerConfig{
		ServerKey:          testServerKey,
		Grant:              GrantAuthorizationCode,
		RegisteredClient:   &oauthex.ClientCredentials{ClientID: cred.ClientID},
		RedirectURL:        "http://127.0.0.1:7601/callback",
		InitialTokenSource: NewTokenSourceFromCredential(store, http.DefaultClient, cred),
	})
	if err != nil {
		t.Fatalf("BuildHandler: %v", err)
	}

	assertWrappedInChallengeAwareness(t, h)

	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != testAccessToken {
		t.Fatalf("AccessToken = %q, want %q", tok.AccessToken, testAccessToken)
	}
}

func TestBuildHandler_FreshFlowYieldsAuthorizationCodeHandler(t *testing.T) {
	h, err := BuildHandler(HandlerConfig{
		ServerKey:        testServerKey,
		Grant:            GrantAuthorizationCode,
		RegisteredClient: &oauthex.ClientCredentials{ClientID: testClientID},
		RedirectURL:      "http://127.0.0.1:7601/callback",
		Fetcher:          NonInteractiveFetcher,
	})
	if err != nil {
		t.Fatalf("BuildHandler: %v", err)
	}

	inner := innerHandler(t, h)
	assertWrappedInChallengeAwareness(t, h)

	if _, ok := inner.(*auth.AuthorizationCodeHandler); !ok {
		t.Fatalf("inner handler = %T, want *auth.AuthorizationCodeHandler", inner)
	}
}

func TestBuildHandler_ClientCredentialsGrant(t *testing.T) {
	h, err := BuildHandler(HandlerConfig{
		ServerKey:    testServerKey,
		Grant:        GrantClientCredentials,
		ClientID:     testClientID,
		ClientSecret: "secret-1",
	})
	if err != nil {
		t.Fatalf("BuildHandler: %v", err)
	}

	assertWrappedInChallengeAwareness(t, h)

	inner := innerHandler(t, h)
	if _, ok := inner.(*extauth.ClientCredentialsHandler); !ok {
		t.Fatalf("inner handler = %T, want *extauth.ClientCredentialsHandler", inner)
	}
}

func TestBuildHandler_UnresolvedGrantFails(t *testing.T) {
	if _, err := BuildHandler(HandlerConfig{ServerKey: testServerKey, Grant: grantAuto}); err == nil {
		t.Fatal("expected an error for an unresolved grant")
	}
}

func TestBuildHandler_UsableCredentialRequiresRegisteredClient(t *testing.T) {
	store, err := NewFileStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = testClientID
	cred.AccessToken = testAccessToken
	cred.Expiry = time.Now().Add(time.Hour)

	if _, err := BuildHandler(HandlerConfig{
		ServerKey:          testServerKey,
		Grant:              GrantAuthorizationCode,
		InitialTokenSource: NewTokenSourceFromCredential(store, http.DefaultClient, cred),
	}); err == nil {
		t.Fatal("expected an error when InitialTokenSource is set without RegisteredClient")
	}
}

func assertWrappedInChallengeAwareness(t *testing.T, h auth.OAuthHandler) {
	t.Helper()

	if _, ok := h.(*challengeAwareHandler); !ok {
		t.Fatalf("handler = %T, want every shape wrapped in *challengeAwareHandler", h)
	}
}

// innerHandler unwraps h's *challengeAwareHandler, failing the test if h is
// not one.
func innerHandler(t *testing.T, h auth.OAuthHandler) auth.OAuthHandler {
	t.Helper()

	wrapped, ok := h.(*challengeAwareHandler)
	if !ok {
		t.Fatalf("handler = %T, want *challengeAwareHandler", h)
	}

	return wrapped.inner
}

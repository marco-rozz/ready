package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// ErrLoginRequired is returned by TokenSource.Token when no usable
// credential exists: the access token has expired and there is no refresh
// token to renew it, or the refresh attempt itself reports
// invalid_grant. internal/mcp maps it to its own
// ErrMCPOAuthLoginRequired sentinel via errors.Is, exactly as it already
// translates other SDK and network errors — this package defines no
// application-level sentinel of its own (the leaf-package constraint, D8).
var ErrLoginRequired = errors.New("oauth: login required")

// CredentialMeta bundles the Credential fields that never appear in the
// SDK's *oauth2.Config or *oauth2.Token — they come from the caller's own
// discovery (the design doc: ResourceURL, Issuer, Resource, and the RFC 7591
// registration-management fields) or from registration itself. NewFromExchange
// uses it to build a fresh Credential when the SDK invokes the
// NewTokenSource hook after a successful authorization-code exchange.
type CredentialMeta struct {
	ServerKey               string
	ResourceURL             string
	Issuer                  string
	Resource                string
	TokenEndpointAuthMethod string
	RedirectURI             string
	RegistrationAccessToken string
	RegistrationClientURI   string
	ClientSecretExpiresAt   time.Time
}

// TokenSource is the store-backed, self-persisting oauth2.TokenSource
// decorator described in the design doc
// (the mcp-oauth-authorization change design). It is safe for
// concurrent use (task 3.7).
type TokenSource struct {
	mu         sync.Mutex
	store      Store
	httpClient *http.Client
	serverKey  string
	cred       *Credential
}

// Guarantees at compile time that TokenSource implements oauth2.TokenSource.
var _ oauth2.TokenSource = (*TokenSource)(nil)

// NewTokenSourceFromCredential builds a TokenSource over an already-persisted
// Credential. This is the InitialTokenSource wiring (the design doc's first row,
// task 3.5): the daemon boots with a stored Credential and never performs
// its own authorization flow.
func NewTokenSourceFromCredential(store Store, httpClient *http.Client, cred *Credential) *TokenSource {
	c := *cred

	return &TokenSource{store: store, httpClient: httpClient, serverKey: cred.ServerKey, cred: &c}
}

// NewFromExchange builds and persists a fresh Credential from the result of
// a successful authorization-code exchange, then returns a TokenSource over
// it. It is the building block for the NewTokenSource closure handed to
// auth.AuthorizationCodeHandlerConfig (task 3.5): the caller supplies meta
// from its own discovery, since PRM/AS-metadata fields never appear in
// *oauth2.Config or *oauth2.Token.
func NewFromExchange(
	ctx context.Context,
	store Store,
	httpClient *http.Client,
	meta CredentialMeta,
	cfg *oauth2.Config,
	tok *oauth2.Token,
) (*TokenSource, error) {
	cred := NewCredential(meta.ServerKey)
	cred.ResourceURL = meta.ResourceURL
	cred.Issuer = meta.Issuer
	cred.TokenEndpoint = cfg.Endpoint.TokenURL
	cred.TokenEndpointAuthMethod = meta.TokenEndpointAuthMethod
	cred.Resource = meta.Resource
	cred.ClientID = cfg.ClientID
	cred.ClientSecret = cfg.ClientSecret
	cred.ClientSecretExpiresAt = meta.ClientSecretExpiresAt
	cred.RegistrationAccessToken = meta.RegistrationAccessToken
	cred.RegistrationClientURI = meta.RegistrationClientURI
	cred.RedirectURI = meta.RedirectURI
	cred.ApplyToken(tok)

	if err := store.Save(ctx, cred); err != nil {
		return nil, fmt.Errorf("failed to persist oauth credential for %q: %w", meta.ServerKey, err)
	}

	return NewTokenSourceFromCredential(store, httpClient, cred), nil
}

// Token implements oauth2.TokenSource per the design section: refresh (or detect
// login-required structurally) against the currently held Credential; on
// failure, reload the store once in case another process — typically
// `ready login` run out of band — wrote a newer Credential, and retry
// exactly once with it. It never logs per failure (task 3.6): Client.Connect
// invokes Token() twice per attempt, and a health-check ping invokes it
// again on every tick, so per-call logging here would flood the log with no
// new information — the daemon's boot loop and HealthMonitor own the single
// actionable message.
func (ts *TokenSource) Token() (*oauth2.Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	tok, err := ts.tokenLocked()
	if err == nil {
		return tok, nil
	}

	reloaded, reloadErr := ts.store.Load(context.Background(), ts.serverKey)
	if reloadErr != nil || reloaded == nil || !reloaded.UpdatedAt.After(ts.cred.UpdatedAt) {
		return nil, err
	}

	ts.cred = reloaded

	return ts.tokenLocked()
}

// tokenLocked returns a usable token for ts.cred, refreshing and persisting
// as needed. Caller must hold ts.mu. It performs no network I/O when the
// credential is structurally unusable (the design doc, task 3.4): an expired
// access token with no refresh token fails immediately with
// ErrLoginRequired, detected from the field itself rather than by matching
// an oauth2 error string.
func (ts *TokenSource) tokenLocked() (*oauth2.Token, error) {
	tok := ts.cred.Token()
	if tok.Valid() {
		return tok, nil
	}

	if ts.cred.RefreshToken == "" {
		return nil, ErrLoginRequired
	}

	refreshed, err := refreshToken(context.Background(), ts.httpClient, refreshParams{
		tokenEndpoint: ts.cred.TokenEndpoint,
		authMethod:    ts.cred.TokenEndpointAuthMethod,
		clientID:      ts.cred.ClientID,
		clientSecret:  ts.cred.ClientSecret,
		resource:      ts.cred.Resource,
	}, ts.cred.RefreshToken)
	if err != nil {
		return nil, err
	}

	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = ts.cred.RefreshToken // the server did not rotate it
	}

	if !ts.cred.SameToken(refreshed) {
		ts.persistLocked(refreshed)
	}

	return refreshed, nil
}

// persistLocked applies the registration write-path guard (the design doc, task
// 3.2): it re-reads the store before saving and, if the stored record's
// ClientID no longer matches the one this TokenSource was built for,
// abandons the write and adopts the stored record instead of overwriting a
// registration it did not create — the daemon has no business rewriting
// registration fields it did not create. The freshly refreshed token is
// still returned to the immediate caller regardless of which branch is
// taken: it was validly issued by the authorization server, so there is no
// reason to discard a working token for this one call. Caller must hold
// ts.mu.
func (ts *TokenSource) persistLocked(tok *oauth2.Token) {
	cur, err := ts.store.Load(context.Background(), ts.serverKey)
	if err != nil {
		return // best-effort: a store hiccup must not block token issuance
	}

	if cur == nil || cur.ClientID != ts.cred.ClientID {
		if cur != nil {
			ts.cred = cur
		}

		return
	}

	cur.ApplyToken(tok)
	if err := ts.store.Save(context.Background(), cur); err == nil {
		ts.cred = cur
	}
}

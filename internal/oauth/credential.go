// Package oauth owns the OAuth 2.0 credential lifecycle for MCP servers that
// require authorization: credential records, persistent storage, a
// self-persisting token source, handler construction, and the two
// authorization-code fetchers (interactive and non-interactive). This
// package is a leaf: it must not import internal/mcp. internal/mcp imports
// this package and translates oauth.ErrLoginRequired to its own
// ErrMCPOAuthLoginRequired sentinel, exactly as it already translates other
// SDK and network errors.
package oauth

import (
	"time"

	"golang.org/x/oauth2"
)

// credentialVersion is the current on-disk format version. Bump it and add
// an explicit migration if the shape of Credential changes in a
// backward-incompatible way — this record holds secrets, so "delete and
// re-login" is a worse remedy than for other on-disk formats in this repo.
const credentialVersion = 1

// Credential is the persisted record for one MCP server's OAuth state. The
// daemon builds its own *oauth2.Config for InitialTokenSource before any
// network I/O, so every field the daemon needs at boot must be on disk — see
// the design doc in the mcp-oauth-authorization change design for the
// rationale behind each field.
type Credential struct {
	// Version is the on-disk format version (credentialVersion).
	Version int `json:"version"`
	// ServerKey is the mcp_servers.<key> this credential belongs to.
	ServerKey string `json:"server_key"`
	// ResourceURL is the configured MCP server URL at login time. A mismatch
	// after a config edit produces an explanatory warning instead of an
	// inscrutable 401.
	ResourceURL string `json:"resource_url"`
	// Issuer is the authorization server's issuer identifier.
	Issuer string `json:"issuer"`
	// TokenEndpoint is the authorization server's token endpoint (RFC 8414).
	// Persisting it avoids a discovery round-trip at daemon boot.
	TokenEndpoint string `json:"token_endpoint"`
	// TokenEndpointAuthMethod is persisted as the RFC 7591 string (e.g.
	// "client_secret_post", "none"), never as the oauth2.AuthStyle int, which
	// is not a stable on-disk contract.
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	// Resource is the RFC 8707 resource indicator captured from the
	// Protected Resource Metadata at login/registration time.
	Resource string `json:"resource"`

	// ClientID and ClientSecret are the (possibly dynamically registered)
	// client credentials used to authenticate to the token endpoint.
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	// ClientSecretExpiresAt is zero when the secret never expires (RFC 7591
	// §3.2.1).
	ClientSecretExpiresAt time.Time `json:"client_secret_expires_at,omitempty"`

	// RegistrationAccessToken and RegistrationClientURI are the RFC 7592
	// registration-management fields, present only when the authorization
	// server returned them at Dynamic Client Registration. They enable
	// `ready login --forget` to deregister the client instead of abandoning
	// it, and cannot be recovered later.
	RegistrationAccessToken string `json:"registration_access_token,omitempty"`
	RegistrationClientURI   string `json:"registration_client_uri,omitempty"`

	// RedirectURI is the fixed loopback callback URI used at registration
	// and authorization time. Frozen into the persisted registration.
	RedirectURI string `json:"redirect_uri"`

	// Token fields, mirroring oauth2.Token.
	AccessToken  string    `json:"access_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`

	// GrantedScopes is diagnostic only. RFC 6749 §6 allows a refresh
	// request's scope parameter to narrow but never widen the token, and
	// golang.org/x/oauth2 does not send scope on refresh at all — wiring
	// this into a refresh request could therefore only ever shrink the
	// resulting token. It exists so `ready login --status` can show what was
	// actually granted; it MUST NOT reach any token request.
	GrantedScopes []string `json:"granted_scopes,omitempty"`

	// UpdatedAt is serialised with nanosecond precision (RFC3339Nano).
	// Second granularity would make two writes within the same second
	// indistinguishable and silently defeat the reload comparison used by
	// the store-backed token source.
	UpdatedAt time.Time `json:"updated_at"`
}

// NewCredential returns a Credential for serverKey with Version set to the
// current format version.
func NewCredential(serverKey string) *Credential {
	return &Credential{Version: credentialVersion, ServerKey: serverKey}
}

// Usable reports whether c would produce a token right now, without any
// network I/O: either the access token has not expired, or a refresh token
// is present to renew it. This is the single definition of "would this
// credential work right now", shared by the token source and by
// `ready login --status`. The boot path deliberately does not call it — see
// D9 for why the token source is the sole authority there.
func (c *Credential) Usable(now time.Time) bool {
	if c == nil || c.ClientID == "" {
		return false
	}

	if c.AccessToken != "" && (c.Expiry.IsZero() || c.Expiry.After(now)) {
		return true
	}

	return c.RefreshToken != ""
}

// Token returns an *oauth2.Token built from c's token fields. Scope is
// deliberately not populated: GrantedScopes is diagnostic only and must
// never reach a token request (see the GrantedScopes field comment).
func (c *Credential) Token() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  c.AccessToken,
		TokenType:    c.TokenType,
		RefreshToken: c.RefreshToken,
		Expiry:       c.Expiry,
	}
}

// ApplyToken copies tok's fields onto c and stamps UpdatedAt with the
// current time (nanosecond precision). It does not touch registration
// fields (ClientID, ClientSecret, RegistrationAccessToken, ...) — callers
// that need to update those do so directly, since the write-path guard
// treats token fields and registration fields differently.
func (c *Credential) ApplyToken(tok *oauth2.Token) {
	c.AccessToken = tok.AccessToken
	c.TokenType = tok.TokenType
	c.RefreshToken = tok.RefreshToken
	c.Expiry = tok.Expiry
	c.UpdatedAt = time.Now()
}

// SameToken reports whether c's token fields are identical to tok's. Used by
// the store-backed token source to decide whether a refresh actually
// rotated anything and is therefore worth persisting.
func (c *Credential) SameToken(tok *oauth2.Token) bool {
	if c == nil || tok == nil {
		return false
	}

	return c.AccessToken == tok.AccessToken &&
		c.RefreshToken == tok.RefreshToken &&
		c.Expiry.Equal(tok.Expiry)
}

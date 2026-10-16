package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
)

// schemeBearer, grantAuto, and errorCodeInvalidClient are shared literals
// used across this file (and its tests) to keep the wire-format strings in
// exactly one place.
const (
	schemeBearer           = "bearer"
	grantAuto              = "auto"
	errorCodeInvalidClient = "invalid_client"
)

// ErrNoAuthorizationRequired is returned by Probe when the server answers
// with a plain 200: no OAuth flow is needed for this URL (design task 5.1).
var ErrNoAuthorizationRequired = errors.New("oauth: server does not require authorization")

// Probe issues a lightweight request to serverURL and reports whether
// authorization is required. A 200 response means no authorization is
// required at all (ErrNoAuthorizationRequired); any other status returns
// the parsed WWW-Authenticate challenges for the discovery chain to
// continue from (design task 5.1).
func Probe(ctx context.Context, httpClient *http.Client, serverURL string) ([]oauthex.Challenge, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build probe request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe request to %q failed: %w", serverURL, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusOK {
		return nil, ErrNoAuthorizationRequired
	}

	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	if err != nil {
		return nil, fmt.Errorf("failed to parse WWW-Authenticate from %q (status %s): %w", serverURL, resp.Status, err)
	}

	return challenges, nil
}

// prmCandidate is one (metadataURL, resourceURL) pair to try when looking
// for Protected Resource Metadata.
type prmCandidate struct {
	metadataURL string
	resourceURL string
}

// protectedResourceMetadataURLs returns the URLs to try for PRM discovery,
// per the MCP specification's discovery-requirements section: the
// WWW-Authenticate-provided resource_metadata URL first (if any), then the
// well-known path scoped to the MCP endpoint's own path, then the well-known
// path at the origin's root. This mirrors the equivalent private helper in
// the SDK's auth package — duplicated here because discovery is performed
// by READY. itself, not by the SDK's handler.
func protectedResourceMetadataURLs(resourceMetadataURL, resourceURL string) []prmCandidate {
	var candidates []prmCandidate

	if resourceMetadataURL != "" {
		candidates = append(candidates, prmCandidate{metadataURL: resourceMetadataURL, resourceURL: resourceURL})
	}

	ru, err := url.Parse(resourceURL)
	if err != nil {
		return candidates
	}

	mu := *ru
	mu.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(ru.Path, "/")
	candidates = append(candidates, prmCandidate{metadataURL: mu.String(), resourceURL: resourceURL})

	mu.Path = "/.well-known/oauth-protected-resource"
	rootRU := *ru
	rootRU.Path = ""
	candidates = append(candidates, prmCandidate{metadataURL: mu.String(), resourceURL: rootRU.String()})

	return candidates
}

// resourceMetadataURLFromChallenges returns the resource_metadata challenge
// parameter, or "" if absent.
func resourceMetadataURLFromChallenges(challenges []oauthex.Challenge) string {
	for _, c := range challenges {
		if c.Scheme == schemeBearer && c.Params["resource_metadata"] != "" {
			return c.Params["resource_metadata"]
		}
	}

	return ""
}

// DiscoverProtectedResource resolves Protected Resource Metadata (RFC 9728)
// for serverURL, trying each candidate URL from protectedResourceMetadataURLs
// in order and falling back, per the 2025-03-26 spec, to treating the MCP
// server's own origin as the sole authorization server when none respond
// (design task 5.2).
func DiscoverProtectedResource(ctx context.Context, httpClient *http.Client, challenges []oauthex.Challenge, serverURL string) (*oauthex.ProtectedResourceMetadata, error) {
	candidates := protectedResourceMetadataURLs(resourceMetadataURLFromChallenges(challenges), serverURL)

	for _, c := range candidates {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, c.metadataURL, c.resourceURL, httpClient)
		if err != nil || prm == nil {
			continue
		}
		if len(prm.AuthorizationServers) == 0 {
			return nil, errors.New("protected resource metadata has no authorization servers specified")
		}

		return prm, nil
	}

	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse MCP server URL %q: %w", serverURL, err)
	}
	u.Path = ""

	return &oauthex.ProtectedResourceMetadata{AuthorizationServers: []string{u.String()}, Resource: serverURL}, nil
}

// discoveryEndpoints is the pre-metadata-discovery fallback for an
// authorization server that predates RFC 8414 support, per the MCP
// specification's 2025-03-26 fallback rule.
func discoveryEndpoints(issuer string) *oauthex.AuthServerMeta {
	return &oauthex.AuthServerMeta{
		Issuer:                issuer,
		AuthorizationEndpoint: issuer + "/authorize",
		TokenEndpoint:         issuer + "/token",
		RegistrationEndpoint:  issuer + "/register",
	}
}

// DiscoverAuthServer resolves Authorization Server Metadata (RFC 8414) for
// the first authorization server named in prm, falling back to
// discoveryEndpoints when no metadata document is found (design task 5.2).
func DiscoverAuthServer(ctx context.Context, httpClient *http.Client, prm *oauthex.ProtectedResourceMetadata) (*oauthex.AuthServerMeta, error) {
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("protected resource metadata has no authorization servers specified")
	}

	issuer := prm.AuthorizationServers[0]

	asm, err := auth.GetAuthServerMetadata(ctx, issuer, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get authorization server metadata for %q: %w", issuer, err)
	}
	if asm != nil {
		return asm, nil
	}

	return discoveryEndpoints(issuer), nil
}

// RegistrationResult is the outcome of a successful Dynamic Client
// Registration (RFC 7591), including the RFC 7592 registration-management
// fields the SDK's own oauthex.ClientRegistrationResponse does not expose —
// the reason this package performs the registration POST itself rather than
// delegating to oauthex.RegisterClient (the design doc, D12).
type RegistrationResult struct {
	ClientID                string
	ClientSecret            string
	ClientSecretExpiresAt   time.Time
	RegistrationAccessToken string
	RegistrationClientURI   string
}

// registrationRequest is the RFC 7591 request body: native application,
// authorization_code + refresh_token grants, PKCE-only ("none" token
// endpoint auth) — the shape appropriate for READY.'s fixed loopback
// redirect client (design task 5.3). There is deliberately no scopes field:
// scopes are server-determined.
type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ApplicationType         string   `json:"application_type"`
	ClientName              string   `json:"client_name,omitempty"`
}

// registrationResponse mirrors the RFC 7591 §3.2.1 success response,
// including the RFC 7592 registration-management fields.
type registrationResponse struct {
	ClientID                string      `json:"client_id"`
	ClientSecret            string      `json:"client_secret"`
	ClientSecretExpiresAt   json.Number `json:"client_secret_expires_at"`
	RegistrationAccessToken string      `json:"registration_access_token"`
	RegistrationClientURI   string      `json:"registration_client_uri"`
}

// registrationErrorResponse mirrors the RFC 7591 §3.2.2 error response.
type registrationErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// clientName is the value advertised to authorization servers during
// registration (RFC 7591's RECOMMENDED client_name).
const clientName = "READY. home agent"

// RegisterClient performs a one-time Dynamic Client Registration (RFC 7591)
// against registrationEndpoint for a native, PKCE-only client using
// redirectURI. Callers must only invoke this when the stored
// Credential has no client_id yet — repeating it registers a
// new, orphaned client at the authorization server every time.
func RegisterClient(ctx context.Context, httpClient *http.Client, registrationEndpoint, redirectURI string) (*RegistrationResult, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	reqBody := registrationRequest{
		RedirectURIs:            []string{redirectURI},
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{GrantAuthorizationCode, paramRefreshTokenJSON},
		ResponseTypes:           []string{"code"},
		ApplicationType:         "native",
		ClientName:              clientName,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registration request to %q failed: %w", registrationEndpoint, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read registration response: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errResp registrationErrorResponse
		_ = json.Unmarshal(body, &errResp) // best-effort

		return nil, fmt.Errorf("registration failed with status %s: %s (%s)", resp.Status, errResp.Error, errResp.ErrorDescription) // reflects a live server response
	}

	var reg registrationResponse
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("failed to parse registration response: %w", err)
	}
	if reg.ClientID == "" {
		return nil, errors.New("registration response is missing the required client_id field")
	}

	result := &RegistrationResult{
		ClientID:                reg.ClientID,
		ClientSecret:            reg.ClientSecret,
		RegistrationAccessToken: reg.RegistrationAccessToken,
		RegistrationClientURI:   reg.RegistrationClientURI,
	}

	if secs, convErr := reg.ClientSecretExpiresAt.Int64(); convErr == nil && secs > 0 {
		result.ClientSecretExpiresAt = time.Unix(secs, 0)
	}

	return result, nil
}

// Deregister removes a client registration at the authorization server per
// RFC 7592, used by `ready login --forget` when the registration-management
// fields are present (design task 5.7).
func Deregister(ctx context.Context, httpClient *http.Client, registrationClientURI, registrationAccessToken string) error {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, registrationClientURI, nil)
	if err != nil {
		return fmt.Errorf("failed to build deregistration request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+registrationAccessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("deregistration request to %q failed: %w", registrationClientURI, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deregistration failed with status %s", resp.Status) // reflects a live server response
	}

	return nil
}

// IsInvalidClient reports whether err is an *oauth2.RetrieveError carrying
// RFC 6749's invalid_client code — the structural signal (never a substring
// match) that the registration was repudiated by the
// authorization server and must be cleared so the next `ready login`
// re-registers.
func IsInvalidClient(err error) bool {
	var retrieveErr *oauth2.RetrieveError

	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == errorCodeInvalidClient
}

// ResolveGrant implements the design doc's grant resolution under "auto":
// client_credentials only when the operator configured both a client_id and
// a secret *and* the authorization server advertises the client_credentials
// grant type; authorization_code otherwise. A non-"auto" configuredGrant is
// returned unchanged — an operator with a pre-registered confidential
// client intended for the interactive flow sets grant explicitly.
func ResolveGrant(configuredGrant string, hasClientCredentials, authServerSupportsClientCredentials bool) string {
	if configuredGrant != grantAuto {
		return configuredGrant
	}

	if hasClientCredentials && authServerSupportsClientCredentials {
		return GrantClientCredentials
	}

	return GrantAuthorizationCode
}

// LogIfNoRefreshToken logs an explicit warning when tok carries no refresh
// token: the server will need a `ready login` at every access-token expiry
// and, per the design section, a daemon restart whenever that expiry happens while
// the daemon is down (design task 5.6). A zero-value logger is silent.
func LogIfNoRefreshToken(logger zerolog.Logger, serverKey string, tok *oauth2.Token) {
	if tok.RefreshToken != "" {
		return
	}

	logger.Warn().
		Str("server", serverKey).
		Msg("oauth login succeeded but the authorization server issued no refresh token: " +
			"a new `ready login` will be required at every access-token expiry, " +
			"and a daemon restart if that expiry happens while the daemon is down")
}

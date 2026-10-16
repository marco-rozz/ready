package oauth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// GrantAuthorizationCode and GrantClientCredentials are the two resolved
// grant values HandlerConfig.Grant accepts. Config validation
// (internal/config) also accepts "auto"; callers resolve it to one of these
// two before calling BuildHandler.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantClientCredentials = "client_credentials"
)

// HandlerConfig configures BuildHandler for a single MCP server. Exactly one
// of the three handler shapes in the design doc's table results, and the result
// is always wrapped in challengeAwareHandler. BuildHandler
// never performs Dynamic Client Registration itself: a
// caller that needs a client_id first registers it (section 5's
// RegisterClient) and supplies the result here.
type HandlerConfig struct {
	// ServerKey names the server for error messages and challengeAwareHandler.
	ServerKey string
	// Grant is the resolved grant (GrantAuthorizationCode or
	// GrantClientCredentials — never "auto").
	Grant string
	// HTTPClient carries the scoped header policy; the same
	// instance the caller hands to the MCP transport.
	HTTPClient *http.Client

	// --- GrantClientCredentials ---
	ClientID     string
	ClientSecret string

	// --- GrantAuthorizationCode ---
	// RegisteredClient is the client_id (+ optional secret) already
	// registered for this server. Required whenever InitialTokenSource or
	// Fetcher is set — the SDK's PreregisteredClient.Validate() rejects an
	// empty ClientID even when Authorize will never run.
	RegisteredClient *oauthex.ClientCredentials
	// RedirectURL is the fixed loopback callback URL. Required
	// alongside RegisteredClient.
	RedirectURL string
	// Fetcher performs the authorization step when the SDK calls Authorize.
	// `ready serve` supplies NonInteractiveFetcher; `ready login` supplies
	// InteractiveFetcher. Required whenever RegisteredClient is set.
	Fetcher auth.AuthorizationCodeFetcher
	// InitialTokenSource, when non-nil, is used directly (the design doc's first
	// row: a usable Credential already exists and the SDK need not call
	// Authorize at all). Takes precedence over a fresh Fetcher-driven flow.
	InitialTokenSource oauth2.TokenSource
	// NewTokenSource is invoked by the SDK only when Authorize actually runs
	// a fresh exchange (InitialTokenSource nil, Fetcher succeeds). Required
	// in that case; ignored otherwise.
	NewTokenSource func(context.Context, *oauth2.Config, *oauth2.Token) (oauth2.TokenSource, error)
}

// BuildHandler constructs the auth.OAuthHandler for cfg per the design section's
// table:
//
//   - GrantClientCredentials              -> extauth.ClientCredentialsHandler
//   - GrantAuthorizationCode, usable path -> auth.AuthorizationCodeHandler
//   - GrantAuthorizationCode, no usable path (neither InitialTokenSource nor
//     a RegisteredClient+Fetcher pair) -> loginRequiredHandler
//
// Every handler set RequestRefreshToken: true — without it
// offline_access is never requested and the "no refresh token" risk becomes
// self-inflicted. Every branch is wrapped in challengeAwareHandler before
// being returned, so all three shapes share one place that recognises
// step-up authorization.
func BuildHandler(cfg HandlerConfig) (auth.OAuthHandler, error) {
	var handler auth.OAuthHandler

	switch cfg.Grant {
	case GrantClientCredentials:
		h, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
			Credentials: &oauthex.ClientCredentials{
				ClientID:         cfg.ClientID,
				ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: cfg.ClientSecret},
			},
			HTTPClient: cfg.HTTPClient,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to build client_credentials handler for %q: %w", cfg.ServerKey, err)
		}

		handler = h

	case GrantAuthorizationCode:
		h, err := buildAuthorizationCodeHandler(cfg)
		if err != nil {
			return nil, err
		}

		handler = h

	default:
		return nil, fmt.Errorf("unresolved oauth grant %q for %q: BuildHandler requires a resolved grant (%s or %s)", // programmer error, not user-facing config
			cfg.Grant, cfg.ServerKey, GrantAuthorizationCode, GrantClientCredentials)
	}

	return wrapWithChallengeAwareness(cfg.ServerKey, handler), nil
}

// buildAuthorizationCodeHandler resolves the second and third rows of design
// D2's table.
func buildAuthorizationCodeHandler(cfg HandlerConfig) (auth.OAuthHandler, error) {
	haveUsableCredential := cfg.InitialTokenSource != nil
	haveFreshFlow := cfg.RegisteredClient != nil && cfg.Fetcher != nil

	if !haveUsableCredential && !haveFreshFlow {
		return newLoginRequiredHandler(cfg.ServerKey), nil
	}

	if cfg.RegisteredClient == nil {
		return nil, fmt.Errorf(
			"missing registered client for %q: BuildHandler requires RegisteredClient whenever InitialTokenSource or Fetcher is set", // programmer error, not user-facing config
			cfg.ServerKey,
		)
	}

	fetcher := cfg.Fetcher
	if fetcher == nil {
		// A usable Credential exists; the SDK will not call Authorize, but
		// NewAuthorizationCodeHandler still requires a non-nil fetcher.
		fetcher = NonInteractiveFetcher
	}

	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      cfg.RegisteredClient,
		RedirectURL:              cfg.RedirectURL,
		AuthorizationCodeFetcher: fetcher,
		RequestRefreshToken:      true,
		Client:                   cfg.HTTPClient,
		InitialTokenSource:       cfg.InitialTokenSource,
		NewTokenSource:           cfg.NewTokenSource,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build authorization_code handler for %q: %w", cfg.ServerKey, err)
	}

	return h, nil
}

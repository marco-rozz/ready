package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// maxTokenResponseBytes bounds how much of a token-endpoint response body is
// read into memory, mirroring the limits oauthex applies to discovery
// responses.
const maxTokenResponseBytes = 1 << 20

// paramRefreshTokenJSON is the RFC 6749 wire name for the refresh token,
// shared by the request form field and the response JSON tag (and reused by
// this package's tests) to avoid three independent literals drifting apart.
const paramRefreshTokenJSON = "refresh_token"

// refreshParams is everything a manual refresh request needs, captured from
// a Credential at the point a refresh is attempted.
type refreshParams struct {
	tokenEndpoint string
	// authMethod is the RFC 7591 string (e.g. "client_secret_post",
	// "client_secret_basic", "none"), never an oauth2.AuthStyle int.
	authMethod   string
	clientID     string
	clientSecret string
	// resource is the RFC 8707 §2.2 resource indicator. Empty means omit —
	// not every authorization server expects it.
	resource string
}

// tokenSuccessResponse mirrors the RFC 6749 §5.1 token endpoint success
// response fields this package needs.
type tokenSuccessResponse struct {
	AccessToken  string      `json:"access_token"`
	TokenType    string      `json:"token_type"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    json.Number `json:"expires_in"`
}

// tokenErrorResponse mirrors the RFC 6749 §5.2 token endpoint error
// response.
type tokenErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	ErrorURI         string `json:"error_uri"`
}

// refreshToken performs a manual "grant_type=refresh_token" request against
// params.tokenEndpoint, including the RFC 8707 §2.2 resource indicator when
// params.resource is non-empty. golang.org/x/oauth2's own tokenRefresher
// never sends resource on refresh (the design doc, task 3.3), so this
// reimplements just the refresh leg from first principles: no retry, no
// caching — TokenSource owns both. On a non-2xx response it returns an
// *oauth2.RetrieveError so callers can classify by ErrorCode
// exactly as they would for the SDK's own authorization-code exchange.
func refreshToken(ctx context.Context, httpClient *http.Client, params refreshParams, refreshTok string) (*oauth2.Token, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	form := url.Values{
		"grant_type":          {paramRefreshTokenJSON},
		paramRefreshTokenJSON: {refreshTok},
	}
	if params.resource != "" {
		form.Set("resource", params.resource)
	}

	var useBasicAuth bool

	switch params.authMethod {
	case "client_secret_basic":
		useBasicAuth = true
	default: // "client_secret_post", "none", "" (unknown): send in the body.
		form.Set("client_id", params.clientID)
		if params.clientSecret != "" {
			form.Set("client_secret", params.clientSecret)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, params.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build oauth2 refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	if useBasicAuth {
		req.SetBasicAuth(url.QueryEscape(params.clientID), url.QueryEscape(params.clientSecret))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth2 refresh request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read oauth2 refresh response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp tokenErrorResponse
		_ = json.Unmarshal(body, &errResp) // best-effort: a malformed body still yields a RetrieveError below

		return nil, &oauth2.RetrieveError{
			Response:         resp,
			Body:             body,
			ErrorCode:        errResp.Error,
			ErrorDescription: errResp.ErrorDescription,
			ErrorURI:         errResp.ErrorURI,
		}
	}

	var tr tokenSuccessResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("failed to parse oauth2 refresh response: %w", err)
	}

	if tr.AccessToken == "" {
		return nil, fmt.Errorf("oauth2 refresh: token endpoint %q returned no access_token", params.tokenEndpoint) // wraps a live server response, not a static sentinel
	}

	tok := &oauth2.Token{
		AccessToken:  tr.AccessToken,
		TokenType:    tr.TokenType,
		RefreshToken: tr.RefreshToken,
	}

	if secs, convErr := tr.ExpiresIn.Int64(); convErr == nil && secs > 0 {
		tok.Expiry = time.Now().Add(time.Duration(secs) * time.Second)
	}

	return tok, nil
}

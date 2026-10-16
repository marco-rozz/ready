package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// These values are the public Codex client metadata. They are deliberately
	// fixed in production: YAML may redirect model traffic, never credentials.
	ClientID              = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthorizationEndpoint = "https://auth.openai.com/oauth/authorize"
	TokenEndpoint         = "https://auth.openai.com/oauth/token" //nolint:gosec // public endpoint, not a credential
	DeviceCodeEndpoint    = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	DeviceTokenEndpoint   = "https://auth.openai.com/api/accounts/deviceauth/token" //nolint:gosec // public endpoint, not a credential
	RedirectURI           = "http://localhost:1455/auth/callback"
	DeviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
	Scope                 = "openid profile email offline_access"
	Originator            = "ready"
	DefaultFlowTimeout    = 5 * time.Minute
	DefaultDeviceTimeout  = 15 * time.Minute
	defaultPollInterval   = 5 * time.Second
	maxErrorBody          = 64 << 10
	formGrantType         = "grant_type"
	formClientID          = "client_id"
)

var ErrAuthorizationPending = errors.New("codex oauth: authorization pending")

// Endpoints is injectable only for deterministic tests. Production callers
// use DefaultEndpoints; configuration cannot override authentication hosts.
type Endpoints struct {
	Authorize   string
	Token       string
	DeviceCode  string
	DeviceToken string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		Authorize: AuthorizationEndpoint, Token: TokenEndpoint,
		DeviceCode: DeviceCodeEndpoint, DeviceToken: DeviceTokenEndpoint,
	}
}

type Client struct {
	HTTPClient *http.Client
	Endpoints  Endpoints
	Now        func() time.Time
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{HTTPClient: httpClient, Endpoints: DefaultEndpoints(), Now: time.Now}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}

// AuthorizationURL constructs the public-client browser authorization URL.
func (c *Client) AuthorizationURL(state, challenge string) (string, error) {
	endpoint := c.Endpoints.Authorize
	if endpoint == "" {
		endpoint = AuthorizationEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("build codex authorization URL: invalid endpoint")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", Scope)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", Originator)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c *Client) ExchangeCode(ctx context.Context, providerKey, code, verifier string) (*Credential, error) {
	return c.exchange(ctx, providerKey, url.Values{
		formGrantType:   {"authorization_code"},
		formClientID:    {ClientID},
		"code":          {code},
		"redirect_uri":  {RedirectURI},
		"code_verifier": {verifier},
	}, "")
}

func (c *Client) exchangeDeviceCode(ctx context.Context, providerKey, code, verifier string) (*Credential, error) {
	return c.exchange(ctx, providerKey, url.Values{
		formGrantType:   {"authorization_code"},
		formClientID:    {ClientID},
		"code":          {code},
		"redirect_uri":  {DeviceRedirectURI},
		"code_verifier": {verifier},
	}, "")
}

func (c *Client) Refresh(ctx context.Context, providerKey, refreshToken string) (*Credential, error) {
	return c.exchange(ctx, providerKey, url.Values{
		formGrantType:   {"refresh_token"},
		formClientID:    {ClientID},
		"refresh_token": {refreshToken},
	}, refreshToken)
}

func (c *Client) exchange(ctx context.Context, providerKey string, form url.Values, previousRefresh string) (*Credential, error) {
	endpoint := c.Endpoints.Token
	if endpoint == "" {
		endpoint = TokenEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create codex token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, fmt.Errorf("read codex token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, sanitizedHTTPError("codex token request", resp.StatusCode, body)
	}
	var out tokenResponse
	if decodeErr := json.Unmarshal(body, &out); decodeErr != nil {
		return nil, fmt.Errorf("decode codex token response: malformed JSON")
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return nil, fmt.Errorf("decode codex token response: required fields are missing")
	}
	if out.RefreshToken == "" {
		out.RefreshToken = previousRefresh
	}
	if out.RefreshToken == "" {
		return nil, fmt.Errorf("decode codex token response: refresh token is missing")
	}
	accountID, err := ExtractAccountID(out.AccessToken)
	if err != nil {
		return nil, err
	}
	cred := NewCredential(providerKey)
	cred.AccessToken = out.AccessToken
	cred.RefreshToken = out.RefreshToken
	cred.Expiry = c.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	cred.AccountID = accountID
	cred.UpdatedAt = c.now().UTC()

	return cred, nil
}

type oauthError struct {
	Error string `json:"error"`
}

func sanitizedHTTPError(operation string, status int, body []byte) error {
	var payload oauthError
	_ = json.Unmarshal(body, &payload)
	if payload.Error != "" {
		return fmt.Errorf("%s failed: HTTP %d code=%s", operation, status, safeCode(payload.Error))
	}

	return fmt.Errorf("%s failed: HTTP %d", operation, status)
}

func safeCode(v string) string {
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}

	return b.String()
}

type DeviceAuthorization struct {
	DeviceAuthID    string
	UserCode        string
	VerificationURI string
	Interval        time.Duration
	ExpiresAt       time.Time
}

type deviceStartResponse struct {
	DeviceAuthID    string `json:"device_auth_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int64  `json:"interval"`
	ExpiresIn       int64  `json:"expires_in"`
}

func (c *Client) StartDeviceAuthorization(ctx context.Context) (*DeviceAuthorization, error) {
	endpoint := c.Endpoints.DeviceCode
	if endpoint == "" {
		endpoint = DeviceCodeEndpoint
	}
	body, err := json.Marshal(map[string]string{formClientID: ClientID})
	if err != nil {
		return nil, fmt.Errorf("encode device authorization request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create device authorization request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device authorization request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, fmt.Errorf("read device authorization response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, sanitizedHTTPError("device authorization request", resp.StatusCode, raw)
	}
	var out deviceStartResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.DeviceAuthID == "" || out.UserCode == "" || out.VerificationURI == "" {
		return nil, fmt.Errorf("decode device authorization response: required fields are missing")
	}
	interval := time.Duration(out.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	expires := time.Duration(out.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = DefaultDeviceTimeout
	}

	return &DeviceAuthorization{
		DeviceAuthID: out.DeviceAuthID, UserCode: out.UserCode,
		VerificationURI: out.VerificationURI, Interval: interval, ExpiresAt: c.now().Add(expires),
	}, nil
}

type devicePollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	Code              string `json:"code"`
	CodeVerifier      string `json:"code_verifier"`
	Error             string `json:"error"`
	Interval          int64  `json:"interval"`
}

func (c *Client) PollDeviceAuthorization(ctx context.Context, providerKey string, auth *DeviceAuthorization) (*Credential, error) {
	if auth == nil {
		return nil, fmt.Errorf("poll device authorization: device state is required")
	}
	interval := auth.Interval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	for {
		if !auth.ExpiresAt.IsZero() && !c.now().Before(auth.ExpiresAt) {
			return nil, fmt.Errorf("device authorization expired: %w", context.DeadlineExceeded)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, fmt.Errorf("poll device authorization: %w", ctx.Err())
		case <-timer.C:
		}

		out, status, err := c.pollDeviceOnce(ctx, auth)
		if err != nil {
			return nil, err
		}
		switch out.Error {
		case "authorization_pending", "pending", "":
			if out.AuthorizationCode == "" && out.Code == "" {
				continue
			}
		case "slow_down":
			if out.Interval > 0 {
				interval = time.Duration(out.Interval) * time.Second
			} else {
				interval += time.Second
			}

			continue
		case "expired_token", "access_denied":
			return nil, fmt.Errorf("device authorization failed: HTTP %d code=%s", status, safeCode(out.Error))
		default:
			return nil, fmt.Errorf("device authorization failed: HTTP %d code=%s", status, safeCode(out.Error))
		}

		code := out.AuthorizationCode
		if code == "" {
			code = out.Code
		}
		if code == "" || out.CodeVerifier == "" {
			return nil, fmt.Errorf("decode device authorization response: required fields are missing")
		}

		return c.exchangeDeviceCode(ctx, providerKey, code, out.CodeVerifier)
	}
}

func (c *Client) pollDeviceOnce(ctx context.Context, auth *DeviceAuthorization) (devicePollResponse, int, error) {
	endpoint := c.Endpoints.DeviceToken
	if endpoint == "" {
		endpoint = DeviceTokenEndpoint
	}
	payload, err := json.Marshal(map[string]string{
		"device_auth_id": auth.DeviceAuthID,
		"user_code":      auth.UserCode,
		formClientID:     ClientID,
	})
	if err != nil {
		return devicePollResponse{}, 0, fmt.Errorf("encode device token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return devicePollResponse{}, 0, fmt.Errorf("create device token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return devicePollResponse{}, 0, fmt.Errorf("device token request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return devicePollResponse{}, resp.StatusCode, fmt.Errorf("read device token response: %w", err)
	}
	var out devicePollResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return devicePollResponse{}, resp.StatusCode, fmt.Errorf("decode device token response: malformed JSON")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Error == "authorization_pending" || out.Error == "slow_down" {
			return out, resp.StatusCode, nil
		}
		if out.Error == "" {
			out.Error = "http_" + strconv.Itoa(resp.StatusCode)
		}
	}

	return out, resp.StatusCode, nil
}

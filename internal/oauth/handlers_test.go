package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

func emptyBody() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}

func TestLoginRequiredHandler(t *testing.T) {
	h := newLoginRequiredHandler(testServerKey)

	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if _, tokErr := ts.Token(); !errors.Is(tokErr, ErrLoginRequired) {
		t.Fatalf("TokenSource().Token() error = %v, want ErrLoginRequired", tokErr)
	}

	resp := &http.Response{StatusCode: http.StatusUnauthorized, Body: emptyBody()}
	authErr := h.Authorize(context.Background(), &http.Request{}, resp)
	if !errors.Is(authErr, ErrLoginRequired) {
		t.Fatalf("Authorize error = %v, want ErrLoginRequired", authErr)
	}
	if !strings.Contains(authErr.Error(), testServerKey) || !strings.Contains(authErr.Error(), "ready login srv") {
		t.Fatalf("Authorize error %q should name the server and remediation command", authErr.Error())
	}
}

// countingHandler records how many times each method was invoked, and how
// many HTTP requests it issued to a probe endpoint, so tests can assert
// challengeAwareHandler either short-circuits (no delegation) or delegates.
type countingHandler struct {
	authorizeCalls int
	probeURL       string
	probeCalled    *bool
}

var _ auth.OAuthHandler = (*countingHandler)(nil)

func (c *countingHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return nil, nil //nolint:nilnil // test double; the interface allows a nil token source
}

func (c *countingHandler) Authorize(ctx context.Context, _ *http.Request, resp *http.Response) error {
	c.authorizeCalls++
	if c.probeURL != "" {
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.probeURL, nil); err == nil {
			if r, doErr := http.DefaultClient.Do(req); doErr == nil {
				*c.probeCalled = true
				_ = r.Body.Close()
			}
		}
	}
	_ = resp.Body.Close()

	return nil
}

func challengeResponse(status int, wwwAuthenticate string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Www-Authenticate": []string{wwwAuthenticate}},
		Body:       emptyBody(),
	}
}

func TestChallengeAwareHandler_InsufficientScopeShortCircuits(t *testing.T) {
	probeCalled := false
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	inner := &countingHandler{probeURL: probeSrv.URL, probeCalled: &probeCalled}
	h := wrapWithChallengeAwareness(testServerKey, inner)

	resp := challengeResponse(http.StatusForbidden, `Bearer error="insufficient_scope", scope="admin"`)
	defer func() { _ = resp.Body.Close() }()

	err := h.Authorize(context.Background(), &http.Request{}, resp)

	if err == nil {
		t.Fatal("expected an explanatory error")
	}
	if !strings.Contains(err.Error(), testServerKey) || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("error %q should name the server and the required scope", err.Error())
	}
	if inner.authorizeCalls != 0 {
		t.Fatalf("inner.Authorize was called %d times, want 0 (no delegation)", inner.authorizeCalls)
	}
	if probeCalled {
		t.Fatal("no HTTP request should have been issued by the inner handler")
	}
}

func TestChallengeAwareHandler_DelegatesNonInsufficientScope403(t *testing.T) {
	inner := &countingHandler{}
	h := wrapWithChallengeAwareness(testServerKey, inner)

	resp := challengeResponse(http.StatusForbidden, `Bearer error="invalid_token"`)
	defer func() { _ = resp.Body.Close() }()

	if err := h.Authorize(context.Background(), &http.Request{}, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.authorizeCalls != 1 {
		t.Fatalf("inner.Authorize was called %d times, want 1 (delegated)", inner.authorizeCalls)
	}
}

func TestChallengeAwareHandler_Delegates401(t *testing.T) {
	inner := &countingHandler{}
	h := wrapWithChallengeAwareness(testServerKey, inner)

	resp := challengeResponse(http.StatusUnauthorized, `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`)
	defer func() { _ = resp.Body.Close() }()

	if err := h.Authorize(context.Background(), &http.Request{}, resp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inner.authorizeCalls != 1 {
		t.Fatalf("inner.Authorize was called %d times, want 1 (delegated)", inner.authorizeCalls)
	}
}

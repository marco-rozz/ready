package codex

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/marco-rozz/ready/internal/codexauth"
)

const schemeHTTPS = "https"

// authTransport applies secrets only to the configured model origin. A 401
// at the HTTP-response boundary occurs before any SSE content/tool effect, so
// exactly one refresh/replay is safe; all other retries belong to READY's
// orchestrator.
type authTransport struct {
	base   http.RoundTripper
	origin *url.URL
	source codexauth.AccessSource
}

var _ http.RoundTripper = (*authTransport)(nil)

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req, false)
}

func (t *authTransport) roundTrip(req *http.Request, replay bool) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if !sameOrigin(t.origin, clone.URL) {
		clone.Header.Del("Authorization")
		clone.Header.Del("chatgpt-account-id")

		return t.base.RoundTrip(clone)
	}
	access, err := t.source.Access(req.Context())
	if err != nil {
		return nil, err
	}
	clone.Header.Set("Authorization", "Bearer "+access.Token)
	clone.Header.Set("chatgpt-account-id", access.AccountID)
	clone.Header.Set("originator", codexauth.Originator)
	clone.Header.Set("User-Agent", readyUserAgent)
	clone.Header.Set("OpenAI-Beta", "responses=experimental")
	clone.Header.Set("Accept", "text/event-stream")
	clone.Header.Set("Content-Type", "application/json")
	resp, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, fmt.Errorf("codex transport request failed: %w", err)
	}
	if resp.StatusCode != http.StatusUnauthorized || replay {
		return resp, nil
	}
	_ = resp.Body.Close()
	if req.GetBody == nil {
		return nil, fmt.Errorf("codex transport authentication failed: HTTP 401 unauthorized")
	}
	t.source.Invalidate(access.Token)
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("codex transport authentication replay failed: %w", err)
	}
	retry := req.Clone(req.Context())
	retry.Body = body

	return t.roundTrip(retry, true)
}

func sameOrigin(origin, target *url.URL) bool {
	if origin == nil || target == nil || !strings.EqualFold(origin.Hostname(), target.Hostname()) {
		return false
	}
	if origin.Scheme == schemeHTTPS && target.Scheme != schemeHTTPS {
		return false
	}

	return effectivePort(origin) == effectivePort(target)
}

func effectivePort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == schemeHTTPS {
		return "443"
	}

	return "80"
}

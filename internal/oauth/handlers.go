package oauth

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// loginRequiredHandler implements auth.OAuthHandler for a server with no
// usable credential (the design doc's second row): its TokenSource returns a
// token source that always fails, and its Authorize returns the same error
// immediately. It never parses WWW-Authenticate — that responsibility
// belongs entirely to challengeAwareHandler — and it performs
// no discovery, no registration, and no fetcher call, so the daemon can
// never trigger a client registration through this path.
type loginRequiredHandler struct {
	serverKey string
}

// Guarantees at compile time that loginRequiredHandler implements auth.OAuthHandler.
var _ auth.OAuthHandler = (*loginRequiredHandler)(nil)

// newLoginRequiredHandler returns a handler for serverKey whose every
// operation fails with an error naming the server and the remediation
// command (the design doc's 401 message).
func newLoginRequiredHandler(serverKey string) *loginRequiredHandler {
	return &loginRequiredHandler{serverKey: serverKey}
}

func (h *loginRequiredHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return &failingTokenSource{err: h.err()}, nil
}

func (h *loginRequiredHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	return h.err()
}

func (h *loginRequiredHandler) err() error {
	return fmt.Errorf("%w: no usable credential for %q: run `ready login %s`", ErrLoginRequired, h.serverKey, h.serverKey)
}

// failingTokenSource always returns the same error and never a token. It is
// deliberately not nil-valued: a nil oauth2.TokenSource makes the transport
// send an unauthenticated request and collect a pointless 401, whereas a
// failing one aborts locally with no network I/O (the design doc, context on
// setMCPHeaders).
type failingTokenSource struct {
	err error
}

var _ oauth2.TokenSource = (*failingTokenSource)(nil)

func (f *failingTokenSource) Token() (*oauth2.Token, error) {
	return nil, f.err
}

// challengeAwareHandler decorates any auth.OAuthHandler and owns all
// WWW-Authenticate reading. Its Authorize inspects the failed
// response before delegating: a 403 carrying error="insufficient_scope"
// returns an explanatory error immediately, without delegating — no PRM or
// AS-metadata fetch happens, consistent with the "fails locally" posture
// applied elsewhere. Every other response — including a plain
// 401 — is delegated to the inner handler unchanged, since a
// 403/insufficient_scope can only occur when a token was actually presented
// (i.e. the inner handler is the SDK's own AuthorizationCodeHandler or
// ClientCredentialsHandler); loginRequiredHandler is mounted precisely when
// no credential exists, and a server with no token answers 401.
type challengeAwareHandler struct {
	serverKey string
	inner     auth.OAuthHandler
}

// Guarantees at compile time that challengeAwareHandler implements auth.OAuthHandler.
var _ auth.OAuthHandler = (*challengeAwareHandler)(nil)

// wrapWithChallengeAwareness wraps inner so every handler shape (the design doc's
// three rows) shares one place that recognises step-up authorization
// . It is not a fourth handler shape.
func wrapWithChallengeAwareness(serverKey string, inner auth.OAuthHandler) auth.OAuthHandler {
	return &challengeAwareHandler{serverKey: serverKey, inner: inner}
}

func (h *challengeAwareHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.inner.TokenSource(ctx)
}

func (h *challengeAwareHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden {
		challenges, err := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
		if err == nil {
			if scope, ok := insufficientScope(challenges); ok {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()

				return fmt.Errorf( // user/operator-facing diagnostic, not a sentinel other code branches on
					"%s requires scope %q; READY. cannot request additional scopes (step-up authorization is not supported)",
					h.serverKey, scope,
				)
			}
		}
	}

	return h.inner.Authorize(ctx, req, resp)
}

// insufficientScope reports whether challenges contains a Bearer challenge
// with error="insufficient_scope", returning the requested scope when
// present.
func insufficientScope(challenges []oauthex.Challenge) (scope string, ok bool) {
	for _, c := range challenges {
		if c.Scheme != schemeBearer {
			continue
		}
		if c.Params["error"] != "insufficient_scope" {
			continue
		}

		return c.Params["scope"], true
	}

	return "", false
}

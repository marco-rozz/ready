package llm

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"sync"
)

// The OpenCode "Go" subscription gateway (https://opencode.ai/zen/go/v1)
// rejects — or, on some endpoints, 500s — requests that do not carry an
// `x-opencode-session` header (https://opencode.ai/docs/go/#where-can-i-use-it).
// It also asks clients not to identify themselves with generic SDK User-Agent
// strings. eino-ext exposes no per-provider custom-header knob on the openai
// (chat_completions) or claude (anthropic) components, so the header injection
// is done here, at provider construction time, for every `provider: opencode`
// instance:
//   - chat_completions / anthropic — via an http.Client whose RoundTripper sets
//     the headers (the components' HTTPClient config field);
//   - google — via genai HTTPOptions.Headers;
//   - responses — via agenticopenai ResponsesConfig.CustomHeaders.
//
// The plain Zen gateway (…/zen/v1) ignores unknown headers, so unconditional
// injection is harmless there and keeps the builders simple.

const (
	// zenSessionHeader is the session-tracking header the OpenCode Go gateway
	// requires on every request.
	zenSessionHeader = "x-opencode-session"

	// zenUserAgent identifies READY to the OpenCode gateway (which asks
	// clients not to present generic SDK User-Agent strings).
	zenUserAgent = "ready-agent/1.0"
)

// zenSessionID returns the process-wide OpenCode session identifier: a UUID v4
// generated once at first use. A daemon run equals one "conversation" for the
// gateway, mirroring how the OpenCode CLI itself scopes a session; every
// opencode provider instance in the candidate chain shares it.
//
//nolint:gochecknoglobals // intentional process-wide singleton: one session id per daemon run, shared by every opencode candidate
var zenSessionID = sync.OnceValue(func() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// deterministic (if embarrassing) session id rather than failing boot.
		return "ready-degraded-session"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4 version bits
	b[8] = (b[8] & 0x3f) | 0x80 // UUID v4 variant bits

	return fmt.Sprintf("ready-%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
})

// zenSessionHTTPClient returns an *http.Client whose RoundTripper injects the
// OpenCode session headers into every request. Used by the chat_completions
// (openai component) and anthropic (claude component) builders; the timeout is
// intentionally unset so each component keeps its own library default.
func zenSessionHTTPClient() *http.Client {
	// H5 (D3): base = stream-safe transport (dial/TLS/header deadlines, no
	// whole-request timeout) so long streams are bounded only by the request
	// context.
	return &http.Client{
		Transport: &zenSessionTransport{base: streamSafeTransport()},
	}
}

// zenSessionTransport is an http.RoundTripper that stamps every request with
// the OpenCode session id and READY's User-Agent. Headers already present on
// the request are never replaced.
type zenSessionTransport struct {
	base http.RoundTripper
}

func (t *zenSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqCopy := req.Clone(req.Context())
	if reqCopy.Header.Get(zenSessionHeader) == "" {
		reqCopy.Header.Set(zenSessionHeader, zenSessionID())
	}
	if reqCopy.Header.Get("User-Agent") == "" {
		reqCopy.Header.Set("User-Agent", zenUserAgent)
	}

	return t.base.RoundTrip(reqCopy)
}

// zenSessionCustomHeaders returns the session headers as a plain map, for
// components that accept custom headers natively (agenticopenai's
// ResponsesConfig.CustomHeaders).
func zenSessionCustomHeaders() map[string]string {
	return map[string]string{
		zenSessionHeader: zenSessionID(),
		"User-Agent":     zenUserAgent,
	}
}

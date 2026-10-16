package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
)

const (
	testAuthHeader = "Authorization"
	testBearerT    = "Bearer T"
)

// roundTripperFunc adapts a function to http.RoundTripper for tests that need
// to inspect the request headerRoundTripper hands to its base transport
// without making a real network call.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// capturedHeaders drives headerRoundTripper.RoundTrip directly (bypassing any
// real network I/O) and returns the header set observed by its base
// transport, so scope decisions can be asserted without flaky DNS/dial
// dependencies.
func capturedHeaders(t *testing.T, cfg *config.MCPServerConfig, reqURL string, existing map[string]string) http.Header {
	t.Helper()

	rt := newHeaderRoundTripper(cfg)

	var captured http.Header
	rt.base = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		captured = req.Header

		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	for k, v := range existing {
		req.Header.Set(k, v)
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return captured
}

// getViaClient issues a GET through client using NewRequestWithContext and
// closes the response body before returning.
func getViaClient(t *testing.T, client *http.Client, reqURL string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, reqURL, nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
}

// TestHeaderScope_CrossHostRedirectDropsHeader is spec scenario 3.1: a
// same-host request carries the configured header, but the server's redirect
// to a different host does not receive it. The "different host" is
// "localhost" pointed at the origin server's own port via a rewritten
// Location — same machine, but a textually distinct hostname from
// "127.0.0.1", which is exactly what an attacker-controlled redirect target
// looks like to the RoundTripper's scope check.
func TestHeaderScope_CrossHostRedirectDropsHeader(t *testing.T) {
	var originHeader, destHeader string

	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			originHeader = r.Header.Get(testAuthHeader)

			addr, ok := origin.Listener.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatalf("unexpected listener address type %T", origin.Listener.Addr())
			}

			http.Redirect(w, r, fmt.Sprintf("http://localhost:%d/dest", addr.Port), http.StatusFound)
		case "/dest":
			destHeader = r.Header.Get(testAuthHeader)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer origin.Close()

	cfg := &config.MCPServerConfig{URL: origin.URL, Headers: map[string]string{testAuthHeader: "Bearer secret"}}
	client := NewScopedHTTPClient(cfg)

	getViaClient(t, client, origin.URL+"/start")

	if originHeader != "Bearer secret" {
		t.Errorf("expected same-host request to carry the header, got %q", originHeader)
	}
	if destHeader != "" {
		t.Errorf("expected cross-host redirect to drop the header, got %q", destHeader)
	}
}

// TestHeaderScope_SamePortDifferentPortHeaderApplied is spec scenario 3.2 /
// the design doc: the header must not depend on the port. Nothing needs to listen
// on the configured port; inScope only inspects the URL.
func TestHeaderScope_SamePortDifferentPortHeaderApplied(t *testing.T) {
	var got string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Test")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	cfg := &config.MCPServerConfig{URL: "http://127.0.0.1:1/", Headers: map[string]string{"X-Test": "v"}}
	client := NewScopedHTTPClient(cfg)

	getViaClient(t, client, target.URL)

	if got != "v" {
		t.Errorf("expected header to be applied across a port change on the same host, got %q", got)
	}
}

// TestHeaderScope_SubdomainInScope is spec scenario 3.3.
func TestHeaderScope_SubdomainInScope(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "https://example.com", Headers: map[string]string{testAuthHeader: testBearerT}}

	h := capturedHeaders(t, cfg, "https://api.example.com/mcp", nil)
	if h.Get(testAuthHeader) != testBearerT {
		t.Errorf("expected header on a subdomain of the configured host, got %q", h.Get(testAuthHeader))
	}
}

// TestHeaderScope_SchemeDowngradeDropsHeader is spec scenario 3.4.
func TestHeaderScope_SchemeDowngradeDropsHeader(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "https://example.com", Headers: map[string]string{testAuthHeader: testBearerT}}

	h := capturedHeaders(t, cfg, "http://example.com/mcp", nil)
	if h.Get(testAuthHeader) != "" {
		t.Errorf("expected a same-host https->http downgrade to drop the header, got %q", h.Get(testAuthHeader))
	}
}

// TestHeaderScope_PlaintextServerUnaffected is spec scenario 3.5: an http://
// configured server behaves exactly as before this change.
func TestHeaderScope_PlaintextServerUnaffected(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "http://homeassistant.local:8123", Headers: map[string]string{testAuthHeader: testBearerT}}

	h := capturedHeaders(t, cfg, "http://homeassistant.local:8123/api/mcp", nil)
	if h.Get(testAuthHeader) != testBearerT {
		t.Errorf("expected header on a plaintext-configured server, got %q", h.Get(testAuthHeader))
	}
}

// TestHeaderScope_ExistingHeaderPreserved is spec scenario 3.6 / the design doc.
func TestHeaderScope_ExistingHeaderPreserved(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "http://homeassistant.local:8123", Headers: map[string]string{testAuthHeader: "Bearer configured"}}

	h := capturedHeaders(t, cfg, "http://homeassistant.local:8123/api/mcp", map[string]string{testAuthHeader: "Bearer original"})
	if h.Get(testAuthHeader) != "Bearer original" {
		t.Errorf("expected the pre-existing header to be preserved, got %q", h.Get(testAuthHeader))
	}
}

// TestHeaderScope_HandshakeRequestsCarryHeaders is spec scenario 3.7: the
// existing guarantee that handshake requests carry configured headers must
// not regress. connectToServer's handshake fails against this plain HTTP
// server (it is not a real MCP endpoint); only the header on the request the
// server received is asserted.
func TestHeaderScope_HandshakeRequestsCarryHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(testAuthHeader)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := &config.MCPServerConfig{URL: srv.URL, Headers: map[string]string{testAuthHeader: testBearerT}}

	_, _ = connectToServer(context.Background(), cfg, 2*time.Second, nil, nil)

	if gotAuth != testBearerT {
		t.Errorf("expected the handshake request to carry the configured header, got %q", gotAuth)
	}
}

// TestHeaderScope_InjectedHTTPClientIsUsed is spec scenario 3.8: a caller-
// supplied ClientConfig.HTTPClient / connectToServer httpClient argument is
// the instance the transport actually issues requests through.
func TestHeaderScope_InjectedHTTPClientIsUsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var hit bool
	injected := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hit = true

		return http.DefaultTransport.RoundTrip(req)
	})}

	cfg := &config.MCPServerConfig{URL: srv.URL}

	_, _ = connectToServer(context.Background(), cfg, 2*time.Second, injected, nil)

	if !hit {
		t.Error("expected connectToServer to issue requests through the injected HTTP client")
	}
}

// TestHeaderScope_UnparseableURLFailsClosed is spec scenario 3.9 / task 1.6:
// an unparseable configured URL must never fail open.
func TestHeaderScope_UnparseableURLFailsClosed(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "http://example.com/\tinvalid", Headers: map[string]string{testAuthHeader: testBearerT}}

	h := capturedHeaders(t, cfg, "http://example.com/mcp", nil)
	if h.Get(testAuthHeader) != "" {
		t.Errorf("expected no header when the configured URL fails to parse, got %q", h.Get(testAuthHeader))
	}
}

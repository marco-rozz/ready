package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestZenSessionID_StableAndUUIDShaped verifies the process-wide OpenCode
// session id: identical across calls (one "conversation" per daemon run) and
// shaped like a UUID v4 with the "ready-" prefix.
func TestZenSessionID_StableAndUUIDShaped(t *testing.T) {
	first := zenSessionID()
	second := zenSessionID()
	if first != second {
		t.Fatalf("session id changed across calls: %q vs %q", first, second)
	}

	if !strings.HasPrefix(first, "ready-") {
		t.Errorf("expected ready- prefix, got %q", first)
	}

	// ready-XXXXXXXX-XXXX-4XXX-VXXX-XXXXXXXXXXXX (32 hex chars + 4 dashes).
	body := strings.TrimPrefix(first, "ready-")
	if len(body) != 36 || strings.Count(body, "-") != 4 {
		t.Errorf("expected UUID-shaped session id, got %q", first)
	}
	if body[14] != '4' {
		t.Errorf("expected UUID v4 version nibble, got %q", first)
	}
}

// TestZenSessionTransport_InjectsHeadersAndPreserves verifies the
// RoundTripper stamps the session and User-Agent headers, does not overwrite
// headers already present on the request, and forwards the rest untouched.
func TestZenSessionTransport_InjectsHeadersAndPreserves(t *testing.T) {
	const existingSession = "preexisting-session"

	var gotSession, gotUA, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get(zenSessionHeader)
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/chat/completions", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set(zenSessionHeader, existingSession) // must be preserved

	transport := &zenSessionTransport{base: http.DefaultTransport}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}

	if gotSession != existingSession {
		t.Errorf("expected preserved session header %q, got %q", existingSession, gotSession)
	}
	if gotUA != zenUserAgent {
		t.Errorf("expected injected User-Agent %q, got %q", zenUserAgent, gotUA)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("expected Authorization untouched, got %q", gotAuth)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("expected path forwarded untouched, got %q", gotPath)
	}
}

// TestZenSessionTransport_InjectsWhenAbsent verifies the session header and
// User-Agent are stamped when the caller set neither.
func TestZenSessionTransport_InjectsWhenAbsent(t *testing.T) {
	var gotSession, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get(zenSessionHeader)
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	transport := &zenSessionTransport{base: http.DefaultTransport}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}
	defer resp.Body.Close()

	if gotSession == "" {
		t.Errorf("expected %s header injected", zenSessionHeader)
	}
	if gotSession != zenSessionID() {
		t.Errorf("expected process-wide session id %q, got %q", zenSessionID(), gotSession)
	}
	if gotUA != zenUserAgent {
		t.Errorf("expected User-Agent %q, got %q", zenUserAgent, gotUA)
	}
}

// TestZenSessionCustomHeaders_NonEmpty verifies the map handed to
// agenticopenai's CustomHeaders carries both headers.
func TestZenSessionCustomHeaders_NonEmpty(t *testing.T) {
	headers := zenSessionCustomHeaders()
	if headers[zenSessionHeader] == "" {
		t.Errorf("expected %s in custom headers", zenSessionHeader)
	}
	if headers["User-Agent"] != zenUserAgent {
		t.Errorf("expected User-Agent %q in custom headers, got %q", zenUserAgent, headers["User-Agent"])
	}
}

// TestZenSessionHTTPClient_NilTimeout verifies the client leaves the timeout
// unset so each eino-ext component keeps its own library default.
func TestZenSessionHTTPClient_NilTimeout(t *testing.T) {
	client := zenSessionHTTPClient()
	if client.Timeout != 0 {
		t.Errorf("expected unset timeout, got %v", client.Timeout)
	}
	if _, ok := client.Transport.(*zenSessionTransport); !ok {
		t.Errorf("expected *zenSessionTransport, got %T", client.Transport)
	}
}

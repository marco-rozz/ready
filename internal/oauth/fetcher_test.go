package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// freePort returns an ephemeral loopback port that is free at the moment of
// the call. There is an inherent (tiny) race between releasing it here and
// the caller binding it again, common to Go's own test suites.
func freePort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig

	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}

	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not a *net.TCPAddr", l.Addr())
	}

	if err := l.Close(); err != nil {
		t.Fatalf("failed to close probe listener: %v", err)
	}

	return tcpAddr.Port
}

func TestNonInteractiveFetcher(t *testing.T) {
	_, err := NonInteractiveFetcher(context.Background(), &auth.AuthorizationArgs{URL: "https://example.com/authorize"})
	if !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("error = %v, want ErrLoginRequired", err)
	}
}

func TestInteractiveFetcher_BusyPortFails(t *testing.T) {
	port := freePort(t)

	var lc net.ListenConfig

	blocker, err := lc.Listen(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("failed to occupy port: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	fetcher := newInteractiveFetcher(port, time.Second, noopOpener)
	if _, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: "https://example.com/authorize"}); err == nil {
		t.Fatal("expected an error when the redirect port is already bound")
	}
}

func TestInteractiveFetcher_Timeout(t *testing.T) {
	port := freePort(t)
	fetcher := newInteractiveFetcher(port, 50*time.Millisecond, noopOpener)

	// No callback ever arrives, so the fetcher must give up once its
	// timeout elapses rather than hang.
	start := time.Now()
	_, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: "http://127.0.0.1:1/nonexistent"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want it to mention timing out", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fetcher took %s, want it to fail quickly", elapsed)
	}
}

// noopOpener is a no-op stand-in for openBrowser, so tests never actually
// launch a browser.
func noopOpener(context.Context, string) error { return nil }

// placeholderAuthURL stands in for a real authorization URL in tests that
// never actually reach an authorization server (the callback is driven
// directly via httpGetCallback).
const placeholderAuthURL = "http://127.0.0.1:1/nonexistent"

// httpGetCallback issues a GET against the fetcher's /callback endpoint on
// port with the given raw query string, and closes the response body.
// waitForPort only proves the listening socket is bound at the kernel
// level, not that the http.Server goroutine has reached its accept loop, so
// this retries briefly instead of failing on the first transient EOF/reset.
func httpGetCallback(t *testing.T, port int, rawQuery string) {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d/callback?%s", port, rawQuery)

	deadline := time.Now().Add(2 * time.Second)
	for {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("failed to build callback request: %v", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("callback GET: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInteractiveFetcher_CallbackSuccess(t *testing.T) {
	port := freePort(t)
	fetcher := newInteractiveFetcher(port, 5*time.Second, noopOpener)

	resultCh := make(chan *auth.AuthorizationResult, 1)
	errCh := make(chan error, 1)

	go func() {
		res, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: placeholderAuthURL})
		if err != nil {
			errCh <- err

			return
		}
		resultCh <- res
	}()

	waitForPort(t, port)
	httpGetCallback(t, port, "code=abc123&state=xyz&iss=https://as.example.com")

	select {
	case res := <-resultCh:
		if res.Code != "abc123" || res.State != "xyz" || res.Iss != "https://as.example.com" {
			t.Fatalf("result = %+v, want code/state/iss populated from the callback", res)
		}
	case err := <-errCh:
		t.Fatalf("fetcher returned an error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fetcher to return")
	}
}

func TestInteractiveFetcher_CallbackErrorResponse(t *testing.T) {
	port := freePort(t)
	fetcher := newInteractiveFetcher(port, 5*time.Second, noopOpener)

	errCh := make(chan error, 1)

	go func() {
		_, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: placeholderAuthURL})
		errCh <- err
	}()

	waitForPort(t, port)
	httpGetCallback(t, port, "error=access_denied&error_description=user+declined")

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "access_denied") {
			t.Fatalf("error = %v, want it to mention access_denied", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fetcher to return")
	}
}

func TestInteractiveFetcher_CallbackMissingCode(t *testing.T) {
	port := freePort(t)
	fetcher := newInteractiveFetcher(port, 5*time.Second, noopOpener)

	errCh := make(chan error, 1)

	go func() {
		_, err := fetcher(context.Background(), &auth.AuthorizationArgs{URL: placeholderAuthURL})
		errCh <- err
	}()

	waitForPort(t, port)
	httpGetCallback(t, port, "state=xyz")

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "code") {
			t.Fatalf("error = %v, want it to mention the missing code parameter", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the fetcher to return")
	}
}

// waitForPort polls until port accepts connections, bounding the wait so a
// broken fetcher fails the test instead of hanging.
func waitForPort(t *testing.T, port int) {
	t.Helper()

	dialer := net.Dialer{Timeout: 50 * time.Millisecond}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(context.Background(), "tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = conn.Close()

			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("port %d never became reachable", port)
}

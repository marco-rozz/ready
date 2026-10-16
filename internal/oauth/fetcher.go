package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// DefaultAuthorizationTimeout bounds how long InteractiveFetcher waits for
// the authorization callback before giving up.
const DefaultAuthorizationTimeout = 5 * time.Minute

// shutdownGrace bounds how long the callback server is given to flush the
// in-flight /callback response (the "authorization complete" page) once a
// result has been received, before it is torn down. Without this, closing
// the server the instant the handler enqueues its result races the
// handler's own response write — the connection can be severed before the
// browser (or a client in tests) finishes reading the page.
const shutdownGrace = 2 * time.Second

// NonInteractiveFetcher is the auth.AuthorizationCodeFetcher injected by
// `ready serve`: it never opens a browser and fails immediately
// with ErrLoginRequired, so boot never blocks on a human. `ready login` is
// the only place a browser is ever opened — see InteractiveFetcher.
func NonInteractiveFetcher(_ context.Context, _ *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	return nil, ErrLoginRequired
}

// InteractiveFetcher returns an auth.AuthorizationCodeFetcher that binds the
// fixed loopback port first, opens the system browser at the authorization
// URL, serves the /callback redirect, and returns the resulting
// code/state/iss. redirectPort must match the port baked into
// the persisted client registration's redirect_uri: a busy port is a hard
// error here, never a silent fallback to another port. timeout bounds how
// long it waits for the callback; ctx cancellation is honoured throughout.
func InteractiveFetcher(redirectPort int, timeout time.Duration) auth.AuthorizationCodeFetcher {
	return newInteractiveFetcher(redirectPort, timeout, openBrowser)
}

// newInteractiveFetcher is InteractiveFetcher with the browser-opening step
// injected, so tests can exercise the callback server without actually
// launching a browser.
func newInteractiveFetcher(redirectPort int, timeout time.Duration, opener func(context.Context, string) error) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		var lc net.ListenConfig

		listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", redirectPort))
		if err != nil {
			return nil, fmt.Errorf("failed to bind the oauth callback port %d (is another `ready login` already running?): %w", redirectPort, err)
		}

		type callbackResult struct {
			res *auth.AuthorizationResult
			err error
		}
		resultCh := make(chan callbackResult, 1)

		mux := http.NewServeMux()
		mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
			res, cbErr := parseCallback(r)
			writeClosingPage(w, cbErr == nil)
			resultCh <- callbackResult{res: res, err: cbErr}
		})

		httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = httpSrv.Serve(listener) }()
		defer shutdownGracefully(httpSrv)

		if err := opener(ctx, args.URL); err != nil {
			return nil, fmt.Errorf("failed to open the system browser: %w", err)
		}

		deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		select {
		case cb := <-resultCh:
			if cb.err != nil {
				return nil, cb.err
			}

			return cb.res, nil
		case <-deadlineCtx.Done():
			return nil, fmt.Errorf("timed out waiting for the authorization callback after %s: %w", timeout, deadlineCtx.Err())
		}
	}
}

// shutdownGracefully stops srv from accepting new connections immediately
// but, unlike Close, waits (bounded by shutdownGrace) for the in-flight
// /callback response to finish flushing before returning.
func shutdownGracefully(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

// parseCallback extracts the authorization result (or error) from the
// /callback request's query parameters.
func parseCallback(r *http.Request) (*auth.AuthorizationResult, error) {
	q := r.URL.Query()

	if errParam := q.Get("error"); errParam != "" {
		desc := q.Get("error_description")

		return nil, fmt.Errorf("authorization server returned error %q: %s", errParam, desc) // reflects a live server response, not a static sentinel
	}

	code := q.Get("code")
	if code == "" {
		return nil, errors.New("authorization callback is missing the code parameter")
	}

	return &auth.AuthorizationResult{Code: code, State: q.Get("state"), Iss: q.Get("iss")}, nil
}

// writeClosingPage renders the page shown in the browser tab after the
// callback lands.
func writeClosingPage(w http.ResponseWriter, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	msg := "Authorization complete. You can close this window and return to the terminal."
	if !ok {
		msg = "Authorization failed. You can close this window and check the terminal for details."
	}

	_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html><body><p>%s</p></body></html>", msg)
}

// openBrowser opens url in the system's default browser. It only starts the
// process (cmd.Start, not Run) since the browser outlives this call; ctx
// governs only the lookup/spawn step.
func openBrowser(ctx context.Context, url string) error {
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}

	return cmd.Start()
}

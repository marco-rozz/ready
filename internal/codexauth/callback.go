package codexauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const callbackPath = "/auth/callback"

type AuthorizationResult struct {
	Code  string
	State string
}

// ParseManualCallback accepts a full redirect URL, query string, code#state,
// or a bare code. State is mandatory only when the supplied form contains it.
func ParseManualCallback(input, expectedState string) (AuthorizationResult, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return AuthorizationResult{}, errors.New("authorization callback is empty")
	}
	var code, state string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return AuthorizationResult{}, errors.New("authorization callback URL is malformed")
		}
		code, state = u.Query().Get("code"), u.Query().Get("state")
	} else if strings.HasPrefix(raw, "?") || strings.Contains(raw, "code=") {
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil {
			return AuthorizationResult{}, errors.New("authorization callback query is malformed")
		}
		code, state = q.Get("code"), q.Get("state")
	} else if before, after, ok := strings.Cut(raw, "#"); ok {
		code, state = before, after
	} else {
		code = raw
	}
	if code == "" {
		return AuthorizationResult{}, errors.New("authorization callback is missing the code")
	}
	if state != "" && state != expectedState {
		return AuthorizationResult{}, errors.New("authorization callback state mismatch")
	}

	return AuthorizationResult{Code: code, State: state}, nil
}

type callbackOutcome struct {
	value AuthorizationResult
	err   error
}

// LoopbackServer owns a bound ChatGPT OAuth loopback listener and HTTP server
// on RedirectURI so callers can obtain the bound server synchronously, return
// the authorization URL to an interactive client, and wait asynchronously for
// either the HTTP callback, a manual callback paste, or cancellation.
type LoopbackServer struct {
	listener net.Listener
	server   *http.Server
	resultCh chan callbackOutcome
}

// StartLoopbackServer binds the registered callback URI and starts serving
// callbackPath in the background.
func StartLoopbackServer(ctx context.Context, expectedState string) (*LoopbackServer, error) {
	u, err := url.Parse(RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("parse registered callback URI: %w", err)
	}

	return StartLoopbackServerOn(ctx, u.Host, expectedState)
}

// StartLoopbackServerOn binds hostPort and starts serving callbackPath in the
// background.
func StartLoopbackServerOn(ctx context.Context, hostPort, expectedState string) (*LoopbackServer, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("bind codex callback %s: %w", hostPort, err)
	}

	resultCh := make(chan callbackOutcome, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var res AuthorizationResult
		var cbErr error
		switch {
		case q.Get("error") != "":
			cbErr = fmt.Errorf("authorization server returned code=%s", safeCode(q.Get("error")))
		case q.Get("state") == "" || q.Get("state") != expectedState:
			cbErr = errors.New("authorization callback state mismatch")
		case q.Get("code") == "":
			cbErr = errors.New("authorization callback is missing the code")
		default:
			res = AuthorizationResult{Code: q.Get("code"), State: q.Get("state")}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cbErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, "<!doctype html><p>Authorization failed. Return to READY for details.</p>")
		} else {
			_, _ = fmt.Fprint(w, "<!doctype html><p>Authorization complete. You can close this window.</p>")
		}
		select {
		case resultCh <- callbackOutcome{value: res, err: cbErr}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()

	return &LoopbackServer{
		listener: listener,
		server:   server,
		resultCh: resultCh,
	}, nil
}

// Addr returns the listener's bound host:port address.
func (s *LoopbackServer) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}

	return s.listener.Addr().String()
}

// Close stops the HTTP callback server and releases the bound listener port.
func (s *LoopbackServer) Close() {
	if s == nil {
		return
	}
	_ = s.listener.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.server.Shutdown(shutdownCtx)
}

// Wait blocks until a callback lands on the server or ctx/timeout expires, and
// closes the server before returning.
func (s *LoopbackServer) Wait(ctx context.Context, timeout time.Duration) (AuthorizationResult, error) {
	defer s.Close()
	if timeout <= 0 {
		timeout = DefaultFlowTimeout
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case got := <-s.resultCh:
		return got.value, got.err
	case <-deadline.Done():
		return AuthorizationResult{}, fmt.Errorf("wait for codex callback: %w", deadline.Err())
	}
}

// WaitForLoopback binds the registered callback URI and validates exact path,
// state, code, cancellation, and timeout. It returns the authorization URL to
// opener only after binding succeeds.
func WaitForLoopback(ctx context.Context, authorizationURL, expectedState string, timeout time.Duration, opener func(context.Context, string) error) (AuthorizationResult, error) {
	srv, err := StartLoopbackServer(ctx, expectedState)
	if err != nil {
		return AuthorizationResult{}, err
	}
	defer srv.Close()

	if opener != nil {
		if err := opener(ctx, authorizationURL); err != nil {
			return AuthorizationResult{}, fmt.Errorf("open browser: %w", err)
		}
	}

	return srv.Wait(ctx, timeout)
}

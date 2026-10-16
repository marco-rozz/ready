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

// WaitForLoopback binds the registered callback URI and validates exact path,
// state, code, cancellation, and timeout. It returns the authorization URL to
// opener only after binding succeeds.
func WaitForLoopback(ctx context.Context, authorizationURL, expectedState string, timeout time.Duration, opener func(context.Context, string) error) (AuthorizationResult, error) {
	u, err := url.Parse(RedirectURI)
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("parse registered callback URI: %w", err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", u.Host)
	if err != nil {
		return AuthorizationResult{}, fmt.Errorf("bind codex callback %s: %w", u.Host, err)
	}
	defer listener.Close()

	type result struct {
		value AuthorizationResult
		err   error
	}
	resultCh := make(chan result, 1)
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
		case resultCh <- result{value: res, err: cbErr}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if opener != nil {
		if err := opener(ctx, authorizationURL); err != nil {
			return AuthorizationResult{}, fmt.Errorf("open browser: %w", err)
		}
	}
	if timeout <= 0 {
		timeout = DefaultFlowTimeout
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case got := <-resultCh:
		return got.value, got.err
	case <-deadline.Done():
		return AuthorizationResult{}, fmt.Errorf("wait for codex callback: %w", deadline.Err())
	}
}

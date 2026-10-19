package mcp

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/marco-rozz/ready/internal/oauth"
	"golang.org/x/oauth2"
)

// Sentinel errors for MCP failure categories.
//
// These wrap the categories below and let the agent
// surface user-friendly natural-language responses regardless of the underlying
// transport library. Errors are wrapped with %w so callers can use errors.Is.
var (
	// ErrMCPAuth indicates the MCP server rejected the credentials (401/403,
	// or an OAuth invalid_client/invalid_target) — fixing config, not
	// retrying, is what helps.
	ErrMCPAuth = errors.New("MCP authentication failed")
	// ErrMCPTimeout indicates the MCP server did not respond in time.
	ErrMCPTimeout = errors.New("MCP request timed out")
	// ErrMCPToolNotFound indicates the requested tool is not exposed by the server.
	ErrMCPToolNotFound = errors.New("MCP tool not found")
	// ErrMCPToolExecution indicates the tool executed but reported an error result.
	ErrMCPToolExecution = errors.New("MCP tool execution failed")
	// ErrMCPOAuthLoginRequired indicates no usable OAuth credential exists
	// for the server (the design doc in
	// the mcp-oauth-authorization): an operator must run
	// `ready login <server>`. Distinct from ErrMCPAuth because a login can
	// fix it, whereas ErrMCPAuth means the configured credentials
	// themselves are wrong.
	ErrMCPOAuthLoginRequired = errors.New("MCP oauth login required")
)

// classifyError inspects an arbitrary transport/protocol error and returns
// the matching sentinel error wrapped with the original cause. Unknown
// errors fall back to ErrMCPConnection.
//
// Structure precedes text: an error already classified is
// returned unchanged; then structured checks (oauth.ErrLoginRequired,
// *oauth2.RetrieveError, net.Error, context errors) run before the
// substring switch, which stays as the final fallback. Nothing that is not
// structured changes behaviour relative to the pre-D8 ordering.
func classifyError(err error) error {
	if err == nil {
		return nil
	}

	// ① Already classified — never double-wrap (e.g. ReconnectingClient.CallTool
	// classifying before BridgedTool sees the error).
	if isAlreadyClassified(err) {
		return err
	}

	// ② Structured checks.
	if errors.Is(err, oauth.ErrLoginRequired) {
		return wrap(ErrMCPOAuthLoginRequired, err)
	}

	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		switch retrieveErr.ErrorCode {
		case "invalid_grant":
			// Refresh token expired or repudiated: only a fresh login helps.
			return wrap(ErrMCPOAuthLoginRequired, err)
		case "invalid_client", "invalid_target":
			// Registration repudiated, or the resource indicator (RFC 8707)
			// was rejected: both are configuration problems, not transient.
			return wrap(ErrMCPAuth, err)
		}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return wrap(ErrMCPTimeout, err)
	}

	// ③ Context.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return wrap(ErrMCPTimeout, err)
	case errors.Is(err, context.Canceled):
		return err
	}

	// ④ Substrings — unchanged fallback.
	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "401"), strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "403"), strings.Contains(msg, "forbidden"):
		return wrap(ErrMCPAuth, err)
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return wrap(ErrMCPTimeout, err)
	case strings.Contains(msg, "tool not found"), strings.Contains(msg, "unknown tool"):
		return wrap(ErrMCPToolNotFound, err)
	}

	return wrap(ErrMCPConnection, err)
}

func wrap(sentinel, cause error) error {
	return &categorizedError{sentinel: sentinel, cause: cause}
}

type categorizedError struct {
	sentinel error
	cause    error
}

func (e *categorizedError) Error() string {
	return e.sentinel.Error() + ": " + e.cause.Error()
}

func (e *categorizedError) Unwrap() []error {
	return []error{e.sentinel, e.cause}
}

// isAlreadyClassified returns true if err already wraps one of the MCP
// sentinel errors (i.e., it was already run through classifyError). This
// prevents double-wrapping when BridgedTool receives an error from
// ReconnectingClient.CallTool which itself called classifyError.
func isAlreadyClassified(err error) bool {
	return errors.Is(err, ErrMCPConnection) ||
		errors.Is(err, ErrMCPAuth) ||
		errors.Is(err, ErrMCPTimeout) ||
		errors.Is(err, ErrMCPToolNotFound) ||
		errors.Is(err, ErrMCPToolExecution) ||
		errors.Is(err, ErrMCPOAuthLoginRequired)
}

// isPermanentAuthCondition reports whether err is one of the two sentinels
// the design doc treats as permanent authorization failures: ErrMCPAuth (static
// credentials, or an OAuth registration/resource rejection) and
// ErrMCPOAuthLoginRequired (no usable OAuth credential). Both short-circuit
// Reconnect (task 6.10) and never drop a live session (task 6.5).
func isPermanentAuthCondition(err error) bool {
	return errors.Is(err, ErrMCPAuth) || errors.Is(err, ErrMCPOAuthLoginRequired)
}

// shouldDropSession reports whether markDisconnected should tear down the
// session for err (the design doc, task 6.5). False for a permanent
// authorization condition: an authorization failure does not mean the
// transport is broken, so the token source must keep being consulted on
// every subsequent call — dropping the session would force
// "client is reconnecting" (ErrMCPConnection → Transient) and bury the one
// message that told the operator what to do.
func shouldDropSession(err error) bool {
	return !isPermanentAuthCondition(err)
}

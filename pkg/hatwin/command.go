package hatwin

import (
	"context"
	"fmt"
	"time"
)

// CommandResult is the correlated outcome of one HA call_service round-trip.
type CommandResult struct {
	// Success mirrors the HA result's success flag. A false value carries the
	// HA error object in Error.
	Success bool
	// Error is the raw HA error object ({code, message}) on failure.
	Error map[string]any
	// ContextID is the id of the context HA assigned to the call (from
	// result.context.id) — the attribution key for state_changed evidence.
	ContextID string
	// Result is the raw result payload, when present.
	Result map[string]any
}

// CallService sends one call_service command on the live session and waits
// for its correlated result (task 1.4, D1/D2).
//
// Write barrier (D2): the call fails immediately — never waiting for a
// reconnect — when no session handle is published (ErrNotConnected), when the
// current session's bootstrap has not completed (ErrSessionNotReady), or when
// the last inbound WS frame is older than Options.HardStaleThreshold
// (ErrStale). No wait on ctx cancellation: the caller's context bounds the
// wait for the result.
func (t *Twin) CallService(ctx context.Context, domain, service string, entityIDs []string, serviceData map[string]any) (CommandResult, error) {
	sess := t.session.Load()
	if sess == nil {
		return CommandResult{}, ErrNotConnected
	}
	if !sess.bootstrapComplete() {
		return CommandResult{}, ErrSessionNotReady
	}
	if hard := t.opts.HardStaleThreshold; hard > 0 {
		if last := t.LastWSActivity(); !last.IsZero() && time.Since(last) > hard {
			return CommandResult{}, fmt.Errorf("%w: last WS activity %ds ago", ErrStale, int(time.Since(last).Seconds()))
		}
	}

	msg := map[string]any{
		msgTypeKey: "call_service",
		"domain":   domain,
		"service":  service,
	}
	if len(entityIDs) > 0 {
		ids := make([]string, len(entityIDs))
		copy(ids, entityIDs)
		msg["target"] = map[string]any{keyEntityID: ids}
	}
	if len(serviceData) > 0 {
		msg["service_data"] = serviceData
	}

	id, ch, err := sess.call(msg)
	if err != nil {
		return CommandResult{}, fmt.Errorf("write call_service: %w", err)
	}

	res, err := sess.wait(ctx, id, ch)
	if err != nil {
		return CommandResult{}, err
	}

	return parseCommandResult(res), nil
}

// parseCommandResult projects a raw HA result message onto CommandResult.
func parseCommandResult(msg map[string]any) CommandResult {
	out := CommandResult{
		Success: msg["success"] == true,
		Result:  asMap(msg["result"]),
	}
	if errObj := asMap(msg["error"]); len(errObj) > 0 {
		out.Error = errObj
	}
	if ctxObj := asMap(msg["result"])["context"]; ctxObj != nil {
		out.ContextID = pickStr(asMap(ctxObj), "id")
	}

	return out
}

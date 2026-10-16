package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

// TestClassifyLLMErrorTable covers every row of the the design doc classification
// table (task 2.7), asserting both axes and the ladder in one place.
func TestClassifyLLMErrorTable(t *testing.T) {
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	tests := []struct {
		name         string
		err          error
		wantAdvance  bool
		wantDemote   bool
		wantRetry    bool
		wantLadder   Ladder
		wantProvider bool
	}{
		// ── hard ladder ──────────────────────────────────────────────
		{"401 status", errors.New("error 401: unauthorized"), true, true, false, LadderHard, false},
		{"unauthorized text", errors.New("request unauthorized: invalid api key"), true, true, false, LadderHard, false},
		{"403 status", errors.New("error 403"), true, true, false, LadderHard, false},
		{"forbidden text", errors.New("request forbidden"), true, true, false, LadderHard, false},
		{"404 model-not-found", errors.New("error 404: model gemma not found"), true, true, false, LadderHard, false},
		{"quota usage limit", errors.New("error 429: you exceeded your weekly usage limit"), true, true, false, LadderHard, true},
		{"quota exceeded", errors.New("error 429: quota exceeded for this key"), true, true, false, LadderHard, true},
		{"transport net.OpError", opErr, true, true, true, LadderHard, false},
		{"attempt budget expiry", fmt.Errorf("%w: %w", ErrAttemptBudgetExceeded, context.DeadlineExceeded), true, true, true, LadderHard, false},
		// ── soft ladder ──────────────────────────────────────────────
		{"429 rate limit", errors.New("error 429: too many requests"), true, true, true, LadderSoft, false},
		{"500 internal", errors.New("error 500: internal server error"), true, true, true, LadderSoft, false},
		{"502 bad gateway", errors.New("error 502: bad gateway"), true, true, true, LadderSoft, false},
		{"503 overload", errors.New("error 503: service overloaded"), true, true, true, LadderSoft, false},
		{"504 gateway timeout", errors.New("error 504: gateway timeout"), true, true, true, LadderSoft, false},
		{"stream EOF before content", io.ErrUnexpectedEOF, true, true, true, LadderSoft, false}, // ── soft ladder — server-side gateway errors without a numeric status ──
		{"[server_error] streaming timeout", errors.New("Streaming response failed: [server_error] upstream service timeout"), true, true, true, LadderSoft, false},
		{"wrapped eino NodeRunError", fmt.Errorf("agent execution failed: [NodeRunError] failed to receive stream chunk: error, Streaming response failed: %w", errors.New("[server_error] upstream service timeout")), true, true, true, LadderSoft, false},
		{"server_error text only", errors.New("error, server_error while generating"), true, true, true, LadderSoft, false},
		{"upstream timeout text only", errors.New("gateway returned: upstream service timeout"), true, true, true, LadderSoft, false},
		// ── soft ladder — silent-provider misbehaviour ───────────────
		{"agent produced no response", errors.New("agent produced no response"), true, true, true, LadderSoft, false},
		{"wrapped empty turn", fmt.Errorf("agent execution failed: %w", errors.New("agent produced no response")), true, true, true, LadderSoft, false},
		// ── neither axis ─────────────────────────────────────────────
		{"blank-but-unnamed error stays neither", errors.New("model returned an empty completion"), false, false, false, LadderNone, false},
		{"400 request shape", errors.New("error 400: invalid request: trailing system message"), false, false, false, LadderNone, false},
		{"caller cancellation", context.Canceled, false, false, false, LadderNone, false},
		{"unknown error", errors.New("something novel happened"), false, false, false, LadderNone, false},
		{"nil error", nil, false, false, false, LadderNone, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyLLMError(tc.err)

			if got.Advance != tc.wantAdvance {
				t.Errorf("Advance = %v, want %v", got.Advance, tc.wantAdvance)
			}
			if got.Demote != tc.wantDemote {
				t.Errorf("Demote = %v, want %v", got.Demote, tc.wantDemote)
			}
			if got.RetrySame != tc.wantRetry {
				t.Errorf("RetrySame = %v, want %v", got.RetrySame, tc.wantRetry)
			}
			if got.Ladder != tc.wantLadder {
				t.Errorf("Ladder = %v, want %v", got.Ladder, tc.wantLadder)
			}
			if got.ProviderScoped != tc.wantProvider {
				t.Errorf("ProviderScoped = %v, want %v", got.ProviderScoped, tc.wantProvider)
			}
		})
	}
}

// TestClassifyLLMError_QuotaPrecedenceOver429 pins the load-bearing
// precedence of quota-exhaustion detection over the generic 429 rule (task
// 2.5): an exhausted account/period budget reported with HTTP 429 must land
// on the hard ladder with provider scope, not the soft one — otherwise a
// free-tier daily cap would be retried every 30 seconds for twelve hours.
func TestClassifyLLMError_QuotaPrecedenceOver429(t *testing.T) {
	quota429 := errors.New("error, status code: 429, message: You have exceeded your weekly usage limit")

	cls := ClassifyLLMError(quota429)
	if !cls.Advance || !cls.Demote {
		t.Fatalf("quota 429 must advance and demote, got %+v", cls)
	}
	if cls.Ladder != LadderHard {
		t.Errorf("quota 429 ladder = %v, want hard", cls.Ladder)
	}
	if !cls.ProviderScoped {
		t.Error("quota 429 must be provider-scoped (every candidate on the instance is demoted)")
	}

	plain429 := errors.New("error, status code: 429, message: too many requests, retry later")
	plain := ClassifyLLMError(plain429)
	if plain.Ladder != LadderSoft {
		t.Errorf("plain rate-limit 429 ladder = %v, want soft", plain.Ladder)
	}
	if plain.ProviderScoped {
		t.Error("plain rate-limit 429 must not be provider-scoped")
	}
}

func TestClassifyLLMError_CodexUsageLimitCodes(t *testing.T) {
	for _, message := range []string{
		"HTTP 429 code=usage_limit_exceeded",
		"workspace_owner_credits_depleted",
		"workspace member credits depleted",
	} {
		t.Run(message, func(t *testing.T) {
			cls := ClassifyLLMError(errors.New(message))
			if !cls.Advance || !cls.Demote || cls.RetrySame {
				t.Fatalf("Codex usage exhaustion = %+v, want advance+demote without retry", cls)
			}
			if cls.Ladder != LadderHard || !cls.ProviderScoped {
				t.Fatalf("Codex usage exhaustion = %+v, want hard provider-scoped demotion", cls)
			}
		})
	}
}

// TestClassifyLLMError_ServerErrorCodeInside400StaysNeither pins the guard:
// "server_error" can appear inside a request-shape 400 (field names,
// identifiers) — the design doc keeps a 400 on neither axis, so the new soft
// marker must not rescue it into a retryable classification.
func TestClassifyLLMError_ServerErrorCodeInside400StaysNeither(t *testing.T) {
	for _, message := range []string{
		"error 400: invalid request: unknown field server_error_mode",
		"error 400: bad request: server_error enum value not allowed",
	} {
		t.Run(message, func(t *testing.T) {
			cls := ClassifyLLMError(errors.New(message))
			if cls.Advance || cls.Demote || cls.RetrySame {
				t.Errorf("400 carrying a server_error identifier = %+v, want neither axis", cls)
			}
		})
	}
}

// TestClassifyLLMError_PermanentClassBeatsServerErrorMarker pins precedence:
// the permanent rows (401/403/404/quota) are matched BEFORE the new
// [server_error] marker, so an error carrying both is classified by its
// permanent class — an auth failure must never be rescued into a retryable
// soft demotion because the gateway tagged it server_error too.
func TestClassifyLLMError_PermanentClassBeatsServerErrorMarker(t *testing.T) {
	cls := ClassifyLLMError(errors.New("[server_error] 401: unauthorized"))
	if cls.Ladder != LadderHard || cls.RetrySame {
		t.Fatalf("[server_error] 401 = %+v, want hard ladder without same-candidate retry", cls)
	}
	quota := ClassifyLLMError(errors.New("[server_error] 429: usage_limit_exceeded"))
	if quota.Ladder != LadderHard || !quota.ProviderScoped {
		t.Fatalf("[server_error] quota = %+v, want hard provider-scoped demotion", quota)
	}
}

// TestClassifyLLMError_404WithoutNotFoundIsNeither pins the guard that an
// unrelated "404" substring alone does not classify as model-not-found.
func TestClassifyLLMError_404WithoutNotFoundIsNeither(t *testing.T) {
	cls := ClassifyLLMError(errors.New("request took 404 milliseconds"))
	if cls.Advance || cls.Demote {
		t.Errorf("bare 404 substring must classify as neither, got %+v", cls)
	}
}

// TestClassifyLLMError_WrappedChain verifies classification inspects the
// full error chain, not just the outermost message (eino components wrap
// provider errors).
func TestClassifyLLMError_WrappedChain(t *testing.T) {
	wrapped := fmt.Errorf("node run failed: %w", fmt.Errorf("chat completion failed: %w",
		&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}))

	cls := ClassifyLLMError(wrapped)
	if !cls.Advance || cls.Ladder != LadderHard {
		t.Errorf("wrapped transport error = %+v, want advance+hard", cls)
	}
}

// TestIsRequestShapeError verifies the diagnostic-only 400 marker used for
// the request-shape WARN (task 2.6); it never changes either axis.
func TestIsRequestShapeError(t *testing.T) {
	if !IsRequestShapeError(errors.New("error 400: bad request")) {
		t.Error("expected 400 marker to be detected")
	}
	if IsRequestShapeError(errors.New("error 401: unauthorized")) {
		t.Error("expected non-400 error to not match")
	}
	if IsRequestShapeError(nil) {
		t.Error("expected nil error to not match")
	}
}

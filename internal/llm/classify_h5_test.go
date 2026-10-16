package llm

// Regression tests for criticalities-review-2026-09-24 H5: deadline and
// timeout conditions surfacing from transport/context levels are advanceable
// so the orchestrator can attempt the next candidate.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
)

func TestClassifyLLMError_DeadlineExceededIsAdvanceable(t *testing.T) {
	cls := ClassifyLLMError(deadlineTestErr())
	if !cls.Advance {
		t.Fatalf("DeadlineExceeded not advanceable: %+v", cls)
	}
}

func TestClassifyLLMError_NetErrorTimeoutIsAdvanceable(t *testing.T) {
	// Shape an http response-header timeout produces: *net.OpError wrapping
	// a timeout error, wrapped in *url.Error.
	headerTimeoutErr := &url.Error{
		Op:  "Post",
		URL: "https://provider.example/v1/chat",
		Err: &net.OpError{Op: "read", Err: timeoutErr()},
	}

	cls := ClassifyLLMError(headerTimeoutErr)
	if !cls.Advance {
		t.Fatalf("transport timeout not advanceable: %+v", cls)
	}

	if cls.Demote && cls.Ladder != LadderHard {
		t.Fatalf("transport timeout demotion = %+v, want hard ladder", cls)
	}
}

// TestClassifyLLMError_CanceledStillNeither pins the boundary added with the
// timeout classification: caller cancellation stays on neither axis.
func TestClassifyLLMError_CanceledStaysNeither(t *testing.T) {
	cls := ClassifyLLMError(canceledTestErr())
	if cls.Advance || cls.Demote {
		t.Fatalf("context.Canceled must be Neither, got %+v", cls)
	}
}

// helpers — indirections keep the test body readable.
func deadlineTestErr() error {
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()

	<-ctx.Done()

	return ctx.Err()
}

func canceledTestErr() error {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx.Err()
}

func timeoutErr() error {
	return &timeoutError{}
}

type timeoutError struct{}

func (t *timeoutError) Error() string { return "i/o timeout" }
func (t *timeoutError) Timeout() bool { return true }
func (t *timeoutError) Temporary() bool {
	return true
}

// TestClassifyLLMError_NoBareStatusFalsePositive pins the LOW
// classification finding (criticalities-remediation): a status-code-looking
// digit run inside a longer number (e.g. "retry after 1500ms") must not
// classify as a 5xx and demote a healthy candidate.
func TestClassifyLLMError_NoBareStatusFalsePositive(t *testing.T) {
	cls := ClassifyLLMError(errors.New("rate limited: retry after 1500ms"))
	if cls.Advance || cls.Demote {
		t.Fatalf("retry-after with 1500ms must not classify as 5xx (got %+v)", cls)
	}

	// The real status still classifies.
	cls2 := ClassifyLLMError(errors.New("http 503 service unavailable"))
	if !cls2.Advance || cls2.Ladder != LadderSoft {
		t.Fatalf("503 not soft-advanceable (got %+v)", cls2)
	}
}

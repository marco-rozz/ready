package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests provoke each classifyError sentinel through the official SDK
// (P6): a pattern that only matches a fabricated string can silently stop
// matching a real SDK error message without failing a test or breaking the
// build. Task 7.1 requires each of classifyError's categories to be pinned
// this way.

// TestClassifyError_ToolNotFound_ThroughSDK calls a tool the mock server does
// not expose over a real in-process session, so the "unknown tool" message
// classified comes from mcp.Server's own tool-lookup error, not a fabricated
// string.
func TestClassifyError_ToolNotFound_ThroughSDK(t *testing.T) {
	mock := NewMockServer(nil)

	client, err := mock.NewClient(context.Background())
	if err != nil {
		t.Fatalf("failed to create in-process client: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, callErr := client.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "does/not/exist"})
	if callErr == nil {
		t.Fatal("expected an error calling an unknown tool")
	}

	classified := classifyError(callErr)
	if !errors.Is(classified, ErrMCPToolNotFound) {
		t.Fatalf("expected ErrMCPToolNotFound, got %v", classified)
	}
}

// TestClassifyError_ConnectionRefused_ThroughSDK connects to a port nothing
// listens on, so the SDK transport surfaces a genuine connection failure.
func TestClassifyError_ConnectionRefused_ThroughSDK(t *testing.T) {
	cfg := &config.MCPServerConfig{URL: "http://127.0.0.1:1"}

	_, err := connectToServer(context.Background(), cfg, 2*time.Second, nil, nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}

	if !errors.Is(err, ErrMCPConnection) {
		t.Fatalf("expected ErrMCPConnection, got %v", err)
	}
}

// TestClassifyError_Timeout_ThroughSDK connects to a server that never
// responds, bounded by a short client timeout, so the deadline exceeded error
// is the SDK/http stack's own, not context.DeadlineExceeded fabricated by the
// test.
func TestClassifyError_Timeout_ThroughSDK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()

	cfg := &config.MCPServerConfig{URL: srv.URL}

	_, err := connectToServer(context.Background(), cfg, 20*time.Millisecond, nil, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}

	if !errors.Is(err, ErrMCPTimeout) {
		t.Fatalf("expected ErrMCPTimeout, got %v", err)
	}
}

// TestClassifyError_Unauthorized_ThroughSDK connects to a server that always
// answers 401, so the message classified is the SDK's own rendering of the
// HTTP status, not a fabricated "401" string.
func TestClassifyError_Unauthorized_ThroughSDK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &config.MCPServerConfig{URL: srv.URL}

	_, err := connectToServer(context.Background(), cfg, 2*time.Second, nil, nil)
	if err == nil {
		t.Fatal("expected an auth error")
	}

	if !errors.Is(err, ErrMCPAuth) {
		t.Fatalf("expected ErrMCPAuth, got %v", err)
	}
}

// erroringInvoker is a toolInvoker stub that always returns a pre-classified
// error, used to verify BridgedTool does not run it through classifyError a
// second time.
type erroringInvoker struct {
	err error
}

func (e *erroringInvoker) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, e.err
}

// TestClassifyError_NoDoubleClassification verifies that an error already
// classified upstream (e.g. by ReconnectingClient.CallTool) is propagated by
// BridgedTool.InvokableRun as-is, without a second classifyError pass that
// would duplicate the sentinel text.
func TestClassifyError_NoDoubleClassification(t *testing.T) {
	preclassified := classifyError(errors.New("401 unauthorized"))
	invoker := &erroringInvoker{err: preclassified}

	bridged := NewBridgedTool(discardLogger(), invoker, newMCPToolStub(), "srv", "srv__"+toolLightTurnOn)

	_, err := bridged.InvokableRun(context.Background(), `{}`)
	if !errors.Is(err, ErrMCPAuth) {
		t.Fatalf("expected ErrMCPAuth, got %v", err)
	}

	if n := strings.Count(err.Error(), ErrMCPAuth.Error()); n != 1 {
		t.Errorf("expected sentinel text exactly once, got %d occurrences in %q", n, err.Error())
	}
}

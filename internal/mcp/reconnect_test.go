package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestReconnectingClient fills in the immutable httpClient/client fields
// (P2) that NewReconnectingClient would normally construct, for tests that
// build a ReconnectingClient by struct literal to exercise Reconnect/CallTool
// without a real NewReconnectingClient call.
func newTestReconnectingClient(rc *ReconnectingClient) *ReconnectingClient {
	rc.httpClient = NewScopedHTTPClient(rc.serverCfg)
	rc.client = mcp.NewClient(&mcp.Implementation{Name: homeAgent, Version: implementationVersion}, nil)

	return rc
}

func TestComputeBackoff_FirstAttemptIsInitial(t *testing.T) {
	p := ReconnectConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		BackoffFactor:  2.0,
	}

	if got := computeBackoff(p, 1); got != 100*time.Millisecond {
		t.Errorf("attempt 1: expected 100ms, got %v", got)
	}
}

func TestComputeBackoff_GrowsExponentially(t *testing.T) {
	p := ReconnectConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
		BackoffFactor:  2.0,
	}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
	}

	for _, c := range cases {
		got := computeBackoff(p, c.attempt)
		if got != c.want {
			t.Errorf("attempt %d: expected %v, got %v", c.attempt, c.want, got)
		}
	}
}

func TestComputeBackoff_ClampsToMaxBackoff(t *testing.T) {
	p := ReconnectConfig{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     500 * time.Millisecond,
		BackoffFactor:  2.0,
	}

	// Attempt 10 would be ~51.2s without the cap.
	got := computeBackoff(p, 10)
	if got != 500*time.Millisecond {
		t.Errorf("expected clamp to 500ms, got %v", got)
	}
}

func TestReconnectConfigFromConfig_AppliesDefaults(t *testing.T) {
	out := ReconnectConfigFromConfig(config.MCPReconnectConfig{})

	if out.InitialBackoff != time.Second {
		t.Errorf("expected default InitialBackoff 1s, got %v", out.InitialBackoff)
	}
	if out.MaxBackoff != 60*time.Second {
		t.Errorf("expected default MaxBackoff 60s, got %v", out.MaxBackoff)
	}
	if out.BackoffFactor != 2.0 {
		t.Errorf("expected default BackoffFactor 2.0, got %v", out.BackoffFactor)
	}
}

func TestReconnectConfigFromConfig_RespectsUserValues(t *testing.T) {
	out := ReconnectConfigFromConfig(config.MCPReconnectConfig{
		MaxRetries:     5,
		InitialBackoff: 250 * time.Millisecond,
		MaxBackoff:     10 * time.Second,
		BackoffFactor:  1.5,
	})

	if out.MaxRetries != 5 {
		t.Errorf("MaxRetries: got %d", out.MaxRetries)
	}
	if out.InitialBackoff != 250*time.Millisecond {
		t.Errorf("InitialBackoff: got %v", out.InitialBackoff)
	}
	if out.MaxBackoff != 10*time.Second {
		t.Errorf("MaxBackoff: got %v", out.MaxBackoff)
	}
	if out.BackoffFactor != 1.5 {
		t.Errorf("BackoffFactor: got %v", out.BackoffFactor)
	}
}

func TestReconnect_ContextCancelAborts(t *testing.T) {
	rc := newTestReconnectingClient(&ReconnectingClient{
		serverCfg: &config.MCPServerConfig{
			URL: "http://127.0.0.1:1/does-not-exist",
		},
		clientCfg: ClientConfig{Timeout: 50 * time.Millisecond},
		reconnectCf: ReconnectConfig{
			InitialBackoff: 10 * time.Millisecond,
			MaxBackoff:     20 * time.Millisecond,
			BackoffFactor:  2.0,
			MaxRetries:     0,
		},
		logger: discardLogger(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := rc.Reconnect(ctx)
	if err == nil {
		t.Fatal("expected error on context cancel")
	}

	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("expected context error, got %v", err)
	}
}

func TestReconnect_MaxRetriesExhausted(t *testing.T) {
	rc := newTestReconnectingClient(&ReconnectingClient{
		serverCfg: &config.MCPServerConfig{
			URL: "http://127.0.0.1:1/does-not-exist",
		},
		clientCfg: ClientConfig{Timeout: 50 * time.Millisecond},
		reconnectCf: ReconnectConfig{
			InitialBackoff: 1 * time.Millisecond,
			MaxBackoff:     5 * time.Millisecond,
			BackoffFactor:  2.0,
			MaxRetries:     2,
		},
		logger: discardLogger(),
	})

	err := rc.Reconnect(context.Background())
	if err == nil {
		t.Fatal("expected exhaustion error")
	}

	if msg := err.Error(); msg == "" {
		t.Error("error should not be empty")
	}
}

func TestMarkDisconnected_FlipsState(t *testing.T) {
	rc := &ReconnectingClient{
		logger: discardLogger(),
	}
	rc.connected.Store(true)

	rc.markDisconnected()

	if rc.Connected() {
		t.Error("expected Connected=false after markDisconnected")
	}
}

func TestCallTool_ReturnsErrWhenDisconnected(t *testing.T) {
	rc := &ReconnectingClient{
		logger: discardLogger(),
	}
	// mcpClient is nil → CallTool must fail fast.
	_, err := rc.CallTool(context.Background(), mcpCallStub())

	if err == nil {
		t.Fatal("expected error when disconnected")
	}
	if !errors.Is(err, ErrMCPConnection) {
		t.Errorf("expected ErrMCPConnection, got %v", err)
	}
}

func TestClassifyError_Categories(t *testing.T) {
	cases := []struct {
		name     string
		in       error
		sentinel error
	}{
		{"deadline", context.DeadlineExceeded, ErrMCPTimeout},
		{"401", errors.New("server returned 401 Unauthorized"), ErrMCPAuth},
		{"403", errors.New("403 forbidden"), ErrMCPAuth},
		{"timeout-string", errors.New("connection timeout reached"), ErrMCPTimeout},
		{"tool-not-found", errors.New("tool not found: foo"), ErrMCPToolNotFound},
		{"unknown", errors.New("connection refused"), ErrMCPConnection},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyError(c.in)
			if !errors.Is(got, c.sentinel) {
				t.Errorf("expected to wrap %v, got %v", c.sentinel, got)
			}
		})
	}
}

func TestClassifyError_PreservesCause(t *testing.T) {
	cause := errors.New("inner cause")
	wrapped := classifyError(fmt.Errorf("call failed: %w 401", cause))

	if !errors.Is(wrapped, cause) {
		t.Error("expected wrapped error to preserve cause via errors.Is")
	}
}

func TestClassifyError_Nil(t *testing.T) {
	if got := classifyError(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

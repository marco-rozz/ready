package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestLivenessSignalsExactlyOnceForIntermediateStream(t *testing.T) {
	var calls atomic.Int32
	ctx := WithLiveness(context.Background(), func() { calls.Add(1) })

	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "thinking"}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1"}}}, nil)
	}()

	_, hasToolCalls := readStreamingAssistant(sr, nil, nil, func() { SignalLiveness(ctx) })
	if !hasToolCalls {
		t.Fatal("expected an intermediate tool-call stream")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("liveness callback calls = %d, want 1", got)
	}
}

func TestLivenessNotSignalledOnStreamError(t *testing.T) {
	var calls atomic.Int32
	ctx := WithLiveness(context.Background(), func() { calls.Add(1) })

	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		sw.Send(nil, errors.New("stream failed before a chunk"))
	}()

	var lastErr error
	_, _ = readStreamingAssistant(sr, nil, &lastErr, func() { SignalLiveness(ctx) })
	if lastErr == nil {
		t.Fatal("expected stream error")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("liveness callback calls = %d, want 0", got)
	}
}

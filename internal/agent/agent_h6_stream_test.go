package agent

// Regression tests for criticalities-review-2026-09-24 H6: content chunks of
// an intermediate ReAct iteration arriving BEFORE the chunk that carries the
// tool calls were forwarded to onChunk (visible to the user, and turning
// ContentDelivered true). The per-iteration buffering fix keeps them off
// every channel.

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// chunk text constants (goconst).
const (
	chunkA = "The "
	chunkB = "light "
	chunkC = "is on."
)

// TestReadStreamingAssistant_PreActionTextBeforeToolCallsNeverForwarded pins
// H6 for the chat path: content chunks that arrive before the tool-call chunk
// within the same iteration are buffered and dropped — onChunk is never
// invoked and the result is empty.
func TestReadStreamingAssistant_PreActionTextBeforeToolCallsNeverForwarded(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		// Pre-action reasoning text arrives first...
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "I should turn off the light."}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: " Let me call the tool."}, nil)
		// ...then the tool-call chunk marks the iteration as intermediate.
		sw.Send(&schema.Message{
			Role:    schema.Assistant,
			Content: "",
			ToolCalls: []schema.ToolCall{
				{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "light_off", Arguments: "{}"}},
			},
		}, nil)
	}()

	var delivered []string
	result, _ := readStreamingAssistant(sr, func(chunk string) {
		delivered = append(delivered, chunk)
	}, nil, nil)

	if result != "" {
		t.Errorf("expected empty result for intermediate iteration, got %q", result)
	}

	if len(delivered) != 0 {
		t.Errorf("pre-action text leaked to onChunk: %v", delivered)
	}
}

// TestReadStreamingAssistant_FinalAnswerBufferedHeadDeliveredInOrder pins the
// final-answer side of H6: when the iteration ends without tool calls, the
// buffered content is forwarded in original chunk order.
func TestReadStreamingAssistant_FinalAnswerBufferedHeadDelivered(t *testing.T) {
	sr, sw := schema.Pipe[*schema.Message](0)
	go func() {
		defer sw.Close()
		sw.Send(&schema.Message{Role: schema.Assistant, Content: chunkA}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: chunkB}, nil)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: chunkC}, nil)
	}()

	var delivered []string
	result, _ := readStreamingAssistant(sr, func(chunk string) {
		delivered = append(delivered, chunk)
	}, nil, nil)

	if result != "The light is on." {
		t.Errorf("result = %q", result)
	}

	if len(delivered) != 3 || delivered[0] != chunkA || delivered[1] != chunkB || delivered[2] != chunkC {
		t.Errorf("delivered = %v, want the original chunk order", delivered)
	}
}

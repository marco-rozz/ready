package mcp

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// syncBuffer is a mutex-guarded bytes.Buffer, for tests that read a
// zerolog.Logger's output from a different goroutine than the one writing
// it (e.g. polling for a log line while a HealthMonitor ticks in the
// background).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// discardLogger returns a zerolog.Logger that drops all output. Tests that
// exercise failure paths use it to keep test output clean.
func discardLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

// mcpCallStub returns a minimal valid CallToolParams for assertions that do
// not depend on tool semantics.
func mcpCallStub() *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: "stub", Arguments: map[string]any{}}
}

// newMCPToolStub returns a minimal *mcp.Tool for unit tests that don't need a
// live MCP server.
func newMCPToolStub() *mcp.Tool {
	return &mcp.Tool{
		Name:        testToolLightOn,
		Description: "Turn on a light",
		InputSchema: schemaObject(map[string]any{
			attrEntityID: map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "Entity ID"},
		}, attrEntityID),
	}
}

// callCapture is a toolInvoker that records every CallTool request for assertions.
type callCapture struct {
	requests []*mcp.CallToolParams
}

func (c *callCapture) CallTool(_ context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	c.requests = append(c.requests, params)

	return textResult(`{"success":true}`), nil
}

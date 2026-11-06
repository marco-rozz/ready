package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

// TestBridgedTool_Info_NoParamsSerialisesAsObject guards the wire shape of a
// zero-argument MCP tool. OpenAI-compatible gateways (and Anthropic behind
// them) require function.parameters to be a JSON object; a nil ParamsOneOf
// marshals to `"parameters": null` and the whole request is rejected with
// HTTP 400 ("tools[N].function.parameters: expected an object").
func TestBridgedTool_Info_NoParamsSerialisesAsObject(t *testing.T) {
	cases := map[string]any{
		"empty properties object": map[string]any{"type": "object", "properties": map[string]any{}},
		"nil input schema":        nil,
	}

	for name, inputSchema := range cases {
		t.Run(name, func(t *testing.T) {
			tool := NewBridgedTool(
				zerolog.Nop(),
				nil,
				&mcp.Tool{Name: "user_check", Description: "no args", InputSchema: inputSchema},
				"backoffice", "user_check",
			)

			info, err := tool.Info(context.Background())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}

			js, err := info.ToJSONSchema()
			if err != nil {
				t.Fatalf("ToJSONSchema: %v", err)
			}

			raw, err := json.Marshal(js)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil || got == nil {
				t.Fatalf("parameters must be a JSON object, got %s", raw)
			}

			if got["type"] != "object" {
				t.Fatalf(`parameters.type = %v, want "object" (raw: %s)`, got["type"], raw)
			}
		})
	}
}

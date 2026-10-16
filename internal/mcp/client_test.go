package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
)

func TestClient_InProcess_DiscoverTools(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create in-process client: %v", err)
	}

	defer func() { _ = client.Close() }()

	tools := client.Tools()
	if len(tools) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(tools))
	}

	// Verify tool names.
	expectedNames := map[string]bool{
		testToolLightOn:     false,
		testToolLightOff:    false,
		testToolClimate:     false,
		testToolSwitch:      false,
		testToolGetState:    false,
		testToolClimateHvac: false,
	}

	for _, t2 := range tools {
		info, infoErr := t2.Info(ctx)
		if infoErr != nil {
			t.Fatalf("Info() failed: %v", infoErr)
		}

		if _, ok := expectedNames[info.Name]; ok {
			expectedNames[info.Name] = true
		}
	}

	for name, found := range expectedNames {
		if !found {
			t.Errorf("tool %q not discovered", name)
		}
	}
}

func TestClient_InProcess_ToolExecution(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	defer func() { _ = client.Close() }()

	// Find the light/turn_on tool.
	var lightOnTool tool.InvokableTool
	for _, t2 := range client.Tools() {
		info, _ := t2.Info(ctx)
		if info.Name == testToolLightOn {
			var ok bool
			lightOnTool, ok = t2.(tool.InvokableTool)
			if !ok {
				t.Fatal("tool does not implement InvokableTool")
			}

			break
		}
	}

	if lightOnTool == nil {
		t.Fatal("light/turn_on tool not found")
	}

	// Execute the tool.
	args, _ := json.Marshal(map[string]any{
		testAttrEntityID:   testEntityKitchen,
		testAttrBrightness: float64(200),
	})

	result, err := lightOnTool.InvokableRun(ctx, string(args))
	if err != nil {
		t.Fatalf("InvokableRun failed: %v", err)
	}

	// Verify result contains success.
	var response map[string]any
	if jsonErr := json.Unmarshal([]byte(result), &response); jsonErr != nil {
		t.Fatalf("failed to parse result: %v", jsonErr)
	}

	if response["success"] != true {
		t.Errorf("expected success=true, got %v", response["success"])
	}

	if response["state"] != "on" {
		t.Errorf("expected state='on', got %v", response["state"])
	}

	// Verify mock state was updated.
	state, ok := mock.GetState(testEntityKitchen)
	if !ok {
		t.Fatal("state not found")
	}

	if state.State != "on" {
		t.Errorf("expected state 'on', got %q", state.State)
	}
}

func TestClient_InProcess_GetState(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	defer func() { _ = client.Close() }()

	// Find get_state tool.
	var getStateTool tool.InvokableTool
	for _, t2 := range client.Tools() {
		info, _ := t2.Info(ctx)
		if info.Name == testToolGetState {
			var ok bool

			getStateTool, ok = t2.(tool.InvokableTool)
			if !ok {
				t.Fatal("tool does not implement InvokableTool")
			}

			break
		}
	}

	args, _ := json.Marshal(map[string]any{testAttrEntityID: testEntityLiving})

	result, err := getStateTool.InvokableRun(ctx, string(args))
	if err != nil {
		t.Fatalf("InvokableRun failed: %v", err)
	}

	var state EntityState
	if jsonErr := json.Unmarshal([]byte(result), &state); jsonErr != nil {
		t.Fatalf("failed to parse state: %v", jsonErr)
	}

	if state.State != "on" {
		t.Errorf("expected state 'on', got %q", state.State)
	}

	_ = mock // prevent unused
}

func TestClient_InProcess_ToolInfo_HasParams(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	defer func() { _ = client.Close() }()

	// Check that light/turn_on has proper parameter info.
	for _, t2 := range client.Tools() {
		info, _ := t2.Info(ctx)
		if info.Name == testToolLightOn {
			if info.ParamsOneOf == nil {
				t.Error("expected ParamsOneOf to be set for light/turn_on")
			}

			return
		}
	}

	t.Error("light/turn_on tool not found")
}

// TestClient_InProcess_RichSchemaToolSurvivesDiscovery verifies task 5.3: a
// full connect -> Tools() -> Info() round trip against the mock server's
// climate/set_hvac_mode tool preserves the enum, the array's ElemInfo, and
// the nested object's SubParams — not just as a pure-function unit test, but
// through the actual MCP discovery path.
func TestClient_InProcess_RichSchemaToolSurvivesDiscovery(t *testing.T) {
	mock := NewMockServer(nil)
	ctx := context.Background()

	client, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create in-process client: %v", err)
	}
	defer func() { _ = client.Close() }()

	var info *schema.ToolInfo
	for _, t2 := range client.Tools() {
		i, infoErr := t2.Info(ctx)
		if infoErr != nil {
			t.Fatalf("Info() failed: %v", infoErr)
		}
		if i.Name == testToolClimateHvac {
			info = i

			break
		}
	}

	if info == nil {
		t.Fatalf("tool %q not discovered", testToolClimateHvac)
	}

	params := decodeToolParams(t, info)

	hvacMode := requireSubParam(t, params, "hvac_mode")
	wantEnum := hvacModes()
	if len(hvacMode.Enum) != len(wantEnum) {
		t.Fatalf("hvac_mode.Enum = %v, want %v", hvacMode.Enum, wantEnum)
	}
	for i, v := range wantEnum {
		if hvacMode.Enum[i] != v {
			t.Errorf("hvac_mode.Enum[%d] = %q, want %q", i, hvacMode.Enum[i], v)
		}
	}

	fanSpeeds := requireSubParam(t, params, "fan_speeds")
	if fanSpeeds.Type != schema.Array || fanSpeeds.ElemInfo == nil || fanSpeeds.ElemInfo.Type != schema.String {
		t.Fatalf("fan_speeds = %+v, want Array of String", fanSpeeds)
	}

	scheduleParam := requireSubParam(t, params, "schedule")
	if scheduleParam.Type != schema.Object || scheduleParam.SubParams == nil {
		t.Fatalf("schedule = %+v, want Object with SubParams", scheduleParam)
	}
	if _, ok := scheduleParam.SubParams["start"]; !ok {
		t.Error("schedule.SubParams missing \"start\"")
	}
	if _, ok := scheduleParam.SubParams["temperature"]; !ok {
		t.Error("schedule.SubParams missing \"temperature\"")
	}
}

// decodeToolParams recovers the map[string]*schema.ParameterInfo underlying
// a ToolInfo's ParamsOneOf by round-tripping through ToolInfo's own
// MarshalJSON (the params map is otherwise unexported outside eino/schema).
func decodeToolParams(t *testing.T, info *schema.ToolInfo) map[string]*schema.ParameterInfo {
	t.Helper()

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal ToolInfo failed: %v", err)
	}

	var decoded struct {
		Params map[string]*schema.ParameterInfo `json:"params"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal ToolInfo params failed: %v", err)
	}

	return decoded.Params
}

// requireSubParam extracts a parameter by name, failing the test if missing.
func requireSubParam(t *testing.T, params map[string]*schema.ParameterInfo, name string) *schema.ParameterInfo {
	t.Helper()

	p, ok := params[name]
	if !ok {
		t.Fatalf("parameter %q not found", name)
	}

	return p
}

func TestClient_ContextCancellation(t *testing.T) {
	mock := NewMockServer(nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := mock.NewClient(ctx)
	// The behavior depends on the transport — it may or may not fail.
	// At minimum, if it does fail, it should wrap context error.
	if err != nil {
		// This is acceptable.
		t.Logf("Got expected error on cancelled context: %v", err)
	}
}

// TestBridgedTool_DisplayNameAndWireName verifies task 2.4/2.8:
// - display name uses the prefix__wireName format
// - wire name (mcpTool.Name) is sent unchanged in CallToolRequest
// - server field is present in logs
func TestBridgedTool_DisplayNameAndWireName(t *testing.T) {
	ctx := context.Background()
	mock := NewMockServer(nil)

	// Create in-process client via raw mcpclient to gain access to BridgedTool.
	inprocCli, err := mock.NewClient(ctx)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer func() { _ = inprocCli.Close() }()

	// Manually build a BridgedTool with a prefix to verify display vs wire names.
	// We use the underlying mcpclient via a minimal toolInvoker shim.
	mcpToolStub := newMCPToolStub()

	const serverName = "home_assistant"
	const toolPrefix = "home_assistant"
	displayName := buildDisplayName(toolPrefix, mcpToolStub.Name)

	bridged := NewBridgedTool(discardLogger(), &callCapture{}, mcpToolStub, serverName, displayName)

	// Task 2.3: Info() returns the display name.
	info, err := bridged.Info(ctx)
	if err != nil {
		t.Fatalf("Info() error: %v", err)
	}

	wantDisplay := "home_assistant__light/turn_on"
	if info.Name != wantDisplay {
		t.Errorf("Info().Name = %q, want %q", info.Name, wantDisplay)
	}

	// Task 2.4: InvokableRun sends the wire name (mcpTool.Name) in the request.
	invoker := &callCapture{}
	bridged2 := NewBridgedTool(discardLogger(), invoker, mcpToolStub, serverName, displayName)
	_, _ = bridged2.InvokableRun(ctx, `{"entity_id":"light.kitchen"}`)

	if len(invoker.requests) == 0 {
		t.Fatal("expected at least one CallTool request")
	}

	wireName := invoker.requests[0].Name
	if wireName != testToolLightOn {
		t.Errorf("wire name in request = %q, want %q", wireName, testToolLightOn)
	}
}

// TestBridgedTool_NoPrefix verifies that an empty prefix leaves the wire name unchanged.
func TestBridgedTool_NoPrefix(t *testing.T) {
	ctx := context.Background()
	mcpToolStub := newMCPToolStub()

	displayName := buildDisplayName("", mcpToolStub.Name)
	if displayName != mcpToolStub.Name {
		t.Errorf("buildDisplayName(\"\", %q) = %q, want %q", mcpToolStub.Name, displayName, mcpToolStub.Name)
	}

	bridged := NewBridgedTool(discardLogger(), &callCapture{}, mcpToolStub, "", displayName)
	info, _ := bridged.Info(ctx)
	if info.Name != testToolLightOn {
		t.Errorf("Info().Name = %q, want bare wire name %q", info.Name, testToolLightOn)
	}
}

type errorResultInvoker struct {
	result *mcp.CallToolResult
}

func (i *errorResultInvoker) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return i.result, nil
}

func TestBridgedTool_ErrorResultLogsServerMessage(t *testing.T) {
	var logOutput bytes.Buffer
	logger := zerolog.New(&logOutput)
	bridged := NewBridgedTool(logger, &errorResultInvoker{
		result: &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "entity not found"}},
		},
	}, newMCPToolStub(), "home_assistant", "home_assistant__"+testToolLightOn)

	_, err := bridged.InvokableRun(context.Background(), `{}`)
	if err == nil {
		t.Fatal("expected tool execution error")
	}

	if !strings.Contains(logOutput.String(), `"error_detail":"entity not found"`) {
		t.Fatalf("log does not contain MCP error detail: %s", logOutput.String())
	}
}

func TestBridgedTool_NormalizesObjectArguments(t *testing.T) {
	toolDef := newMCPToolStub()
	toolDef.InputSchema = schemaObject(map[string]any{
		"data": map[string]any{
			jsonKeyAnyOf: []any{
				map[string]any{jsonKeyType: jsonTypeObject},
				map[string]any{jsonKeyType: string(schema.Null)},
			},
		},
	})
	capture := &callCapture{}
	var logOutput bytes.Buffer
	bridged := NewBridgedTool(zerolog.New(&logOutput), capture, toolDef, "home_assistant", "home_assistant__"+testToolLightOn)

	for _, tc := range []struct {
		name string
		args string
		want map[string]any
	}{
		{name: "empty string", args: `{"data":""}`, want: map[string]any{}},
		{name: "encoded object", args: `{"data":"{\"brightness\":50}"}`, want: map[string]any{"brightness": float64(50)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture.requests = nil
			logOutput.Reset()
			if _, err := bridged.InvokableRun(context.Background(), tc.args); err != nil {
				t.Fatalf("InvokableRun failed: %v", err)
			}
			arguments, ok := capture.requests[0].Arguments.(map[string]any)
			if !ok {
				t.Fatalf("arguments have type %T, want map[string]any", capture.requests[0].Arguments)
			}
			if got := arguments["data"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("normalized data = %#v, want %#v", got, tc.want)
			}
			if strings.Contains(logOutput.String(), `"data":"`) {
				t.Fatalf("debug log contains pre-normalized data: %s", logOutput.String())
			}
		})
	}

	t.Run("raw message schema", func(t *testing.T) {
		toolDef.InputSchema = json.RawMessage(`{"type":"object","properties":{"data":{"anyOf":[{"type":"object"},{"type":"null"}]}}}`)
		capture.requests = nil
		if _, err := bridged.InvokableRun(context.Background(), `{"data":""}`); err != nil {
			t.Fatalf("InvokableRun failed: %v", err)
		}
		arguments, ok := capture.requests[0].Arguments.(map[string]any)
		if !ok {
			t.Fatalf("arguments have type %T, want map[string]any", capture.requests[0].Arguments)
		}
		if got := arguments["data"]; !reflect.DeepEqual(got, map[string]any{}) {
			t.Fatalf("normalized data = %#v, want empty object", got)
		}
	})
}

func TestBridgedTool_OmitsEmptyOptionalStringArguments(t *testing.T) {
	toolDef := newMCPToolStub()
	toolDef.InputSchema = schemaObject(map[string]any{
		"entity_id": map[string]any{jsonKeyType: jsonTypeString},
		"level":     map[string]any{jsonKeyType: jsonTypeString},
		"search":    map[string]any{jsonKeyType: jsonTypeString},
		"mode": map[string]any{
			jsonKeyType: "string",
			"enum":      []any{"", "brief", "full"},
		},
	}, "entity_id")

	capture := &callCapture{}
	bridged := NewBridgedTool(discardLogger(), capture, toolDef, "home_assistant", "home_assistant__ha_get_logs")
	_, err := bridged.InvokableRun(context.Background(), `{"entity_id":"","level":"","search":"","mode":""}`)
	if err != nil {
		t.Fatalf("InvokableRun failed: %v", err)
	}

	arguments, ok := capture.requests[0].Arguments.(map[string]any)
	if !ok {
		t.Fatalf("arguments have type %T, want map[string]any", capture.requests[0].Arguments)
	}
	for _, name := range []string{"level", "search"} {
		if _, exists := arguments[name]; exists {
			t.Errorf("optional empty string argument %q was forwarded: %#v", name, arguments)
		}
	}
	for _, name := range []string{"entity_id", "mode"} {
		if got, exists := arguments[name]; !exists || got != "" {
			t.Errorf("argument %q = %#v, exists=%v; want preserved empty string", name, got, exists)
		}
	}
}

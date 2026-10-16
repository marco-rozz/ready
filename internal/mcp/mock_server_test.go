package mcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testToolLightOn     = toolLightTurnOn
	testToolLightOff    = toolLightTurnOff
	testToolClimate     = toolClimateSetTemp
	testToolSwitch      = toolSwitchToggle
	testToolGetState    = toolGetState
	testToolClimateHvac = toolClimateSetHvacMode
	testEntityKitchen   = "light.kitchen"
	testEntityLiving    = "light.living_room"
	testEntityClimate   = "climate.thermostat"
	testEntityGarage    = "switch.garage_door"
	testAttrBrightness  = "brightness"
	testAttrTemperature = "temperature"
	testAttrEntityID    = "entity_id"
	testStateOff        = "off"
	testStateOn         = "on"
)

// setupTestServer connects an in-process client to a fresh mock server via
// MockServer.NewClient, which encapsulates the in-memory transport pair (P4).
// The returned Client's Close shuts down both sides.
func setupTestServer(t *testing.T) (*Client, *MockServer) {
	t.Helper()

	mock := NewMockServer(nil)

	client, err := mock.NewClient(context.Background())
	if err != nil {
		t.Fatalf("failed to create in-process client: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client, mock
}

// callTool is a small helper around Client.session.CallTool so the test body
// stays focused on the assertion rather than params/session plumbing.
func callTool(t *testing.T, client *Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	result, err := client.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool(%q) failed: %v", name, err)
	}

	return result
}

func TestMockServer_ListTools(t *testing.T) {
	client, _ := setupTestServer(t)
	ctx := context.Background()

	result, err := client.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}

	expectedTools := map[string]bool{
		testToolLightOn:     false,
		testToolLightOff:    false,
		testToolClimate:     false,
		testToolSwitch:      false,
		testToolGetState:    false,
		testToolClimateHvac: false,
	}

	for _, tool := range result.Tools {
		if _, ok := expectedTools[tool.Name]; ok {
			expectedTools[tool.Name] = true
		}
	}

	for name, found := range expectedTools {
		if !found {
			t.Errorf("expected tool %q not found", name)
		}
	}
}

func TestMockServer_LightTurnOn(t *testing.T) {
	client, mock := setupTestServer(t)

	result := callTool(t, client, testToolLightOn, map[string]any{testAttrEntityID: testEntityKitchen, testAttrBrightness: float64(128)})

	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}

	// Verify state was updated.
	state, ok := mock.GetState(testEntityKitchen)
	if !ok {
		t.Fatal("expected state for light.kitchen")
	}

	if state.State != testStateOn {
		t.Errorf("expected state 'on', got %q", state.State)
	}

	if state.Attributes[testAttrBrightness] != float64(128) {
		t.Errorf("expected brightness 128, got %v", state.Attributes[testAttrBrightness])
	}

	// Verify call was recorded.
	calls := mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}

	if calls[0].ToolName != testToolLightOn {
		t.Errorf("expected tool name 'light/turn_on', got %q", calls[0].ToolName)
	}
}

func TestMockServer_LightTurnOff(t *testing.T) {
	client, mock := setupTestServer(t)

	// First turn it on.
	callTool(t, client, testToolLightOn, map[string]any{testAttrEntityID: testEntityKitchen})

	// Now turn off.
	result := callTool(t, client, testToolLightOff, map[string]any{testAttrEntityID: testEntityKitchen})

	if result.IsError {
		t.Fatalf("tool returned error")
	}

	state, _ := mock.GetState(testEntityKitchen)
	if state.State != testStateOff {
		t.Errorf("expected state 'off', got %q", state.State)
	}
}

func TestMockServer_ClimateSetTemperature(t *testing.T) {
	client, mock := setupTestServer(t)

	result := callTool(t, client, testToolClimate, map[string]any{testAttrEntityID: testEntityClimate, testAttrTemperature: float64(23.5)})

	if result.IsError {
		t.Fatalf("tool returned error")
	}

	state, _ := mock.GetState(testEntityClimate)
	if state.Attributes[testAttrTemperature] != float64(23.5) {
		t.Errorf("expected temperature 23.5, got %v", state.Attributes[testAttrTemperature])
	}
}

func TestMockServer_SwitchToggle(t *testing.T) {
	client, mock := setupTestServer(t)

	// Default state is testStateOff, toggle should make it testStateOn.
	callTool(t, client, testToolSwitch, map[string]any{testAttrEntityID: testEntityGarage})

	state, _ := mock.GetState(testEntityGarage)
	if state.State != testStateOn {
		t.Errorf("expected state 'on' after toggle, got %q", state.State)
	}

	// Toggle again → off.
	callTool(t, client, testToolSwitch, map[string]any{testAttrEntityID: testEntityGarage})

	state, _ = mock.GetState(testEntityGarage)
	if state.State != testStateOff {
		t.Errorf("expected state 'off' after second toggle, got %q", state.State)
	}
}

func TestMockServer_GetState_Exists(t *testing.T) {
	client, _ := setupTestServer(t)

	result := callTool(t, client, testToolGetState, map[string]any{testAttrEntityID: testEntityLiving})

	if result.IsError {
		t.Fatalf("tool returned error")
	}

	// Should contain the entity state.
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
}

func TestMockServer_GetState_NotFound(t *testing.T) {
	client, _ := setupTestServer(t)

	result := callTool(t, client, testToolGetState, map[string]any{testAttrEntityID: "light.nonexistent"})

	// Should return a result (not an MCP error) with error info.
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
}

func TestMockServer_CustomInitialStates(t *testing.T) {
	custom := map[string]*EntityState{
		"light.bedroom": {
			EntityID:   "light.bedroom",
			State:      testStateOn,
			Attributes: map[string]any{testAttrBrightness: float64(100)},
		},
	}

	mock := NewMockServer(custom)

	state, ok := mock.GetState("light.bedroom")
	if !ok {
		t.Fatal("expected custom state")
	}

	if state.State != testStateOn {
		t.Errorf("expected state 'on', got %q", state.State)
	}

	// Default states should not be present.
	_, ok = mock.GetState(testEntityKitchen)
	if ok {
		t.Error("default state should not be present with custom states")
	}
}

func TestMockServer_ResetCalls(t *testing.T) {
	client, mock := setupTestServer(t)

	callTool(t, client, testToolLightOn, map[string]any{testAttrEntityID: testEntityKitchen})

	if len(mock.Calls()) == 0 {
		t.Fatal("expected recorded calls")
	}

	mock.ResetCalls()

	if len(mock.Calls()) != 0 {
		t.Error("expected empty calls after reset")
	}
}

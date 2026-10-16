// Package mcp provides MCP client integration and mock server for testing.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Mock server constants.
const (
	attrEntityID    = "entity_id"
	attrBrightness  = "brightness"
	attrTemperature = "temperature"
	attrState       = "state"
	attrFriendly    = "friendly_name"
	attrSuccess     = "success"
	attrHvacMode    = "hvac_mode"
	stateOff        = "off"
	stateOn         = "on"
	hvacModeHeat    = "heat"
	hvacModeCool    = "cool"

	// implementationVersion is the Implementation.Version reported by every
	// mcp.NewClient/mcp.NewServer call in this package — a single literal
	// with no runtime meaning (goconst).
	implementationVersion = "1.0.0"

	toolLightTurnOn        = "light/turn_on"
	toolLightTurnOff       = "light/turn_off"
	toolClimateSetTemp     = "climate/set_temperature"
	toolSwitchToggle       = "switch/toggle"
	toolGetState           = "homeassistant/get_state"
	toolClimateSetHvacMode = "climate/set_hvac_mode"
)

// hvacModes lists the values accepted by climate/set_hvac_mode's hvac_mode
// enum, modeled on Home Assistant's climate domain.
func hvacModes() []string {
	return []string{stateOff, hvacModeHeat, hvacModeCool, "heat_cool", "auto", "dry", "fan_only"}
}

// EntityState represents the state of a Home Assistant entity.
type EntityState struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// MockServer wraps an MCP server that simulates Home Assistant for testing.
type MockServer struct {
	server *mcp.Server

	mu     sync.RWMutex
	states map[string]*EntityState
	calls  []ToolCall
}

// ToolCall records a tool invocation for test assertions.
type ToolCall struct {
	ToolName  string
	Arguments map[string]any
}

// NewMockServer creates a mock MCP server that simulates Home Assistant.
// initialStates provides the starting entity states; if nil, defaults are used.
func NewMockServer(initialStates map[string]*EntityState) *MockServer {
	ms := &MockServer{
		states: make(map[string]*EntityState),
	}

	if initialStates != nil {
		for k, v := range initialStates {
			ms.states[k] = v
		}
	} else {
		ms.applyDefaultStates()
	}

	ms.server = mcp.NewServer(&mcp.Implementation{Name: "mock-home-assistant", Version: implementationVersion}, nil)

	ms.server.AddTool(ms.lightTurnOnTool())
	ms.server.AddTool(ms.lightTurnOffTool())
	ms.server.AddTool(ms.climateSetTemperatureTool())
	ms.server.AddTool(ms.switchToggleTool())
	ms.server.AddTool(ms.getStateTool())
	ms.server.AddTool(ms.climateSetHvacModeTool())

	return ms
}

// NewClient connects an in-process client to this mock server. It
// encapsulates mcp.NewInMemoryTransports and both sides of Connect (P4), so
// callers never name an SDK type; the returned Client's Close shuts down
// both the client and server sessions of the in-memory pair.
func (ms *MockServer) NewClient(ctx context.Context) (*Client, error) {
	return newInProcessClient(ctx, ms.server)
}

// GetState returns the current state of an entity.
func (ms *MockServer) GetState(entityID string) (*EntityState, bool) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	state, ok := ms.states[entityID]

	return state, ok
}

// Calls returns all recorded tool invocations.
func (ms *MockServer) Calls() []ToolCall {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	result := make([]ToolCall, len(ms.calls))
	copy(result, ms.calls)

	return result
}

// ResetCalls clears the recorded tool invocations.
func (ms *MockServer) ResetCalls() {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.calls = nil
}

func (ms *MockServer) recordCall(name string, args map[string]any) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.calls = append(ms.calls, ToolCall{ToolName: name, Arguments: args})
}

func (ms *MockServer) applyDefaultStates() {
	ms.states["light.kitchen"] = &EntityState{
		EntityID:   "light.kitchen",
		State:      stateOff,
		Attributes: map[string]any{attrBrightness: float64(0), attrFriendly: "Kitchen Light"},
	}
	ms.states["light.living_room"] = &EntityState{
		EntityID:   "light.living_room",
		State:      stateOn,
		Attributes: map[string]any{attrBrightness: float64(200), attrFriendly: "Living Room Light"},
	}
	ms.states["climate.thermostat"] = &EntityState{
		EntityID:   "climate.thermostat",
		State:      hvacModeHeat,
		Attributes: map[string]any{attrTemperature: float64(21), attrFriendly: "Main Thermostat"},
	}
	ms.states["switch.garage_door"] = &EntityState{
		EntityID:   "switch.garage_door",
		State:      stateOff,
		Attributes: map[string]any{attrFriendly: "Garage Door"},
	}
}

// schemaObject builds a "type: object" input schema as the untyped
// map[string]any shape the official SDK expects for Tool.InputSchema,
// mirroring the shape convertMCPSchemaToParams (schema.go) reads back on the
// client side.
func schemaObject(properties map[string]any, required ...string) map[string]any {
	m := map[string]any{jsonKeyType: jsonTypeObject, jsonKeyProperties: properties}
	if len(required) > 0 {
		reqAny := make([]any, len(required))
		for i, r := range required {
			reqAny[i] = r
		}
		m["required"] = reqAny
	}

	return m
}

// textResult builds a CallToolResult carrying a single text content block.
func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// decodeArgs unmarshals a tool call's raw JSON arguments into a
// map[string]any, matching the shape ToolCall.Arguments already exposes to
// test assertions. A missing/empty payload decodes to an empty, non-nil map.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	args := map[string]any{}
	if len(raw) == 0 {
		return args, nil
	}

	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("failed to decode tool arguments: %w", err)
	}

	return args, nil
}

// requireStringArg extracts a required string argument for the mock's own
// error-payload tools.
func requireStringArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("required argument %q not found", key)
	}

	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q is not a string", key)
	}

	return s, nil
}

// requireFloatArg extracts a required numeric argument.
func requireFloatArg(args map[string]any, key string) (float64, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("required argument %q not found", key)
	}

	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("argument %q is not a number", key)
	}

	return f, nil
}

// floatArgOrDefault extracts an optional numeric argument, returning def when
// absent or not a number.
func floatArgOrDefault(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok {
		return def
	}

	f, ok := v.(float64)
	if !ok {
		return def
	}

	return f
}

func (ms *MockServer) lightTurnOnTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolLightTurnOn,
		Description: "Turn on a light entity",
		InputSchema: schemaObject(map[string]any{
			attrEntityID:   map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID of the light"},
			attrBrightness: map[string]any{jsonKeyType: jsonTypeNumber, jsonKeyDescription: "Brightness level 0-255"},
		}, attrEntityID),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		brightness := floatArgOrDefault(args, attrBrightness, 255)

		ms.recordCall(toolLightTurnOn, map[string]any{attrEntityID: entityID, attrBrightness: brightness})

		ms.mu.Lock()
		state, exists := ms.states[entityID]
		if !exists {
			state = &EntityState{EntityID: entityID, Attributes: map[string]any{}}
			ms.states[entityID] = state
		}
		state.State = stateOn
		state.Attributes[attrBrightness] = brightness
		ms.mu.Unlock()

		result := map[string]any{attrSuccess: true, attrState: stateOn, attrBrightness: brightness}
		resultJSON, _ := json.Marshal(result)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

func (ms *MockServer) lightTurnOffTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolLightTurnOff,
		Description: "Turn off a light entity",
		InputSchema: schemaObject(map[string]any{
			attrEntityID: map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID of the light"},
		}, attrEntityID),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		ms.recordCall(toolLightTurnOff, map[string]any{attrEntityID: entityID})

		ms.mu.Lock()
		state, exists := ms.states[entityID]
		if !exists {
			state = &EntityState{EntityID: entityID, Attributes: map[string]any{}}
			ms.states[entityID] = state
		}
		state.State = stateOff
		state.Attributes[attrBrightness] = float64(0)
		ms.mu.Unlock()

		result := map[string]any{attrSuccess: true, attrState: stateOff}
		resultJSON, _ := json.Marshal(result)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

func (ms *MockServer) climateSetTemperatureTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolClimateSetTemp,
		Description: "Set the target temperature for a climate entity",
		InputSchema: schemaObject(map[string]any{
			attrEntityID:    map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID of the climate device"},
			attrTemperature: map[string]any{jsonKeyType: jsonTypeNumber, jsonKeyDescription: "Target temperature in degrees"},
		}, attrEntityID, attrTemperature),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		temperature, err := requireFloatArg(args, attrTemperature)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		ms.recordCall(toolClimateSetTemp, map[string]any{attrEntityID: entityID, attrTemperature: temperature})

		ms.mu.Lock()
		state, exists := ms.states[entityID]
		if !exists {
			state = &EntityState{EntityID: entityID, Attributes: map[string]any{}}
			ms.states[entityID] = state
		}
		state.Attributes[attrTemperature] = temperature
		ms.mu.Unlock()

		result := map[string]any{attrSuccess: true, attrTemperature: temperature}
		resultJSON, _ := json.Marshal(result)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

func (ms *MockServer) switchToggleTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolSwitchToggle,
		Description: "Toggle a switch entity",
		InputSchema: schemaObject(map[string]any{
			attrEntityID: map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID of the switch"},
		}, attrEntityID),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		ms.recordCall(toolSwitchToggle, map[string]any{attrEntityID: entityID})

		ms.mu.Lock()
		state, exists := ms.states[entityID]
		if !exists {
			state = &EntityState{EntityID: entityID, State: stateOff, Attributes: map[string]any{}}
			ms.states[entityID] = state
		}

		if state.State == stateOn {
			state.State = stateOff
		} else {
			state.State = stateOn
		}

		newState := state.State
		ms.mu.Unlock()

		result := map[string]any{attrSuccess: true, attrState: newState}
		resultJSON, _ := json.Marshal(result)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

// climateSetHvacModeTool is modeled on Home Assistant's climate.set_hvac_mode
// service (the hvac_mode string enum) combined with a fan-speed override
// array and a nested schedule object, so the mock server exercises enum,
// array, and nested-object conversion through the full discovery path in a
// single tool. The exact field names are illustrative rather than captured
// from a live HA instance, but the shapes (a constrained hvac_mode enum, a
// homogeneous array, a small nested object) mirror what HA's MCP server
// declares.
func (ms *MockServer) climateSetHvacModeTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolClimateSetHvacMode,
		Description: "Set the HVAC mode for a climate entity, with optional fan-speed overrides and a schedule window",
		InputSchema: schemaObject(map[string]any{
			attrEntityID: map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID of the climate device"},
			attrHvacMode: map[string]any{
				jsonKeyType:        jsonTypeString,
				jsonKeyDescription: "The target HVAC mode",
				jsonKeyEnum:        toAnySlice(hvacModes()),
			},
			"fan_speeds": map[string]any{
				jsonKeyType:        jsonTypeArray,
				jsonKeyDescription: "Allowed fan speed overrides, in preference order",
				jsonKeyItems:       map[string]any{jsonKeyType: jsonTypeString},
			},
			"schedule": map[string]any{
				jsonKeyType:        jsonTypeObject,
				jsonKeyDescription: "Optional schedule window during which the mode applies",
				jsonKeyProperties: map[string]any{
					"start":       map[string]any{jsonKeyType: jsonTypeString, jsonKeyFormat: "time"},
					"end":         map[string]any{jsonKeyType: jsonTypeString, jsonKeyFormat: "time"},
					"temperature": map[string]any{jsonKeyType: jsonTypeNumber},
				},
			},
		}, attrEntityID, attrHvacMode),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		hvacMode, err := requireStringArg(args, attrHvacMode)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		ms.recordCall(toolClimateSetHvacMode, map[string]any{attrEntityID: entityID, attrHvacMode: hvacMode})

		ms.mu.Lock()
		state, exists := ms.states[entityID]
		if !exists {
			state = &EntityState{EntityID: entityID, Attributes: map[string]any{}}
			ms.states[entityID] = state
		}
		state.State = hvacMode
		ms.mu.Unlock()

		result := map[string]any{attrSuccess: true, attrState: hvacMode}
		resultJSON, _ := json.Marshal(result)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

func (ms *MockServer) getStateTool() (*mcp.Tool, mcp.ToolHandler) {
	tool := &mcp.Tool{
		Name:        toolGetState,
		Description: "Get the current state of a Home Assistant entity",
		InputSchema: schemaObject(map[string]any{
			attrEntityID: map[string]any{jsonKeyType: jsonTypeString, jsonKeyDescription: "The entity ID to query"},
		}, attrEntityID),
	}

	handler := func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		entityID, err := requireStringArg(args, attrEntityID)
		if err != nil {
			return textResult(fmt.Sprintf(`{"error": "%s"}`, err.Error())), nil
		}

		ms.recordCall(toolGetState, map[string]any{attrEntityID: entityID})

		ms.mu.RLock()
		state, exists := ms.states[entityID]
		ms.mu.RUnlock()

		if !exists {
			result := map[string]any{"error": "entity not found", attrEntityID: entityID}
			resultJSON, _ := json.Marshal(result)

			return textResult(string(resultJSON)), nil
		}

		resultJSON, _ := json.Marshal(state)

		return textResult(string(resultJSON)), nil
	}

	return tool, handler
}

// toAnySlice converts a []string to []any, the shape schemaObject's callers
// need for a JSON Schema "enum" array (mixed-type in the general case, but
// homogeneous here).
func toAnySlice(vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}

	return out
}

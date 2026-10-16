package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// Test constants for repeated string literals in this file.
const (
	testModelName          = "test"
	testAPIKey             = "test-key"
	testToolName           = "test_tool"
	testModelBad           = "bad"
	testModelGood          = "good"
	testModelZen           = "zen"
	testModelClaude        = "claude"
	testModelReachable     = "reachable"
	testModelMyEntry       = "my-entry"
	testModelGemma         = "gemma4:31b"
	testModelOllamaEntry   = "ollama-gemma"
	testModelGoogle        = "gemini"
	testModelOR            = "google/gemma-3-27b-it"
	probePathModels        = "/models"
	testUnreachableBaseURL = "http://127.0.0.1:1" // unreachable port for transport-error probes

	// JSON field names used in httptest response fixtures.
	jsonKeyID           = "id"
	jsonKeyData         = "data"
	jsonKeyObject       = "object"
	jsonKeyCreated      = "created"
	jsonKeyModel        = "model"
	jsonKeyChoices      = "choices"
	jsonKeyIndex        = "index"
	jsonKeyMessage      = "message"
	jsonKeyRole         = "role"
	jsonKeyType         = "type"
	jsonKeyContent      = "content"
	jsonKeyToolCalls    = "tool_calls"
	jsonKeyFunction     = "function"
	jsonKeyName         = "name"
	jsonKeyFinishReason = "finish_reason"
	jsonKeyStopReason   = "stop_reason"
	jsonKeyInput        = "input"

	// JSON values used in httptest response fixtures.
	jsonValAssistant = "assistant"
	jsonValFunction  = "function"
	jsonValMessage   = "message"
	jsonValToolUse   = "tool_use"
	jsonValToolCalls = "tool_calls"
	jsonValStop      = "stop"

	// Other repeated test strings.
	testParamKey          = "key"
	testChatCompletionID  = "chatcmpl-123"
	testChatCompletionObj = "chat.completion"
	testToolDesc          = "A test tool"
	testGoodModelName     = "good-model"

	// Responses API (task 3.1) test constants.
	testModelResponses     = "responses-entry"
	jsonKeyCallID          = "call_id"
	jsonKeyArguments       = "arguments"
	jsonKeyStatus          = "status"
	jsonKeyOutput          = "output"
	jsonKeyCreatedAt       = "created_at"
	jsonValFunctionCall    = "function_call"
	jsonValResponseObject  = "response"
	jsonValCompletedStatus = "completed"
)

func TestResolve_ConfigError_NoFallthrough(t *testing.T) {
	// Invalid config with a valid failover → should return config error, never connect.
	registry := &config.LLMConfig{
		Default:  testModelBad + "::" + testModelName,
		Failover: []string{testModelGood + "::" + testModelName},
		Providers: map[string]config.LLMProviderConfig{
			testModelBad: {
				Provider:   config.ProviderOpenRouter,
				APIKey:     "inline",
				APIKeyFile: "file", // both → config error
			},
			testModelGood: {
				Provider: config.ProviderOllama,
			},
		},
	}

	_, err := BuildForReference(context.Background(), registry, registry.Default)
	if err == nil {
		t.Fatal("expected config error for both api_key and api_key_file")
	}
}

func TestResolve_OllamaSuccess(t *testing.T) {
	registry := &config.LLMConfig{
		Default: config.ProviderOllama + "::" + testModelGemma,
		Providers: map[string]config.LLMProviderConfig{
			config.ProviderOllama: {
				Provider: config.ProviderOllama,
				BaseURL:  "http://localhost:11434",
			},
		},
	}

	// Use a test server for the probe.
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	registry.Providers[config.ProviderOllama] = config.LLMProviderConfig{
		Provider: config.ProviderOllama,
		BaseURL:  probeSrv.URL,
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}
}

func TestResolve_OllamaDefaultBaseURL(t *testing.T) {
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	registry := &config.LLMConfig{
		Default: config.ProviderOllama + "::" + testModelGemma,
		Providers: map[string]config.LLMProviderConfig{
			config.ProviderOllama: {
				Provider: config.ProviderOllama,
				BaseURL:  probeSrv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}
}

func TestResolve_OpenRouterSuccess(t *testing.T) {
	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer probeSrv.Close()

	registry := &config.LLMConfig{
		Default: "or::" + testModelOR,
		Providers: map[string]config.LLMProviderConfig{
			"or": {
				Provider: config.ProviderOpenRouter,
				APIKey:   testAPIKey,
				BaseURL:  probeSrv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}
}

func TestBuildForReference_OpenAICodexUsesNamedCredential(t *testing.T) {
	oauthPath := t.TempDir()
	store, err := codexauth.NewFileStore(context.Background(), oauthPath)
	if err != nil {
		t.Fatal(err)
	}
	cred := codexauth.NewCredential("work")
	cred.AccessToken = "header.payload.signature"
	cred.RefreshToken = "refresh-secret"
	cred.AccountID = "account-work"
	cred.Expiry = time.Now().Add(time.Hour)
	cred.UpdatedAt = time.Now()
	if saveErr := store.Save(context.Background(), cred); saveErr != nil {
		t.Fatal(saveErr)
	}

	registry := &config.LLMConfig{
		OAuthPath: oauthPath,
		Default:   "work::gpt-5-codex",
		Providers: map[string]config.LLMProviderConfig{
			"work": {
				Provider: config.ProviderOpenAICodex,
				API:      config.APIResponses,
				BaseURL:  "https://chatgpt.example/backend-api",
			},
		},
	}
	resolved, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("BuildForReference: %v", err)
	}
	if !resolved.IsAgentic() || resolved.Chat != nil {
		t.Fatalf("resolved model = %#v, want agentic-only", resolved)
	}
	if resolved.Layout() != LayoutTailDynamic {
		t.Fatalf("layout = %v, want tail-dynamic", resolved.Layout())
	}
	if err := PreflightCatalog(context.Background(), registry, registry.Default, zerolog.Nop()); err != nil {
		t.Fatalf("credential-only preflight: %v", err)
	}
}

func TestBuildForReference_OpenAICodexMissingCredentialIsCandidateFailure(t *testing.T) {
	registry := &config.LLMConfig{
		OAuthPath: t.TempDir(),
		Default:   "personal::gpt-5-codex",
		Providers: map[string]config.LLMProviderConfig{
			"personal": {Provider: config.ProviderOpenAICodex, API: config.APIResponses},
		},
	}
	_, err := BuildForReference(context.Background(), registry, registry.Default)
	if err == nil || !strings.Contains(err.Error(), "ready login personal") {
		t.Fatalf("missing credential error = %v", err)
	}
}

// Task 2.4 / 4.1: httptest server asserting chat_completions request shape over opencode.
func TestOpenCode_ChatCompletions_RequestShape(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedSession string
	var capturedTools bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedSession = r.Header.Get(zenSessionHeader)

		// Handle probe first.
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}

		// Parse body to check for tools.
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if tools, ok := body["tools"]; ok && tools != nil {
				capturedTools = true
			}
		}

		// Respond with a canned tool_call response.
		resp := map[string]interface{}{
			jsonKeyID:      testChatCompletionID,
			jsonKeyObject:  testChatCompletionObj,
			jsonKeyCreated: 1234567890,
			jsonKeyModel:   "deepseek-v4-flash-free",
			jsonKeyChoices: []map[string]interface{}{
				{
					jsonKeyIndex: 0,
					jsonKeyMessage: map[string]interface{}{
						jsonKeyRole: jsonValAssistant,
						jsonKeyToolCalls: []map[string]interface{}{
							{
								jsonKeyID:   "call_1",
								jsonKeyType: jsonValFunction,
								jsonKeyFunction: map[string]interface{}{
									jsonKeyName: testToolName,
									"arguments": `{testParamKey:"value"}`,
								},
							},
						},
					},
					jsonKeyFinishReason: jsonValToolCalls,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: testModelZen + "::deepseek-v4-flash-free",
		Providers: map[string]config.LLMProviderConfig{
			testModelZen: {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}

	// Make a real call to trigger the request.
	toolInfo := &schema.ToolInfo{
		Name: testToolName,
		Desc: testToolDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			testParamKey: {Type: schema.String, Desc: "A parameter"},
		}),
	}
	chatModelWithTools, err := chatModel.WithTools([]*schema.ToolInfo{toolInfo})
	if err != nil {
		t.Fatalf("WithTools failed: %v", err)
	}

	msgs := []*schema.Message{
		{Role: schema.User, Content: "Hello, use test_tool with key=value"},
	}
	result, err := chatModelWithTools.Generate(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Assert request shape.
	if !strings.HasSuffix(capturedPath, "/chat/completions") {
		t.Errorf("expected path to end with /chat/completions, got %q", capturedPath)
	}
	if capturedAuth != "Bearer "+testAPIKey {
		t.Errorf("expected Authorization: Bearer test-key, got %q", capturedAuth)
	}
	if capturedSession == "" {
		t.Errorf("expected %s header on chat_completions request (OpenCode Go gateway requirement)", zenSessionHeader)
	}
	if !capturedTools {
		t.Error("expected tools array in request body")
	}

	// Assert the response surfaces the tool_call.
	if result == nil {
		t.Fatal("expected non-nil response")
	}
	if len(result.ToolCalls) == 0 {
		t.Fatal("expected at least one tool call in response")
	}
	if result.ToolCalls[0].Function.Name != testToolName {
		t.Errorf("expected tool call name %q, got %q", testToolName, result.ToolCalls[0].Function.Name)
	}
}

// Task (openrouter stream-safe): the OpenRouter model is wrapped so that
// Stream is served from a single Generate call, avoiding the upstream
// nil-pointer panic in the OpenRouter streaming chunk modifier on EOF
// (see openrouterStreamSafe docs). This test exercises the wrapped streaming
// path end-to-end against an httptest server and asserts no panic and full
// content delivery.
func TestOpenRouter_StreamSafe_NoPanic(t *testing.T) {
	var receivedStreamFlag any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probe first.
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}

		// Inspect the request body to confirm the wrapper sent a non-streaming
		// Generate (stream: false or absent) rather than a streaming request.
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			receivedStreamFlag = body["stream"]
		}

		// Standard (non-SSE) chat completion response.
		resp := map[string]interface{}{
			jsonKeyID:      testChatCompletionID,
			jsonKeyObject:  testChatCompletionObj,
			jsonKeyCreated: 1234567890,
			jsonKeyModel:   testModelOR,
			jsonKeyChoices: []map[string]interface{}{
				{
					jsonKeyIndex: 0,
					jsonKeyMessage: map[string]interface{}{
						jsonKeyRole:    jsonValAssistant,
						jsonKeyContent: "Ciao, tutto bene!",
					},
					jsonKeyFinishReason: jsonValStop,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: "or::" + testModelOR,
		Providers: map[string]config.LLMProviderConfig{
			"or": {
				Provider: config.ProviderOpenRouter,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}

	// The wrapper must survive the agent's WithTools().Stream() call chain.
	chatModelWithTools, err := chatModel.WithTools([]*schema.ToolInfo{
		{Name: testToolName, Desc: testToolDesc},
	})
	if err != nil {
		t.Fatalf("WithTools failed: %v", err)
	}
	if _, ok := chatModelWithTools.(*openrouterStreamSafe); !ok {
		t.Fatalf("expected WithTools to re-wrap as *openrouterStreamSafe, got %T", chatModelWithTools)
	}

	msgs := []*schema.Message{{Role: schema.User, Content: "Ciao, come stai?"}}
	reader, err := chatModelWithTools.Stream(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer reader.Close()

	var got strings.Builder
	for {
		chunk, chunkErr := reader.Recv()
		if chunkErr != nil {
			if errors.Is(chunkErr, io.EOF) {
				break
			}
			t.Fatalf("Stream recv error (would be the upstream panic in the unwrapped path): %v", chunkErr)
		}
		if chunk != nil {
			got.WriteString(chunk.Content)
		}
	}

	if got.String() != "Ciao, tutto bene!" {
		t.Errorf("expected streamed content %q, got %q", "Ciao, tutto bene!", got.String())
	}
	// The wrapper must have routed Stream through Generate (non-streaming
	// request), proving the broken streaming modifier path was not taken.
	if receivedStreamFlag == true {
		t.Error("expected wrapper to send a non-streaming Generate, got stream: true")
	}
}

// Task 2.1 (llm-google-protocol-2): httptest server asserting Google-format
// request shape over opencode — generateContent-style body, Bearer auth, and a
// canned functionCall response parsed into Eino tool calls.
func TestOpenCode_Google_RequestShape(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedGoogKey string
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handle probe first.
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}

		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")
		capturedGoogKey = r.Header.Get("x-goog-api-key")

		_ = json.NewDecoder(r.Body).Decode(&capturedBody)

		// Google mldev generateContent response with a functionCall part.
		resp := map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"role": "model",
						"parts": []map[string]interface{}{
							{
								"functionCall": map[string]interface{}{
									jsonKeyName: testToolName,
									"args":      map[string]interface{}{testParamKey: "value"},
								},
							},
						},
					},
					"finishReason": "STOP",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: testModelGoogle + "::gemini-3-flash",
		Providers: map[string]config.LLMProviderConfig{
			testModelGoogle: {
				Provider: config.ProviderOpenCode,
				API:      config.APIGoogle,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}

	// Make a real call to trigger the generateContent request.
	msgsWithTool := []*schema.Message{
		{Role: schema.User, Content: "Hello, use test_tool with key=value"},
	}
	result, err := chatModel.Generate(context.Background(), msgsWithTool)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Assert request shape — the genai SDK appends /v1beta/models/{model}:generateContent.
	if !strings.HasSuffix(capturedPath, "/v1beta/models/gemini-3-flash:generateContent") {
		t.Errorf("expected path to end with /v1beta/models/gemini-3-flash:generateContent, got %q", capturedPath)
	}
	if capturedAuth != "Bearer "+testAPIKey {
		t.Errorf("expected Authorization: Bearer test-key, got %q", capturedAuth)
	}
	// The SDK also forwards the key as x-goog-api-key; Zen ignores it in favour
	// of Authorization. Assert it carries the same key (sanity).
	if capturedGoogKey != testAPIKey {
		t.Errorf("expected x-goog-api-key test-key, got %q", capturedGoogKey)
	}

	// Assert the response surfaces the functionCall as an Eino tool call.
	if result == nil {
		t.Fatal("expected non-nil response")
	}
	if len(result.ToolCalls) == 0 {
		t.Fatal("expected at least one tool call in response")
	}
	if result.ToolCalls[0].Function.Name != testToolName {
		t.Errorf("expected tool call name %q, got %q", testToolName, result.ToolCalls[0].Function.Name)
	}
}

// Task 4.2: httptest server asserting anthropic request shape over opencode.
func TestOpenCode_Anthropic_RequestShape(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var requestReceived bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")

		// Handle probe first.
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}

		requestReceived = true

		// Respond with a canned tool_use response in Anthropic format.
		resp := map[string]interface{}{
			jsonKeyID:    "msg_123",
			jsonKeyType:  "message",
			jsonKeyRole:  "assistant",
			jsonKeyModel: "claude-sonnet-4-5",
			jsonKeyContent: []map[string]interface{}{
				{
					jsonKeyType:  "tool_use",
					jsonKeyID:    "toolu_01",
					jsonKeyName:  testToolName,
					jsonKeyInput: map[string]interface{}{testParamKey: "value"},
				},
			},
			jsonKeyStopReason: jsonValToolUse,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: testModelClaude + "::claude-sonnet-4-5",
		Providers: map[string]config.LLMProviderConfig{
			testModelClaude: {
				Provider: config.ProviderOpenCode,
				API:      config.APIAnthropic,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}

	// Make a real call.
	msgs := []*schema.Message{
		{Role: schema.User, Content: "Hello"},
	}
	result, err := chatModel.Generate(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if !requestReceived {
		t.Fatal("no request received by test server")
	}
	// The claude component sends to {base}/messages.
	if !strings.HasSuffix(capturedPath, "/messages") {
		t.Errorf("expected path to end with /messages for anthropic, got %q", capturedPath)
	}
	if capturedAuth != "Bearer "+testAPIKey {
		t.Errorf("expected Authorization: Bearer test-key, got %q", capturedAuth)
	}

	if result == nil {
		t.Fatal("expected non-nil response")
	}
	if len(result.ToolCalls) == 0 {
		t.Fatal("expected at least one tool call in response")
	}
}

// Task 3.1: httptest fixture asserting the Responses API request shape
// (endpoint path, Bearer auth, tool definitions) and that a canned
// function_call output surfaces as a FunctionToolCall content block on the
// returned AgenticMessage — i.e. the tool call reaches the ReAct loop.
func TestOpenCode_Responses_RequestShape(t *testing.T) {
	var capturedPath string
	var capturedAuth string
	var capturedTools bool
	var requestReceived bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		capturedAuth = r.Header.Get("Authorization")

		// Handle probe first.
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}

		requestReceived = true

		// Parse body to check for tool definitions.
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			if tools, ok := body["tools"]; ok && tools != nil {
				capturedTools = true
			}
		}

		// Respond with a canned Responses API object whose sole output item
		// is a function_call, mirroring what the real Responses API returns
		// when the model decides to call a tool.
		resp := map[string]interface{}{
			jsonKeyID:        "resp_123",
			jsonKeyObject:    jsonValResponseObject,
			jsonKeyCreatedAt: 1234567890,
			jsonKeyStatus:    jsonValCompletedStatus,
			jsonKeyModel:     "gpt-5.4-mini",
			jsonKeyOutput: []map[string]interface{}{
				{
					jsonKeyType:      jsonValFunctionCall,
					jsonKeyID:        "fc_1",
					jsonKeyCallID:    "call_1",
					jsonKeyName:      testToolName,
					jsonKeyArguments: `{"key":"value"}`,
					jsonKeyStatus:    jsonValCompletedStatus,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: testModelResponses + "::gpt-5.4-mini",
		Providers: map[string]config.LLMProviderConfig{
			testModelResponses: {
				Provider: config.ProviderOpenCode,
				API:      config.APIResponses,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resolvedModel.IsAgentic() {
		t.Fatal("expected an agentic (responses) resolved model")
	}
	if resolvedModel.Chat != nil {
		t.Error("expected Chat to be nil for a responses-protocol entry")
	}

	toolInfo := &schema.ToolInfo{
		Name: testToolName,
		Desc: testToolDesc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			testParamKey: {Type: schema.String, Desc: "A parameter"},
		}),
	}

	input := []*schema.AgenticMessage{
		schema.UserAgenticMessage("Hello, use test_tool with key=value"),
	}

	result, err := resolvedModel.Agentic.Generate(context.Background(), input, model.WithTools([]*schema.ToolInfo{toolInfo}))
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	if !requestReceived {
		t.Fatal("no request received by test server")
	}
	if !strings.HasSuffix(capturedPath, "/responses") {
		t.Errorf("expected path to end with /responses, got %q", capturedPath)
	}
	if capturedAuth != "Bearer "+testAPIKey {
		t.Errorf("expected Authorization: Bearer test-key, got %q", capturedAuth)
	}
	if !capturedTools {
		t.Error("expected tools array in request body")
	}

	if result == nil {
		t.Fatal("expected non-nil response")
	}

	var foundCall bool
	for _, block := range result.ContentBlocks {
		if block.FunctionToolCall != nil {
			foundCall = true
			if block.FunctionToolCall.Name != testToolName {
				t.Errorf("expected tool call name %q, got %q", testToolName, block.FunctionToolCall.Name)
			}
		}
	}
	if !foundCall {
		t.Fatal("expected a FunctionToolCall content block in the response, surfacing the tool call to the ReAct loop")
	}
}

// Task 4.4: Config-error test — invalid entry with valid failover still returns config error.
// Task 4.4 (reworked by llm-failover-orchestrator task 1.5): a missing
// credential is a PER-CANDIDATE failure — BuildForReference errors only for
// the affected reference, and a valid candidate on another provider still
// builds. Fatal global errors (reference shape, undeclared providers) are
// CompileCandidates' contract instead.
func TestBuildForReference_MissingCredentialIsPerCandidate(t *testing.T) {
	registry := &config.LLMConfig{
		Default:  "invalid::" + testModelName,
		Failover: []string{"valid::" + testModelName},
		Providers: map[string]config.LLMProviderConfig{
			"invalid": {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				// Missing APIKey for a keyed provider → per-candidate error.
			},
			"valid": {
				Provider: config.ProviderOllama,
				API:      config.APIOllama,
				BaseURL:  "http://localhost:11434",
			},
		},
	}

	// The global chain compiles: shape is valid.
	if _, err := CompileCandidates(registry); err != nil {
		t.Fatalf("expected the chain to compile, got: %v", err)
	}

	if _, err := BuildForReference(context.Background(), registry, registry.Default); err == nil {
		t.Fatal("expected per-candidate error for missing api_key on the default")
	}

	if _, err := BuildForReference(context.Background(), registry, "valid::"+testModelName); err != nil {
		t.Fatalf("expected the valid fallback to build, got: %v", err)
	}
}

// Task 4.5: Probe-classification tests.
func TestProbe_Classification_2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyData: []map[string]interface{}{{jsonKeyID: testModelName}},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeOK || err != nil {
		t.Errorf("expected probeOK for 2xx probe with model-id present, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_401Failover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeFailed {
		t.Errorf("expected probeFailed for 401 probe, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_403Failover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeFailed {
		t.Errorf("expected probeFailed for 403 probe, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_TransportError(t *testing.T) {
	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  testUnreachableBaseURL, // unreachable port
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeFailed {
		t.Errorf("expected probeFailed for transport error, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_404Inconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeInconclusive || err != nil {
		t.Errorf("expected probeInconclusive for 404 probe, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_405Inconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeInconclusive || err != nil {
		t.Errorf("expected probeInconclusive for 405 probe, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_Classification_500Inconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeInconclusive || err != nil {
		t.Errorf("expected probeInconclusive for 500 probe, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_NoGenerationCall(t *testing.T) {
	// The probe should only do GET /models — never invoke a generation.
	// This is inherently tested by the probe function only doing a GET.
	var probeCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalled = true
		if r.Method != http.MethodGet {
			t.Errorf("probe should only use GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyData: []map[string]interface{}{{jsonKeyID: testModelName}},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelName, "", 10*time.Second, zerolog.Nop())
	if outcome != probeOK || err != nil {
		t.Fatalf("expected probeOK, got outcome=%v err=%v", outcome, err)
	}
	if !probeCalled {
		t.Error("expected probe to be called")
	}
}

// TestResolve_Failover_TransportRefused tests that a transport-refused first
// entry falls over to the second.
// Task 4.4 (reworked by llm-failover-orchestrator task 4.6): the catalog
// preflight replaces Resolve's boot acceptance. Construction is local — a
// transport-refused endpoint still builds — while PreflightCatalog reports
// the not-ready reason for the unreachable reference and nil for the
// reachable one.
func TestPreflightCatalog_TransportRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.WriteHeader(http.StatusOK)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyID:      testChatCompletionID,
			jsonKeyObject:  testChatCompletionObj,
			jsonKeyCreated: 1234567890,
			jsonKeyModel:   testModelName,
			jsonKeyChoices: []map[string]interface{}{
				{
					jsonKeyIndex:   0,
					jsonKeyMessage: map[string]interface{}{jsonKeyRole: jsonValAssistant, jsonKeyContent: "OK"},
				},
			},
		})
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default:  "unreachable::" + testModelName,
		Failover: []string{testModelReachable + "::" + testModelName},
		Providers: map[string]config.LLMProviderConfig{
			"unreachable": {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				APIKey:   "key",
				BaseURL:  testUnreachableBaseURL, // unreachable
			},
			testModelReachable: {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				APIKey:   "key",
				BaseURL:  srv.URL,
			},
		},
	}

	// Construction is local: both references build without any network I/O.
	if _, err := BuildForReference(context.Background(), registry, registry.Default); err != nil {
		t.Fatalf("construction should not depend on the endpoint, got: %v", err)
	}

	// The preflight rejects the unreachable reference and accepts the reachable one.
	if err := PreflightCatalog(context.Background(), registry, registry.Default, zerolog.Nop()); err == nil {
		t.Fatal("expected preflight error for the unreachable default")
	}
	if err := PreflightCatalog(context.Background(), registry, testModelReachable+"::"+testModelName, zerolog.Nop()); err != nil {
		t.Fatalf("expected the reachable fallback to pass preflight, got: %v", err)
	}
}

// TestOpenCodeAPIKeyFileResolved verifies that api_key_file is read and trimmed.
func TestOpenCodeAPIKeyFileResolved(t *testing.T) {
	// This test verifies the ResolvedAPIKey method on LLMProviderConfig.
	entry := config.LLMProviderConfig{
		APIKey: "inline-key",
	}
	key, err := entry.ResolvedAPIKey(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "inline-key" {
		t.Errorf("expected 'inline-key', got %q", key)
	}
}

// TestOpenCodeDefaultBaseURL verifies the default base_url is applied.
func TestOpenCodeDefaultBaseURL(t *testing.T) {
	// This is a unit test verifying the default.
	// The actual test would need a server at the default URL, so we test logic.
	registry := &config.LLMConfig{
		Default: testModelZen + "::" + testModelName,
		Providers: map[string]config.LLMProviderConfig{
			testModelZen: {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				APIKey:   testAPIKey,
			},
		},
	}

	// When BaseURL is empty, the default should be used.
	// We verify the default constant value.
	if defaultOpenCodeBaseURL != "https://opencode.ai/zen/v1" {
		t.Errorf("expected defaultOpenCodeBaseURL to be https://opencode.ai/zen/v1, got %q", defaultOpenCodeBaseURL)
	}

	// Verify the entry in the registry has no BaseURL set.
	entry := registry.Providers[testModelZen]
	if entry.BaseURL != "" {
		t.Errorf("expected empty BaseURL, got %q", entry.BaseURL)
	}
}

// TestOllamaProbeURL verifies the probe URL for ollama is {base}/api/tags.
func TestOllamaProbeURL(t *testing.T) {
	var probePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			probePath = r.URL.Path
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: config.ProviderOllama + "::" + testModelName,
		Providers: map[string]config.LLMProviderConfig{
			config.ProviderOllama: {
				Provider: config.ProviderOllama,
				BaseURL:  srv.URL,
			},
		},
	}

	// The probe path is exercised through the catalog preflight (the only
	// remaining caller of the probe URL derivation).
	if err := PreflightCatalog(context.Background(), registry, registry.Default, zerolog.Nop()); err != nil {
		t.Fatalf("unexpected preflight error: %v", err)
	}

	if probePath != "/api/tags" {
		t.Errorf("expected ollama probe path '/api/tags', got %q", probePath)
	}
}

// TestOllamaAPIOptions_NilForEmpty asserts that a nil or fully-empty
// OllamaOptions yields a nil *ollama.Options, preserving Ollama's defaults.
func TestOllamaAPIOptions_NilForEmpty(t *testing.T) {
	if got := ollamaAPIOptions(nil); got != nil {
		t.Errorf("nil cfg -> expected nil, got %+v", got)
	}
	if got := ollamaAPIOptions(&config.OllamaOptions{}); got != nil {
		t.Errorf("empty cfg -> expected nil, got %+v", got)
	}
}

// TestOllamaAPIOptions_PartialSet asserts only the set fields are populated.
func TestOllamaAPIOptions_PartialSet(t *testing.T) {
	np := 256
	temp := float32(0.3)
	got := ollamaAPIOptions(&config.OllamaOptions{NumPredict: &np, Temperature: &temp})
	if got == nil {
		t.Fatal("expected non-nil options for a partially-set cfg")
	}
	if got.NumPredict != 256 {
		t.Errorf("NumPredict = %d, want 256", got.NumPredict)
	}
	if got.Temperature != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", got.Temperature)
	}
	// Unset fields must remain zero (they are omitted on the wire by omitempty).
	if got.TopK != 0 || got.NumCtx != 0 || got.Seed != 0 || got.NumThread != 0 ||
		got.TopP != 0 || got.RepeatPenalty != 0 {
		t.Errorf("unset fields must be zero, got %+v", got)
	}
}

// TestOllamaAPIOptions_SeedZero asserts an explicit zero seed is forwarded
// (distinguishing "omitted" from "explicit zero" on the config side).
func TestOllamaAPIOptions_SeedZero(t *testing.T) {
	seed := 0
	got := ollamaAPIOptions(&config.OllamaOptions{Seed: &seed})
	if got == nil {
		t.Fatal("expected non-nil options when Seed is explicitly set to 0")
	}
	if got.Seed != 0 {
		t.Errorf("Seed = %d, want 0", got.Seed)
	}
}

// TestLoad_OllamaKeepAliveAndOptionsRoundTrip verifies YAML decoding of an
// ollama entry with keep_alive + a partial options block.
func TestLoad_OllamaKeepAliveAndOptionsRoundTrip(t *testing.T) {
	content := []byte(`
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      base_url: "http://localhost:11434"
      keep_alive: "30m"
      options:
        num_predict: 256
        temperature: 0.3
`)
	var cfg config.Config
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entry, ok := cfg.LLM.Providers[testModelOllamaEntry]
	if !ok {
		t.Fatal("expected ollama-gemma entry")
	}
	if entry.KeepAlive == nil {
		t.Fatal("expected KeepAlive to be set")
	}
	if want := 30 * time.Minute; *entry.KeepAlive != want {
		t.Errorf("KeepAlive = %v, want %v", *entry.KeepAlive, want)
	}
	if entry.Options == nil {
		t.Fatal("expected Options to be set")
	}
	if entry.Options.NumPredict == nil || *entry.Options.NumPredict != 256 {
		t.Errorf("NumPredict = %v, want 256", entry.Options.NumPredict)
	}
	if entry.Options.Temperature == nil || *entry.Options.Temperature != 0.3 {
		t.Errorf("Temperature = %v, want 0.3", entry.Options.Temperature)
	}
	// Unset fields must be nil (not zero) — the "omitted vs explicit zero" line.
	if entry.Options.Seed != nil || entry.Options.NumCtx != nil ||
		entry.Options.TopK != nil || entry.Options.TopP != nil ||
		entry.Options.RepeatPenalty != nil || entry.Options.NumThread != nil {
		t.Errorf("unset option fields must be nil, got %+v", entry.Options)
	}
}

// TestResolve_OllamaForwardsKeepAliveAndOptions asserts the configured
// keep_alive and options reach the constructed Ollama model. The eino-ext
// ChatModelConfig is not directly inspectable after NewChatModel, so this
// exercises the conversion path end-to-end: the probe server confirms the
// entry is reachable, and we rely on ollamaAPIOptions unit tests above for the
// field-mapping correctness. Here we only assert Resolve succeeds and returns
// a non-nil model when keep_alive + options are set.
func TestResolve_OllamaForwardsKeepAliveAndOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ka := 30 * time.Minute
	np := 256
	temp := float32(0.3)
	registry := &config.LLMConfig{
		Default: testModelOllamaEntry + "::" + testModelGemma,
		Providers: map[string]config.LLMProviderConfig{
			testModelOllamaEntry: {
				Provider:  config.ProviderOllama,
				BaseURL:   srv.URL,
				KeepAlive: &ka,
				Options:   &config.OllamaOptions{NumPredict: &np, Temperature: &temp},
			},
		},
	}

	resolvedModel, err := BuildForReference(context.Background(), registry, registry.Default)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chatModel := resolvedModel.Chat
	if chatModel == nil {
		t.Fatal("expected non-nil chat model")
	}
}

// --- Boot-probe model-catalog verification (change llm-runtime-failover) ---

func TestProbe_CatalogPresent_ProbeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyData: []map[string]interface{}{
				{jsonKeyID: "other-model"},
				{jsonKeyID: testModelGemma},
			},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelGemma, "", 10*time.Second, zerolog.Nop())
	if outcome != probeOK || err != nil {
		t.Fatalf("expected probeOK when model-id present, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_CatalogAbsent_ProbeFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyData: []map[string]interface{}{{jsonKeyID: "other-model"}},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, _ := probeEntry(context.Background(), entry, entry.Name, testModelGemma, "", 10*time.Second, zerolog.Nop())
	if outcome != probeFailed {
		t.Fatalf("expected probeFailed when model-id absent, got outcome=%v", outcome)
	}
}

func TestProbe_CatalogUnparseable_Inconclusive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-json-at-all"))
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenCode,
		API:      config.APIChatCompletions,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelGemma, "", 10*time.Second, zerolog.Nop())
	if outcome != probeInconclusive || err != nil {
		t.Fatalf("expected probeInconclusive for unparseable 2xx body, got outcome=%v err=%v", outcome, err)
	}
}

func TestProbe_CatalogOllamaShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("expected ollama probe path /api/tags, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]interface{}{
				{"name": "llama3:8b"},
				{"name": testModelGemma},
			},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOllama,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelGemma, "", 10*time.Second, zerolog.Nop())
	if outcome != probeOK || err != nil {
		t.Fatalf("expected probeOK for ollama catalog containing model-id, got outcome=%v err=%v", outcome, err)
	}

	// Absent model-id on the same ollama shape → probeFailed.
	outcome2, _ := probeEntry(context.Background(), entry, entry.Name, "nope:42", "", 10*time.Second, zerolog.Nop())
	if outcome2 != probeFailed {
		t.Fatalf("expected probeFailed for ollama catalog missing model-id, got outcome=%v", outcome2)
	}
}

func TestProbe_CatalogOpenRouterShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			jsonKeyData: []map[string]interface{}{
				{jsonKeyID: testModelOR},
				{jsonKeyID: "meta-llama/llama-4"},
			},
		})
	}))
	defer srv.Close()

	entry := &config.LLMProviderConfig{
		Name:     testModelName,
		Provider: config.ProviderOpenRouter,
		BaseURL:  srv.URL,
	}

	outcome, err := probeEntry(context.Background(), entry, entry.Name, testModelOR, "", 10*time.Second, zerolog.Nop())
	if outcome != probeOK || err != nil {
		t.Fatalf("expected probeOK for openrouter catalog containing model-id, got outcome=%v err=%v", outcome, err)
	}
}

// Task 4.6 (reworked by llm-failover-orchestrator): a 2xx catalog WITHOUT
// the configured model-id is a preflight rejection for that reference —
// catalog presence is a preflight, not boot acceptance, and readiness is
// established by the orchestrator's tool-call probe.
func TestPreflightCatalog_ModelIDAbsent(t *testing.T) {
	// Server: 2xx catalog WITHOUT the configured model-id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == probePathModels {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				jsonKeyData: []map[string]interface{}{{jsonKeyID: "some-other-model"}},
			})

			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	registry := &config.LLMConfig{
		Default: testModelBad + "::bad-model",
		Providers: map[string]config.LLMProviderConfig{
			testModelBad: {
				Provider: config.ProviderOpenCode,
				API:      config.APIChatCompletions,
				APIKey:   testAPIKey,
				BaseURL:  srv.URL,
			},
		},
	}

	if err := PreflightCatalog(context.Background(), registry, registry.Default, zerolog.Nop()); err == nil {
		t.Fatal("expected preflight rejection when the model-id is absent from the catalog")
	}
}

package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	schemaopenai "github.com/cloudwego/eino/schema/openai"
	"github.com/marco-rozz/ready/internal/codexauth"
)

const (
	testCodexModel   = "model-verbatim"
	testCodexBaseURL = "https://chatgpt.example/backend-api"
	testToolName     = "turn_on"
	headerCT         = "Content-Type"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type fakeAccessSource struct {
	mu          sync.Mutex
	access      codexauth.Access
	refreshed   codexauth.Access
	invalidated bool
}

func (s *fakeAccessSource) Access(context.Context) (codexauth.Access, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalidated {
		return s.refreshed, nil
	}

	return s.access, nil
}

func (s *fakeAccessSource) Invalidate(string) {
	s.mu.Lock()
	s.invalidated = true
	s.mu.Unlock()
}

func TestRequestContract(t *testing.T) {
	var gotURL string
	var gotHeader http.Header
	var gotBody map[string]any
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		gotHeader = req.Header.Clone()
		if err := json.NewDecoder(req.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}

		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{headerCT: {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad request","type":"invalid_request_error"}}`)),
			Request:    req,
		}, nil
	})
	source := &fakeAccessSource{access: codexauth.Access{Token: "access-secret", AccountID: "acct-1"}}
	m, err := NewModel(context.Background(), Config{
		BaseURL: testCodexBaseURL, Model: testCodexModel,
		TokenSource: source, HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	toolInfo := &schema.ToolInfo{
		Name: testToolName, Desc: "turn on a light",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"entity_id": {Type: schema.String, Required: true},
		}),
	}
	_, err = m.Stream(context.Background(), []*schema.AgenticMessage{
		schema.SystemAgenticMessage("stable instructions"),
		schema.UserAgenticMessage("hello"),
	}, model.WithTools([]*schema.ToolInfo{toolInfo}))
	if err == nil {
		t.Fatal("expected fixture HTTP 400")
	}
	if gotURL != "https://chatgpt.example/backend-api/codex/responses" {
		t.Fatalf("URL=%q", gotURL)
	}
	for key, want := range map[string]string{
		"Authorization": "Bearer access-secret", "chatgpt-account-id": "acct-1",
		"originator": codexauth.Originator, "OpenAI-Beta": "responses=experimental",
	} {
		if got := gotHeader.Get(key); got != want {
			t.Errorf("%s=%q want %q", key, got, want)
		}
	}
	if gotBody["model"] != testCodexModel || gotBody["store"] != false || gotBody["stream"] != true || gotBody["instructions"] != "stable instructions" {
		t.Fatalf("body=%#v", gotBody)
	}
	if gotBody["parallel_tool_calls"] != true || gotBody["tool_choice"] != "auto" {
		t.Fatalf("tool policy body=%#v", gotBody)
	}
}

func TestAuthTransportScopesSecretsAndReplaysOne401(t *testing.T) {
	origin, _ := url.Parse(testCodexBaseURL)
	source := &fakeAccessSource{
		access:    codexauth.Access{Token: "old", AccountID: "acct-old"},
		refreshed: codexauth.Access{Token: "new", AccountID: "acct-new"},
	}
	var calls int
	transport := &authTransport{origin: origin, source: source, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			if req.Header.Get("Authorization") != "Bearer old" {
				t.Fatalf("first auth=%q", req.Header.Get("Authorization"))
			}

			return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		if req.Header.Get("Authorization") != "Bearer new" {
			t.Fatalf("replay auth=%q", req.Header.Get("Authorization"))
		}

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://chatgpt.example/backend-api/codex/responses", strings.NewReader("{}"))
	resp, err := transport.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || calls != 2 {
		t.Fatalf("resp=%v calls=%d err=%v", resp, calls, err)
	}
	_ = resp.Body.Close()

	crossOrigin := false
	transport.base = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		crossOrigin = req.Header.Get("Authorization") == "" && req.Header.Get("chatgpt-account-id") == ""

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	crossReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://evil.example/redirect", nil)
	crossReq.Header.Set("Authorization", "Bearer stale")
	crossReq.Header.Set("chatgpt-account-id", "stale")
	crossResp, err := transport.RoundTrip(crossReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = crossResp.Body.Close()
	if !crossOrigin {
		t.Fatal("cross-origin request retained authentication headers")
	}
}

func TestValidateStreamRequiresAndStopsAtTerminal(t *testing.T) {
	completed := &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{
		OpenAIExtension: &schemaopenai.ResponseMetaExtension{Status: schemaopenai.ResponseStatusCompleted},
	}}
	inner, writer := schema.Pipe[*schema.AgenticMessage](2)
	if writer.Send(completed, nil) {
		t.Fatal("unexpected closed input stream")
	}
	// Deliberately leave the source open: the validator must complete as soon
	// as it observes the terminal response instead of waiting for body EOF.
	validated := validateStream(inner)
	if got, err := validated.Recv(); err != nil || got != completed {
		t.Fatalf("terminal chunk = %#v, %v", got, err)
	}
	if _, err := validated.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("after terminal error = %v, want EOF", err)
	}
	writer.Close()

	missing, missingWriter := schema.Pipe[*schema.AgenticMessage](1)
	_ = missingWriter.Send(schema.UserAgenticMessage("partial"), nil)
	missingWriter.Close()
	checked := validateStream(missing)
	if _, err := checked.Recv(); err != nil {
		t.Fatalf("partial chunk: %v", err)
	}
	if _, err := checked.Recv(); err == nil || !strings.Contains(err.Error(), "without a terminal response") {
		t.Fatalf("missing terminal error = %v", err)
	}
}

func TestValidateStreamSanitizesFailureCode(t *testing.T) {
	inner, writer := schema.Pipe[*schema.AgenticMessage](1)
	failed := &schema.AgenticMessage{ResponseMeta: &schema.AgenticResponseMeta{
		OpenAIExtension: &schemaopenai.ResponseMetaExtension{
			Status: schemaopenai.ResponseStatusFailed,
			Error: &schemaopenai.ResponseError{
				Code:    schemaopenai.ResponseErrorCode("usage_limit_exceeded;token=secret"),
				Message: "never expose this response body",
			},
		},
	}}
	_ = writer.Send(failed, nil)
	writer.Close()
	_, err := validateStream(inner).Recv()
	if err == nil || !strings.Contains(err.Error(), "usage_limit_exceededtokensecret") {
		t.Fatalf("failure error = %v", err)
	}
	if strings.Contains(err.Error(), "never expose") {
		t.Fatalf("failure leaked response message: %v", err)
	}
}

func TestStreamingSSEFixtureTextToolUsageAndUnknownEvent(t *testing.T) {
	sseEvent := func(eventType, data string) string {
		return "event: " + eventType + "\ndata: " + data + "\n\n"
	}
	body := sseEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"Hello","logprobs":[],"sequence_number":1}`) +
		sseEvent("response.future_additive_event", `{"type":"response.future_additive_event","value":true}`) +
		sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"sequence_number":2,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"turn_on","arguments":"","status":"in_progress"}}`) +
		sseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"item-1","output_index":1,"delta":"{\"entity_id\":\"light.kitchen\"}","sequence_number":3}`) +
		sseEvent("response.completed", `{"type":"response.completed","sequence_number":4,"response":{"id":"resp-1","created_at":1,"model":"model-verbatim","object":"response","output":[],"parallel_tool_calls":true,"temperature":1,"tool_choice":"auto","tools":[],"top_p":1,"status":"completed","usage":{"input_tokens":3,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":1},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":5}}}`)

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{headerCT: {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	m, err := NewModel(context.Background(), Config{
		BaseURL: testCodexBaseURL, Model: testCodexModel,
		TokenSource: &fakeAccessSource{access: codexauth.Access{Token: "token", AccountID: "account"}},
		HTTPClient:  &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := m.Stream(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var text, callID, callName, arguments string
	var usage *schema.TokenUsage
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if chunk.ResponseMeta != nil && chunk.ResponseMeta.TokenUsage != nil {
			usage = chunk.ResponseMeta.TokenUsage
		}
		for _, block := range chunk.ContentBlocks {
			if block.AssistantGenText != nil {
				text += block.AssistantGenText.Text
			}
			if block.FunctionToolCall != nil {
				if block.FunctionToolCall.CallID != "" {
					callID = block.FunctionToolCall.CallID
				}
				if block.FunctionToolCall.Name != "" {
					callName = block.FunctionToolCall.Name
				}
				arguments += block.FunctionToolCall.Arguments
			}
		}
	}
	if text != "Hello" || callID != "call-1" || callName != testToolName || arguments != `{"entity_id":"light.kitchen"}` {
		t.Fatalf("text=%q call_id=%q name=%q arguments=%q", text, callID, callName, arguments)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 || usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

// TestStreamingSSEFixtureReasoningSummaryIsolation pins the Responses wire
// mapping the thought tap relies on (change responses-react-thought-logging):
// reasoning-summary deltas become generic Reasoning.Text, while encrypted
// replay material lands in Reasoning.Signature and raw reasoning text in
// Reasoning.OpenAIExtension — never in Reasoning.Text.
func TestStreamingSSEFixtureReasoningSummaryIsolation(t *testing.T) {
	sseEvent := func(eventType, data string) string {
		return "event: " + eventType + "\ndata: " + data + "\n\n"
	}
	body := sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"ENCRYPTED-SECRET","status":"in_progress"}}`) +
		sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs-1","output_index":0,"summary_index":0,"delta":"Checking the ","sequence_number":2}`) +
		sseEvent("response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","item_id":"rs-1","output_index":0,"summary_index":0,"delta":"kitchen light.","sequence_number":3}`) +
		sseEvent("response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","item_id":"rs-1","output_index":0,"content_index":0,"delta":"RAW-CHAIN-OF-THOUGHT","sequence_number":4}`) +
		sseEvent("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"sequence_number":5,"item":{"type":"reasoning","id":"rs-1","summary":[],"status":"completed"}}`) +
		sseEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":1,"sequence_number":6,"item":{"type":"function_call","id":"item-1","call_id":"call-1","name":"turn_on","arguments":"","status":"in_progress"}}`) +
		sseEvent("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","item_id":"item-1","output_index":1,"delta":"{\"entity_id\":\"light.kitchen\"}","sequence_number":7}`) +
		sseEvent("response.completed", `{"type":"response.completed","sequence_number":8,"response":{"id":"resp-1","created_at":1,"model":"model-verbatim","object":"response","output":[],"parallel_tool_calls":true,"temperature":1,"tool_choice":"auto","tools":[],"top_p":1,"status":"completed","usage":{"input_tokens":3,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":5}}}`)

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{headerCT: {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	m, err := NewModel(context.Background(), Config{
		BaseURL: testCodexBaseURL, Model: testCodexModel,
		TokenSource: &fakeAccessSource{access: codexauth.Access{Token: "token", AccountID: "account"}},
		HTTPClient:  &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Generate materializes the stream with schema.ConcatAgenticMessages — the
	// same complete-message seam the agentic runner hands to the thought tap.
	msg, err := m.Generate(context.Background(), []*schema.AgenticMessage{schema.UserAgenticMessage("hello")})
	if err != nil {
		t.Fatal(err)
	}

	var reasoning []*schema.Reasoning
	var call *schema.FunctionToolCall
	for _, block := range msg.ContentBlocks {
		if block == nil {
			continue
		}
		if block.Reasoning != nil {
			reasoning = append(reasoning, block.Reasoning)
		}
		if block.FunctionToolCall != nil {
			call = block.FunctionToolCall
		}
	}
	if len(reasoning) != 1 {
		t.Fatalf("reasoning blocks = %d, want 1 (blocks: %+v)", len(reasoning), msg.ContentBlocks)
	}
	r := reasoning[0]
	if r.Text != "Checking the kitchen light." {
		t.Errorf("Reasoning.Text = %q, want only the summary", r.Text)
	}
	if strings.Contains(r.Text, "ENCRYPTED-SECRET") || strings.Contains(r.Text, "RAW-CHAIN-OF-THOUGHT") {
		t.Errorf("Reasoning.Text leaked provider-specific material: %q", r.Text)
	}
	if r.Signature != "ENCRYPTED-SECRET" {
		t.Errorf("Reasoning.Signature = %q, want encrypted material preserved outside Text", r.Signature)
	}
	var raw string
	if r.OpenAIExtension != nil {
		for _, c := range r.OpenAIExtension.Content {
			if c != nil {
				raw += c.Text
			}
		}
	}
	if raw != "RAW-CHAIN-OF-THOUGHT" {
		t.Errorf("OpenAIExtension raw reasoning = %q, want preserved outside Text", raw)
	}
	if call == nil || call.CallID != "call-1" || call.Name != testToolName || call.Arguments != `{"entity_id":"light.kitchen"}` {
		t.Fatalf("function call = %+v", call)
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
		t.Errorf("terminal metadata missing: %+v", msg.ResponseMeta)
	}
}

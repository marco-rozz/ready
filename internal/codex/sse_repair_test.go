package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/codexauth"
)

// testAccessSource serves a static credential to the auth transport so the
// model can be exercised against a local test server.
type testAccessSource struct{}

func (testAccessSource) Access(context.Context) (codexauth.Access, error) {
	return codexauth.Access{Token: "test-token", AccountID: "test-account"}, nil
}

func (testAccessSource) Invalidate(string) {}

// sseFixtureServer replays a raw SSE body with the protocol content type.
func sseFixtureServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	return srv
}

// added/done events for one function call, no argument deltas — the shape
// the ChatGPT Codex backend emits for PARALLEL calls (captured 2026-10-20).
const parallelSSEFixture = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n" +
	"event: response.output_item.added\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_A\",\"call_id\":\"call_A\",\"name\":\"ha_state_query\",\"arguments\":\"\"}}\n\n" +
	"event: response.output_item.added\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_B\",\"call_id\":\"call_B\",\"name\":\"ha_state_query\",\"arguments\":\"\"}}\n\n" +
	"event: response.function_call_arguments.done\n" +
	"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_A\",\"output_index\":0,\"arguments\":\"{\\\"limit\\\":200}\"}\n\n" +
	"event: response.output_item.done\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_A\",\"call_id\":\"call_A\",\"name\":\"ha_state_query\",\"arguments\":\"{\\\"limit\\\":200}\",\"status\":\"completed\"}}\n\n" +
	"event: response.function_call_arguments.done\n" +
	"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_B\",\"output_index\":1,\"arguments\":\"{\\\"only_problem\\\":true}\"}\n\n" +
	"event: response.output_item.done\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_B\",\"call_id\":\"call_B\",\"name\":\"ha_state_query\",\"arguments\":\"{\\\"only_problem\\\":true}\",\"status\":\"completed\"}}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

// Single-call shape: the backend streams the arguments via deltas (the
// public-API behaviour).
const singleSSEFixture = "event: response.output_item.added\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_C\",\"call_id\":\"call_C\",\"name\":\"ha_state_query\",\"arguments\":\"\"}}\n\n" +
	"event: response.function_call_arguments.delta\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_C\",\"output_index\":0,\"delta\":\"{\\\"domain\\\":\"}\n\n" +
	"event: response.function_call_arguments.delta\n" +
	"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_C\",\"output_index\":0,\"delta\":\"\\\"light\\\"}\"}\n\n" +
	"event: response.function_call_arguments.done\n" +
	"data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_C\",\"output_index\":0,\"arguments\":\"{\\\"domain\\\":\\\"light\\\"}\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func toolInfos() []*schema.ToolInfo {
	return []*schema.ToolInfo{
		{
			Name: "ha_state_query",
			Desc: "Queries the home state.",
			ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
				"limit": {Type: schema.Number},
			}),
		},
	}
}

func readFullMessage(t *testing.T, m *Model) *schema.AgenticMessage {
	t.Helper()
	stream, err := m.Stream(context.Background(), []*schema.AgenticMessage{{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.UserInputText{Text: "stato casa"}),
		},
	}}, model.WithTools(toolInfos()))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var chunks []*schema.AgenticMessage
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			break
		}
		if chunk != nil {
			chunks = append(chunks, chunk)
		}
	}
	merged, err := schema.ConcatAgenticMessages(chunks)
	if err != nil {
		t.Fatalf("concat %d chunks: %v", len(chunks), err)
	}

	return merged
}

func TestSSERepair_ParallelCallsGetArguments(t *testing.T) {
	srv := sseFixtureServer(t, parallelSSEFixture)
	m, err := NewModel(context.Background(), Config{
		BaseURL:     srv.URL,
		Model:       "gpt-6-luna",
		TokenSource: testAccessSource{},
		HTTPClient:  &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}

	merged := readFullMessage(t, m)
	if len(merged.ContentBlocks) != 2 {
		t.Fatalf("want 2 function-call blocks, got %d", len(merged.ContentBlocks))
	}
	for i, b := range merged.ContentBlocks {
		fc := b.FunctionToolCall
		if fc == nil {
			t.Fatalf("block %d is not a function tool call", i)
		}
		if fc.Arguments == "" {
			t.Errorf("block %d (%s): arguments still empty — repair not applied", i, fc.CallID)
		}
	}
	if got := merged.ContentBlocks[0].FunctionToolCall.Arguments; got != `{"limit":200}` {
		t.Errorf("call A arguments = %q, want {\"limit\":200}", got)
	}
	if got := merged.ContentBlocks[1].FunctionToolCall.Arguments; got != `{"only_problem":true}` {
		t.Errorf("call B arguments = %q, want {\"only_problem\":true}", got)
	}
}

func TestSSERepair_SingleCallWithDeltasNotDuplicated(t *testing.T) {
	srv := sseFixtureServer(t, singleSSEFixture)
	m, err := NewModel(context.Background(), Config{
		BaseURL:     srv.URL,
		Model:       "gpt-6-luna",
		TokenSource: testAccessSource{},
		HTTPClient:  &http.Client{},
	})
	if err != nil {
		t.Fatal(err)
	}

	merged := readFullMessage(t, m)
	if len(merged.ContentBlocks) != 1 {
		t.Fatalf("want 1 function-call block, got %d", len(merged.ContentBlocks))
	}
	fc := merged.ContentBlocks[0].FunctionToolCall
	if fc == nil {
		t.Fatal("block 0 is not a function tool call")
	}
	if got := fc.Arguments; got != `{"domain":"light"}` {
		t.Errorf("arguments = %q, want {\"domain\":\"light\"} (deltas must not be duplicated by the repair)", got)
	}
}

func TestSSERepair_NonSSEBodyUntouched(t *testing.T) {
	body := `{"error":{"message":"nope"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wrapIfSSE(resp)

	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != body {
		t.Errorf("non-SSE body must pass through untouched, got %q", string(raw))
	}
	if _, ok := resp.Body.(*sseArgsRepairer); ok {
		t.Error("non-SSE body must not be wrapped")
	}
}

func TestSSERepair_MalformedDataLinePassesThrough(t *testing.T) {
	input := "data: {not json}\n\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_X\",\"output_index\":0,\"arguments\":\"{}\"}\n\n"
	rep := newSSEArgsRepairer(io.NopCloser(strings.NewReader(input)))
	out, err := io.ReadAll(rep)
	if err != nil {
		t.Fatal(err)
	}
	// The malformed line is passed through; the valid done event still gets
	// its synthetic delta.
	if !strings.Contains(string(out), "{not json}") {
		t.Error("malformed line must pass through untouched")
	}
	if strings.Count(string(out), `"type":"response.function_call_arguments.delta"`) != 1 {
		t.Errorf("want exactly one synthetic delta, got %s", string(out))
	}
}

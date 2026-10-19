package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/rs/zerolog"
)

// Shared fixture constants (goconst: repeated literals become constants).
// testProbeArgs / testDoneText / testToolType / testTurnChannel live in
// orchestrator_test.go (same package).
const (
	testRefP0            = "p0::m"
	testRefP1            = "p1::m"
	testChunkObj         = "chat.completion.chunk"
	testCompletionObj    = "chat.completion"
	testChatcmpl1        = "chatcmpl-1"
	testChatcmpl2        = "chatcmpl-2"
	testJSONRole         = "role"
	testJSONCreated      = "created"
	testJSONObject       = "object"
	testJSONModelKey     = "model"
	testJSONChoices      = "choices"
	testJSONIndex        = "index"
	testJSONDelta        = "delta"
	testAssistantRole    = "assistant"
	testJSONToolCalls    = "tool_calls"
	testJSONFinishReason = "finish_reason"
)

// ── The synthetic no-op tool ───────────────────────────────────────────────

// The probe tool's shape and side-effect freedom.
func TestProbeNoopTool(t *testing.T) {
	tool := &probeNoopTool{}

	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info() error: %v", err)
	}
	if info.Name != probeToolName {
		t.Errorf("tool name = %q, want %q", info.Name, probeToolName)
	}
	if info.ParamsOneOf == nil {
		t.Error("tool must declare a parameter schema")
	}

	// No side effects by construction: a constant result, no I/O.
	out, err := tool.InvokableRun(context.Background(), testProbeArgs)
	if err != nil {
		t.Fatalf("InvokableRun() error: %v", err)
	}
	if out != "probe ok" {
		t.Errorf("InvokableRun() = %q, want the constant result", out)
	}
}

// ── Readiness probe outcomes ───────────────────────────────────────────────

func probeOrchWithModel(m *probeMockModel) *Orchestrator {
	o := newTestOrch(newFakeClock(),
		mkCand("a::m", "a", "chat_completions", llm.LayoutTailDynamic,
			&fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Chat: m}

	return o
}

// A tool-capable model completes the synthetic tool-call exchange
// and passes the probe.
func TestProbeCandidate_ToolCapableSucceeds(t *testing.T) {
	o := probeOrchWithModel(&probeMockModel{behavior: 0})

	if err := o.probeCandidate(context.Background(), o.candidateCopy(0)); err != nil {
		t.Fatalf("expected the tool-capable model to pass, got: %v", err)
	}
}

// A text-capable but tool-incompatible model fails
// the probe — text-only capability is insufficient.
func TestProbeCandidate_TextOnlyFails(t *testing.T) {
	o := probeOrchWithModel(&probeMockModel{behavior: 1})

	err := o.probeCandidate(context.Background(), o.candidateCopy(0))
	if err == nil {
		t.Fatal("expected the text-only model to fail the probe")
	}
	if !contains(err.Error(), "tool-call") {
		t.Errorf("expected the failure to name the tool-call exchange, got: %v", err)
	}
}

// A provider error during the probe fails it.
func TestProbeCandidate_ProviderErrorFails(t *testing.T) {
	o := probeOrchWithModel(&probeMockModel{behavior: 2})

	if err := o.probeCandidate(context.Background(), o.candidateCopy(0)); err == nil {
		t.Fatal("expected the provider error to fail the probe")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}

	return false
}

// The probe is isolated — it carries no user history, no production
// tools, no Home Assistant data, and has no side effects. Verified through a
// mock model that records everything it is asked.
func TestProbeIsolation(t *testing.T) {
	mock := &probeMockModel{behavior: 0}
	o := probeOrchWithModel(mock)

	if err := o.probeCandidate(context.Background(), o.candidateCopy(0)); err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	gen, toolsSeen, msgsSeen := mock.stats()
	if gen < 2 {
		t.Fatalf("expected a call/result exchange (>=2 model calls), got %d", gen)
	}

	// The FIRST model call carries the entire probe input: the fixed system
	// prompt and the probe user message — nothing else.
	first := msgsSeen[0]
	if len(first) > 2 {
		t.Errorf("probe input has %d messages, want at most system+user (no history)", len(first))
	}
	for _, m := range first {
		switch m.Role {
		case schema.System, schema.User:
		default:
			t.Errorf("probe input carries a %q-role message — no history may reach the probe", m.Role)
		}
	}
	if !contains(first[len(first)-1].Content, probeToolName) {
		t.Errorf("probe user message = %q, want the fixed probe instruction", first[len(first)-1].Content)
	}

	// Only the synthetic no-op tool is attached — never the production tool
	// catalog, never Home Assistant tools.
	for i, tools := range toolsSeen {
		if len(tools) != 1 || tools[0].Name != probeToolName {
			t.Errorf("model call %d saw tools %v, want exactly [%s]", i, toolNames(tools), probeToolName)
		}
	}
}

func toolNames(tools []*schema.ToolInfo) []string {
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}

	return names
}

// The probe covers the agentic (`responses`) construction path —
// the same synthetic exchange over a model.AgenticModel.
func TestProbeCandidate_AgenticPath(t *testing.T) {
	mock := &mockAgenticProbeModel{}
	o := newTestOrch(newFakeClock(),
		mkCand("a::m", "a", "responses", llm.LayoutTailDynamic,
			&fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}}, nil),
	)
	o.cands[0].cand.Model = llm.ResolvedModel{Agentic: mock}

	if err := o.probeCandidate(context.Background(), o.candidateCopy(0)); err != nil {
		t.Fatalf("expected the agentic probe to pass, got: %v", err)
	}
	if mock.calls.Load() < 2 {
		t.Errorf("expected a call/result exchange, got %d model calls", mock.calls.Load())
	}
}

// mockAgenticProbeModel completes the synthetic tool-call exchange over the
// agentic message shape: first call emits a function tool call, second call
// answers in text.
type mockAgenticProbeModel struct {
	calls atomic.Int32
}

var _ model.AgenticModel = (*mockAgenticProbeModel)(nil)

func (m *mockAgenticProbeModel) Generate(_ context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.AgenticMessage, error) {
	if m.calls.Add(1) == 1 {
		return &schema.AgenticMessage{
			Role: schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{
				schema.NewContentBlock(&schema.FunctionToolCall{
					CallID:    "probe_call_1",
					Name:      probeToolName,
					Arguments: testProbeArgs,
				}),
			},
		}, nil
	}

	return &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{
			schema.NewContentBlock(&schema.AssistantGenText{Text: testDoneText}),
		},
	}, nil
}

func (m *mockAgenticProbeModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

// buildProbeRunner selects the right construction path per
// protocol kind — chat for ToolCallingChatModel, agentic for AgenticModel —
// covering every configured wire API's probe shape.
func TestBuildProbeRunner_ProtocolPaths(t *testing.T) {
	chat, err := buildProbeRunner(context.Background(), llm.ResolvedModel{Chat: &probeMockModel{}})
	if err != nil {
		t.Fatalf("chat probe runner: %v", err)
	}
	if _, isAgent := chat.(*agent.Agent); !isAgent {
		t.Errorf("chat model must build the *agent.Agent path, got %T", chat)
	}

	agentic, err := buildProbeRunner(context.Background(), llm.ResolvedModel{Agentic: &mockAgenticProbeModel{}})
	if err != nil {
		t.Fatalf("agentic probe runner: %v", err)
	}
	if _, isAgentic := agentic.(*agent.AgenticAgent); !isAgentic {
		t.Errorf("agentic model must build the *agent.AgenticAgent path, got %T", agentic)
	}
}

// ── Boot: lazy readiness evaluation over a real registry ──────────────────

// chatProbeServer is an httptest OpenAI-compatible server whose catalog
// lists the model and whose chat completions endpoint completes the probe
// exchange (tool call, then final text), answering SSE when the request
// asks to stream (the ADK probe runner streams). authFail makes /models
// return 401.
func chatProbeServer(t *testing.T, authFail bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var chatCalls atomic.Int32

	toolCallChunk := map[string]interface{}{
		"id": testChatcmpl1, testJSONObject: testChunkObj, testJSONCreated: 1, testJSONModelKey: "m",
		testJSONChoices: []map[string]interface{}{{
			testJSONIndex: 0,
			testJSONDelta: map[string]interface{}{
				testJSONRole: testAssistantRole,
				testJSONToolCalls: []map[string]interface{}{{
					testJSONIndex: 0, "id": "call_1", "type": testToolType,
					"function": map[string]interface{}{
						"name":      probeToolName,
						"arguments": testProbeArgs,
					},
				}},
			},
			testJSONFinishReason: nil,
		}},
	}
	toolCallFinish := map[string]interface{}{
		"id": testChatcmpl1, testJSONObject: testChunkObj, testJSONCreated: 1, testJSONModelKey: "m",
		testJSONChoices: []map[string]interface{}{{testJSONIndex: 0, testJSONDelta: map[string]interface{}{}, testJSONFinishReason: testJSONToolCalls}},
	}
	textChunk := map[string]interface{}{
		"id": testChatcmpl2, testJSONObject: testChunkObj, testJSONCreated: 2, testJSONModelKey: "m",
		testJSONChoices: []map[string]interface{}{{
			testJSONIndex:        0,
			testJSONDelta:        map[string]interface{}{testJSONRole: testAssistantRole, "content": testDoneText},
			testJSONFinishReason: nil,
		}},
	}
	textFinish := map[string]interface{}{
		"id": testChatcmpl2, testJSONObject: testChunkObj, testJSONCreated: 2, testJSONModelKey: "m",
		testJSONChoices: []map[string]interface{}{{testJSONIndex: 0, testJSONDelta: map[string]interface{}{}, testJSONFinishReason: "stop"}},
	}

	writeSSE := func(w http.ResponseWriter, events ...map[string]interface{}) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)

			return
		}

		for _, ev := range events {
			data, _ := json.Marshal(ev)
			_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
			flusher.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}

	nonStreaming := func(w http.ResponseWriter, n int32) {
		w.Header().Set("Content-Type", "application/json")

		if n == 1 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": testChatcmpl1, testJSONObject: testCompletionObj, testJSONCreated: 1, testJSONModelKey: "m",
				testJSONChoices: []map[string]interface{}{{
					testJSONIndex: 0,
					"message": map[string]interface{}{
						testJSONRole: testAssistantRole,
						testJSONToolCalls: []map[string]interface{}{{
							"id":   "call_1",
							"type": testToolType,
							"function": map[string]interface{}{
								"name":      probeToolName,
								"arguments": testProbeArgs,
							},
						}},
					},
					testJSONFinishReason: testJSONToolCalls,
				}},
			})

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": testChatcmpl2, testJSONObject: testCompletionObj, testJSONCreated: 2, testJSONModelKey: "m",
			testJSONChoices: []map[string]interface{}{{
				testJSONIndex: 0,
				"message": map[string]interface{}{
					testJSONRole: "assistant",
					"content":    testDoneText,
				},
				testJSONFinishReason: "stop",
			}},
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			if authFail {
				w.WriteHeader(http.StatusUnauthorized)

				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{{"id": "m"}},
			})

			return
		}

		if r.Method == http.MethodPost && r.URL.Path == "/chat/completions" {
			n := chatCalls.Add(1)

			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			streaming := bytes.Contains(body, []byte(`"stream":true`)) ||
				bytes.Contains(body, []byte(`"stream": true`))

			if !streaming {
				nonStreaming(w, n)

				return
			}

			if n == 1 {
				writeSSE(w, toolCallChunk, toolCallFinish)

				return
			}
			writeSSE(w, textChunk, textFinish)

			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	return srv, &chatCalls
}

// bootTestOrch builds a real-registry orchestrator over two opencode
// providers backed by probe servers.
func bootTestOrch(t *testing.T, authFailFirst bool) (*Orchestrator, *atomic.Int32) {
	t.Helper()

	srv0, calls0 := chatProbeServer(t, authFailFirst)
	srv1, _ := chatProbeServer(t, false)

	registry := &config.LLMConfig{
		Default:  testRefP0,
		Failover: []string{testRefP1},
		Providers: map[string]config.LLMProviderConfig{
			"p0": {Provider: config.ProviderOpenCode, API: config.APIChatCompletions, APIKey: "k", BaseURL: srv0.URL},
			"p1": {Provider: config.ProviderOpenCode, API: config.APIChatCompletions, APIKey: "k", BaseURL: srv1.URL},
		},
	}

	cands := make([]Candidate, 0, 2)
	descs, err := llm.CompileCandidates(registry)
	if err != nil {
		t.Fatalf("CompileCandidates: %v", err)
	}
	for _, desc := range descs {
		resolved, buildErr := llm.BuildForReference(context.Background(), registry, desc.Ref)
		if buildErr != nil {
			t.Fatalf("BuildForReference(%s): %v", desc.Ref, buildErr)
		}

		cand := Candidate{
			CandidateDescriptor: desc,
			Runner:              &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}},
			Aux:                 resolved.Chat,
			Model:               resolved,
		}
		cands = append(cands, cand)
	}

	o := New(Config{
		Candidates:     cands,
		Registry:       registry,
		Recovery:       testRecovery(),
		AttemptTimeout: 2 * time.Second,
	}, zerolog.Nop())

	return o, calls0
}

// The default passes readiness and later candidates
// are left unproven (never probed at boot).
func TestBoot_LazyUntilFirstReady(t *testing.T) {
	o, firstChatCalls := bootTestOrch(t, false)

	o.Boot(context.Background())

	snap := o.Snapshot()
	if snap.ActiveRef != testRefP0 {
		t.Errorf("active = %q, want p0::m", snap.ActiveRef)
	}
	if !snap.Candidates[0].Verified {
		t.Error("the boot-probed candidate must be verified")
	}
	if !snap.Candidates[1].Unproven || snap.Candidates[1].Verified {
		t.Errorf("later candidate must stay unproven, got %+v", snap.Candidates[1])
	}
	if got := firstChatCalls.Load(); got < 2 {
		t.Errorf("first candidate's probe exchange ran %d model calls, want >= 2", got)
	}
}

// The default fails readiness (auth) and the first
// fallback is ready → the daemon starts with the fallback active.
func TestBoot_DefaultFailsFallbackReady(t *testing.T) {
	o, firstChatCalls := bootTestOrch(t, true)

	o.Boot(context.Background())

	snap := o.Snapshot()
	if snap.ActiveRef != testRefP1 {
		t.Errorf("active = %q, want p1::m", snap.ActiveRef)
	}
	if !snap.Candidates[0].Cooling {
		t.Error("the failed default must be cooling (unhealthy)")
	}
	if got := firstChatCalls.Load(); got != 0 {
		t.Errorf("an auth-failed preflight must skip the tool probe, got %d chat calls", got)
	}
}

// Boot logging sanity: Boot on an all-failing chain does
// not error (the daemon starts unavailable) — covered fully in
// TestBoot_NoCandidateReadyStartsUnavailable in orchestrator_test.go.
func TestBoot_AllFailNoError(t *testing.T) {
	srv0, _ := chatProbeServer(t, true)
	srv1, _ := chatProbeServer(t, true)

	registry := &config.LLMConfig{
		Default:  testRefP0,
		Failover: []string{testRefP1},
		Providers: map[string]config.LLMProviderConfig{
			"p0": {Provider: config.ProviderOpenCode, API: config.APIChatCompletions, APIKey: "k", BaseURL: srv0.URL},
			"p1": {Provider: config.ProviderOpenCode, API: config.APIChatCompletions, APIKey: "k", BaseURL: srv1.URL},
		},
	}

	descs, err := llm.CompileCandidates(registry)
	if err != nil {
		t.Fatalf("CompileCandidates: %v", err)
	}

	cands := make([]Candidate, 0, len(descs))
	for _, desc := range descs {
		resolved, buildErr := llm.BuildForReference(context.Background(), registry, desc.Ref)
		if buildErr != nil {
			t.Fatalf("BuildForReference: %v", buildErr)
		}
		cands = append(cands, Candidate{
			CandidateDescriptor: desc,
			Runner:              &fakeRunner{steps: []runnerStep{{outcome: okOutcome("x")}}},
			Model:               resolved,
		})
	}

	o := New(Config{Candidates: cands, Registry: registry, Recovery: testRecovery()}, zerolog.Nop())
	o.Boot(context.Background()) // must not panic or error

	if o.Snapshot().State != StateUnavailable {
		t.Errorf("state = %q, want unavailable", o.Snapshot().State)
	}
	if errors.Is(nil, llm.ErrChainExhausted) {
		t.Fatal("unreachable sanity check")
	}
}

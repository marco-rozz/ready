//nolint:goconst
package twin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/marco-rozz/ready/pkg/hatwin"
	"github.com/rs/zerolog"
)

// --- mock HA WS server (local copy of the pkg-level test server) ---

type mockHA struct {
	*httptest.Server
	connCh chan *websocket.Conn
	mu     sync.Mutex
	frames []map[string]any
	ctxSeq int
}

func newMockHA(t *testing.T) *mockHA {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	m := &mockHA{connCh: make(chan *websocket.Conn, 8)}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		m.connCh <- conn
		m.serve(t, conn)
	}))

	return m
}

// serve performs the HA handshake + bootstrap responses, then answers every
// call_service with a success result carrying a fresh context id.
func (m *mockHA) serve(_ *testing.T, conn *websocket.Conn) {
	send := func(v any) {
		_ = conn.WriteJSON(v)
	}

	send(map[string]any{"type": "auth_required", "ha_version": "2025.1"})

	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		return
	}
	send(map[string]any{"type": "auth_ok"})

	// Consume the 6 bootstrap commands.
	var ids []float64
	for range 6 {
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		idf, ok := msg["id"].(float64)
		if !ok {
			return
		}
		ids = append(ids, idf)
	}

	results := []any{
		map[string]any{"type": "result", "id": ids[0], "success": true, "result": []any{
			map[string]any{"area_id": "cucina", "name": "Cucina"},
		}},
		map[string]any{"type": "result", "id": ids[1], "success": true, "result": []any{}},
		map[string]any{"type": "result", "id": ids[2], "success": true, "result": map[string]any{"entities": []any{
			map[string]any{"ei": "light.cucina", "ai": "cucina"},
		}}},
		map[string]any{"type": "result", "id": ids[4], "success": true, "result": nil},
		map[string]any{"type": "result", "id": ids[5], "success": true, "result": nil},
		map[string]any{"type": "result", "id": ids[3], "success": true, "result": []any{
			map[string]any{"entity_id": "light.cucina", "state": "off", "attributes": map[string]any{"friendly_name": "Luce Cucina"}},
			map[string]any{"entity_id": "sensor.temp", "state": "21.5", "attributes": map[string]any{"device_class": "temperature", "unit_of_measurement": "°C"}},
			map[string]any{"entity_id": "cover.tenda", "state": "closed", "attributes": map[string]any{"friendly_name": "Tenda"}},
		}},
	}
	for _, r := range results {
		send(r)
	}

	// Command loop: answer every call_service; record frames.
	for {
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		if msg["type"] != "call_service" {
			continue
		}
		m.mu.Lock()
		m.frames = append(m.frames, msg)
		m.ctxSeq++
		ctxID := fmt.Sprintf("ctx-%d", m.ctxSeq)
		m.mu.Unlock()
		send(map[string]any{
			"type": "result", "id": msg["id"], "success": true,
			"result": map[string]any{"context": map[string]any{"id": ctxID}},
		})
	}
}

func (m *mockHA) sentFrames() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]map[string]any(nil), m.frames...)
}

func (m *mockHA) wsURL() string { return "ws" + strings.TrimPrefix(m.URL, "http") }

// newCommandTwin boots a ready hatwin twin against the mock HA with the
// command domain set active.
func newCommandTwin(t *testing.T, m *mockHA) (*hatwin.Twin, func()) {
	t.Helper()

	tw, err := hatwin.New(hatwin.Options{
		URL:                  m.wsURL(),
		Token:                "tok",
		PingInterval:         time.Hour,
		ActiveCommandDomains: hatwin.DefaultCommandDomains(),
	})
	if err != nil {
		t.Fatalf("hatwin.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	tw.Start(ctx)
	if err := tw.WaitReady(ctx); err != nil {
		cancel()
		t.Fatalf("WaitReady: %v", err)
	}

	return tw, func() {
		cancel()
		tw.Close()
	}
}

// --- ha_command tests ---

func TestCommandTool_OutcomeMapping(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: 2 * time.Second, MaxTargets: 20}, zerolog.Nop())

	out, err := tool.InvokableRun(context.Background(), `{"action":"on","domain":"light","entity_ids":["light.cucina"]}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	var resp struct {
		CallID   string `json:"call_id"`
		Sent     bool   `json:"sent"`
		Outcomes []struct {
			EntityID string `json:"entity_id"`
			Outcome  string `json:"outcome"`
			Note     string `json:"note"`
		} `json:"outcomes"`
		StateCheck string `json:"state_check"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if !resp.Sent || resp.CallID == "" {
		t.Fatalf("call_id/sent missing: %s", out)
	}
	if len(resp.Outcomes) != 1 || resp.Outcomes[0].EntityID != "light.cucina" {
		t.Fatalf("outcomes: %+v", resp.Outcomes)
	}
	// No event arrives in this mock: already-on? Pre-state was off, action
	// on, no evidence → unchanged with the [UNVERIFIED] marker.
	if resp.Outcomes[0].Outcome != "unchanged" {
		t.Fatalf("outcome = %q, want unchanged", resp.Outcomes[0].Outcome)
	}
	if !strings.Contains(resp.Outcomes[0].Note, "[UNVERIFIED]") {
		t.Fatalf("note = %q, want [UNVERIFIED] marker", resp.Outcomes[0].Note)
	}
	if resp.StateCheck == "" {
		t.Fatal("unchanged outcome must carry the no-blind-retry state_check guidance")
	}

	// The sent call is verifiable: mode=verify with the returned call_id.
	vout, verr := tool.InvokableRun(context.Background(), fmt.Sprintf(`{"action":"on","mode":"verify","call_id":%q}`, resp.CallID))
	if verr != nil {
		t.Fatalf("verify: %v", verr)
	}
	if !strings.Contains(vout, resp.CallID) {
		t.Fatalf("verify response missing call_id: %s", vout)
	}
}

func TestCommandTool_BarrierRefusalGuidance(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()
	tw.SetActiveCommandDomains(nil) // restore defaults (defensive)

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: time.Second, MaxTargets: 20}, zerolog.Nop())

	// Kill the session handle (simulates a reconnect gap) → transient
	// refusal with generic fallback guidance (never naming MCP servers).
	tw.StopSessionsForTest()
	_, err := tool.InvokableRun(context.Background(), `{"action":"on","domain":"light","entity_ids":["light.cucina"]}`)
	if err == nil {
		t.Fatal("expected a barrier refusal")
	}
	if !strings.Contains(err.Error(), "equivalent tool") && !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v, want generic fallback guidance", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "mcp") {
		t.Fatalf("fallback must not name MCP: %v", err)
	}
}

func TestCommandTool_FallbackTexts(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: time.Second, MaxTargets: 20}, zerolog.Nop())

	// Out of vocabulary: lock is never available.
	_, err := tool.InvokableRun(context.Background(), `{"action":"open","domain":"lock"}`)
	if err == nil {
		t.Fatal("expected refusal for lock")
	}
	if !strings.Contains(err.Error(), "equivalent tool") {
		t.Fatalf("err = %v, want fallback guidance", err)
	}

	// No match: guided error, nothing sent.
	_, err = tool.InvokableRun(context.Background(), `{"action":"on","domain":"light","area":"bagno"}`)
	if err == nil {
		t.Fatal("expected no-match refusal")
	}
	if !strings.Contains(err.Error(), "bagno") {
		t.Fatalf("err = %v, want selector echoed", err)
	}

	// Nothing was sent on any refusal.
	if frames := m.sentFrames(); len(frames) != 0 {
		t.Fatalf("frames sent on refusals: %v", frames)
	}
}

func TestCommandTool_SendModeReturnsImmediately(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: 5 * time.Second, MaxTargets: 20}, zerolog.Nop())

	out, err := tool.InvokableRun(context.Background(), `{"action":"on","domain":"light","entity_ids":["light.cucina"],"mode":"send"}`)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(out, "\"call_id\"") {
		t.Fatalf("send response missing call_id: %s", out)
	}

	// Deadline-bound verify: a remaining budget shorter than the 5s margin
	// degrades to send semantics (window 0) instead of erroring.
	deadCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
	defer cancel()
	out, err = tool.InvokableRun(deadCtx, `{"action":"on","domain":"light","entity_ids":["light.cucina"]}`)
	if err != nil {
		t.Fatalf("expired-deadline sync must degrade, got err: %v", err)
	}
	if !strings.Contains(out, "\"call_id\"") {
		t.Fatalf("response missing call_id: %s", out)
	}
}

func TestCommandTool_VerifyUnknownCallID(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: time.Second, MaxTargets: 20}, zerolog.Nop())

	_, err := tool.InvokableRun(context.Background(), `{"action":"on","mode":"verify","call_id":"cmd_niente"}`)
	if err == nil {
		t.Fatal("expected unknown-call_id refusal")
	}
	if !strings.Contains(err.Error(), "ha_state_query") {
		t.Fatalf("err = %v, want guidance to the read tool", err)
	}
}

func TestCommandTool_DescriptionReflectsActiveDomains(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()
	tw.SetActiveCommandDomains([]string{"light", "cover"})

	tool := NewCommandTool(tw, commandToolConfig{VerifyTimeout: time.Second, MaxTargets: 20}, zerolog.Nop())
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}

	if strings.Contains(info.Desc, "switch:") || strings.Contains(info.Desc, "siren") {
		t.Fatalf("description must list only active domains: %s", info.Desc)
	}
	if !strings.Contains(info.Desc, "light: ") || !strings.Contains(info.Desc, "cover: ") {
		t.Fatalf("description missing active domains: %s", info.Desc)
	}
}

// --- ha_state_query extension tests ---

func TestQueryTool_LimitAndTruncationCount(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewQueryTool(tw, time.Minute, 5*time.Minute)

	out, err := tool.InvokableRun(context.Background(), `{"domain":"light","limit":1}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	var resp struct {
		Results []hatwin.EntitySnapshot `json:"results"`
		More    int                     `json:"more"`
	}
	if uerr := json.Unmarshal([]byte(out), &resp); uerr != nil {
		t.Fatalf("unmarshal: %v (%s)", uerr, out)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(resp.Results))
	}
	// The mock home has exactly one light; more must be 0 — flip the fixture
	// by querying the unfiltered set instead.
	out, err = tool.InvokableRun(context.Background(), `{"limit":2}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	resp2 := struct {
		Results []hatwin.EntitySnapshot `json:"results"`
		More    int                     `json:"more"`
	}{}
	if err := json.Unmarshal([]byte(out), &resp2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp2.Results) != 2 || resp2.More != 1 {
		t.Fatalf("results=%d more=%d, want 2/1 (3 entities in mock home)", len(resp2.Results), resp2.More)
	}
}

func TestQueryTool_GroupByArea(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewQueryTool(tw, time.Minute, 5*time.Minute)

	out, err := tool.InvokableRun(context.Background(), `{"group_by":"area"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	var resp struct {
		Groups map[string][]hatwin.EntitySnapshot `json:"groups"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	if len(resp.Groups["Cucina"]) != 1 { // light.cucina (registry area)
		t.Fatalf("Cucina bucket: %v", resp.Groups["Cucina"])
	}
	if len(resp.Groups["unassigned"]) != 2 { // sensor.temp, cover.tenda
		t.Fatalf("unassigned bucket: %v", resp.Groups["unassigned"])
	}
}

func TestQueryTool_StateFilterShapes(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	tool := NewQueryTool(tw, time.Minute, 5*time.Minute)

	// Semantic off on covers resolves to closed.
	out, err := tool.InvokableRun(context.Background(), `{"domain":"cover","state":"off"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "cover.tenda") {
		t.Fatalf("semantic off filter failed: %s", out)
	}

	// Numeric comparison on sensor state.
	out, err = tool.InvokableRun(context.Background(), `{"domain":"sensor","state":{">":25}}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if strings.Contains(out, "sensor.temp") {
		t.Fatalf("state > 25 must exclude 21.5: %s", out)
	}

	out, err = tool.InvokableRun(context.Background(), `{"domain":"sensor","state":{"<":25}}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "sensor.temp") {
		t.Fatalf("state < 25 must include sensor.temp: %s", out)
	}

	// Attribute equality without verbose.
	out, err = tool.InvokableRun(context.Background(), `{"attributes":{"hvac_action":"heating"}}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if strings.Contains(out, "\"results\":[{") {
		t.Fatalf("attribute filter must exclude non-matching entities: %s", out)
	}

	// Name substring, accent-insensitive.
	out, err = tool.InvokableRun(context.Background(), `{"name":"tend"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, "cover.tenda") {
		t.Fatalf("name filter failed: %s", out)
	}
}

func TestEntityIDList_AcceptsStringForms(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		`{"entity_ids":["light.a","light.b"]}`:        {"light.a", "light.b"},
		`{"entity_ids":"[\"scene.soggiorno_lime\"]"}`: {"scene.soggiorno_lime"},
		`{"entity_ids":"light.a"}`:                    {"light.a"},
		`{"entity_ids":""}`:                           nil,
		`{}`:                                          nil,
	}
	for in, want := range cases {
		var args commandArgs
		if err := json.Unmarshal([]byte(in), &args); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if fmt.Sprint([]string(args.EntityIDs)) != fmt.Sprint(want) {
			t.Errorf("%s: got %q, want %q", in, args.EntityIDs, want)
		}
	}

	var args commandArgs
	if err := json.Unmarshal([]byte(`{"entity_ids":"[not json"}`), &args); err == nil {
		t.Error("malformed JSON-encoded array: want error")
	}
}

func TestCommandTool_EntityIDsDeclaredAsArray(t *testing.T) {
	t.Parallel()

	m := newMockHA(t)
	defer m.Close()
	tw, stop := newCommandTwin(t, m)
	defer stop()

	info, err := NewCommandTool(tw, commandToolConfig{}, zerolog.Nop()).Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	js, err := info.ToJSONSchema()
	if err != nil {
		t.Fatalf("ToJSONSchema: %v", err)
	}
	prop, ok := js.Properties.Get("entity_ids")
	if !ok || prop.Type != "array" || prop.Items == nil || prop.Items.Type != "string" {
		t.Fatalf("entity_ids schema = %+v, want array of string", prop)
	}
}

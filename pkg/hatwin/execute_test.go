//nolint:goconst
package hatwin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- command orchestration fixtures ---

// homeFixture is a canned house for command/filter tests.
type homeFixture struct {
	areas    []any
	devices  []any
	entities []any
	states   []any
}

func defaultHome() homeFixture {
	return homeFixture{
		areas: []any{
			map[string]any{"area_id": "cucina", "name": "Cucina"},
			map[string]any{"area_id": "salotto", "name": "Salotto"},
		},
		devices: []any{
			map[string]any{"id": "dev1", "name": "Dev1", "area_id": "cucina"},
		},
		entities: []any{
			map[string]any{"ei": "light.cucina", "di": "dev1"},
			map[string]any{"ei": "light.salotto"},
			map[string]any{"ei": "switch.cucina", "di": "dev1", "disabled_by": "user"},
			map[string]any{"ei": "cover.tenda"},
			map[string]any{"ei": "sensor.temp", "di": "dev1"},
			map[string]any{"ei": "light.spenta", "name": "Luce Spenta"},
			map[string]any{"ei": "light.morta", "name": "Luce Morta"},
		},
		states: []any{
			stateEntry("light.cucina", "off", map[string]any{"friendly_name": "Luce Cucina"}),
			stateEntry("light.salotto", "on", map[string]any{"friendly_name": "Luce Salotto"}),
			stateEntry("switch.cucina", "off", nil),
			stateEntry("switch.presa", "off", map[string]any{"friendly_name": "Presa Cucina"}),
			stateEntry("cover.tenda", "closed", map[string]any{"friendly_name": "Tenda Salotto"}),
			stateEntry("sensor.temp", "21.5", map[string]any{"device_class": "temperature", "unit_of_measurement": "°C"}),
			stateEntry("light.spenta", "off", map[string]any{"friendly_name": "Luce Spenta"}),
			stateEntry("light.morta", "unavailable", nil),
			stateEntry("light.gruppo", "off", map[string]any{"entity_id": []any{"light.cucina", "light.salotto"}, "friendly_name": "Gruppo Luci"}),
		},
	}
}

func stateEntry(entityID, state string, attrs map[string]any) map[string]any {
	obj := map[string]any{
		"entity_id":    entityID,
		"state":        state,
		"last_changed": "2025-01-01T00:00:00Z",
		"attributes":   map[string]any{},
	}
	stateAttrs, _ := obj["attributes"].(map[string]any)
	for k, v := range attrs {
		stateAttrs[k] = v
	}

	return obj
}

// startCommandTwin boots a ready twin against a mock HA whose server, after
// the bootstrap, answers every call_service with a success result carrying a
// fresh context id and records the received frames. Tests can also read
// frames themselves via the returned conn.
func startCommandTwin(t *testing.T, home homeFixture, opts Options) (*Twin, context.CancelFunc, *serverConn) {
	t.Helper()

	ms := newMockServer(t)
	defer ms.Close()

	srv := &serverConn{}
	ready := make(chan *websocketConn, 1)
	go func() {
		conn := <-ms.connCh
		serveBootstrap(t, conn, home.areas, home.devices, home.entities, home.states, nil)
		srv.conn = conn
		ready <- &websocketConn{conn}
	}()

	if opts.URL == "" {
		opts.URL = ms.wsURL()
	}
	if opts.Token == "" {
		opts.Token = "test-token"
	}
	if opts.ActiveCommandDomains == nil {
		opts.ActiveCommandDomains = DefaultCommandDomains()
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = time.Hour // keep pings out of the frame stream
	}

	twin, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	twin.Start(ctx)
	if err := twin.WaitReady(ctx); err != nil {
		cancel()
		t.Fatalf("WaitReady: %v", err)
	}
	srv.ws = <-ready

	return twin, cancel, srv
}

// serverConn wraps the server side of the mock WS for command tests.
type serverConn struct {
	ws     *websocketConn
	conn   any
	frames []map[string]any
}

// answerCallService reads one call_service frame and answers it with a
// success result carrying a fresh context id.
func (s *serverConn) answerCallService(t *testing.T, ctxID string) map[string]any {
	t.Helper()
	msg := s.ws.readCmd(t)
	s.frames = append(s.frames, msg)
	if msg["type"] != "call_service" {
		t.Fatalf("expected call_service frame, got %v", msg)
	}
	s.ws.writeCmd(t, map[string]any{
		"type": "result", "id": msg["id"], "success": true,
		"result": map[string]any{"context": map[string]any{"id": ctxID}},
	})

	return msg
}

// sendStateChanged emits a state_changed event for an entity.
func (s *serverConn) sendStateChanged(t *testing.T, entityID, state, ctxID string) {
	t.Helper()
	s.ws.writeCmd(t, map[string]any{
		"type": "event",
		"id":   bootSubscribeAllID,
		"event": map[string]any{
			"event_type": "state_changed",
			"data": map[string]any{
				"entity_id": entityID,
				"new_state": map[string]any{
					"state":      state,
					"context":    map[string]any{"id": ctxID},
					"attributes": map[string]any{},
				},
			},
		},
	})
}

func outcomeOf(rep CommandReport, entityID string) EntityOutcome {
	for _, o := range rep.Outcomes {
		if o.EntityID == entityID {
			return o
		}
	}

	return EntityOutcome{}
}

// --- ExecuteCommand outcome tests (task 2.5) ---

func TestExecuteCommand_AppliedByContextID(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	errCh := make(chan error, 1)
	go func() {
		rep, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", Area: "cucina",
			VerifyWindow: 2 * time.Second,
		})
		done <- rep
		errCh <- err
	}()

	msg := srv.answerCallService(t, "ctx-own-1")
	// Event BEFORE the result: must be buffered until the context id is
	// known, then attributed (event-before-result race, D3).
	srv.sendStateChanged(t, "light.cucina", "on", "ctx-own-1")
	_ = msg

	select {
	case rep := <-done:
		if err := <-errCh; err != nil {
			t.Fatalf("ExecuteCommand: %v", err)
		}
		if !rep.Sent {
			t.Fatal("Sent = false, want true")
		}
		o := outcomeOf(rep, "light.cucina")
		if o.Outcome != OutcomeApplied || o.Evidence != "direct" {
			t.Fatalf("outcome = %+v, want applied/direct", o)
		}
		if o.Before != "off" || o.After != "on" {
			t.Fatalf("before/after = %q/%q, want off/on", o.Before, o.After)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteCommand did not return")
	}
}

func TestExecuteCommand_ForeignContextIndirectFallback(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "off", Domain: "light", EntityIDs: []string{"light.salotto"},
			VerifyWindow: time.Second,
		})
		done <- rep
	}()

	_ = srv.answerCallService(t, "ctx-own")
	// A local integration re-polls under a FOREIGN context but the state
	// reaches the expected value: indirect applied.
	srv.sendStateChanged(t, "light.salotto", "off", "ctx-foreign")

	rep := <-done
	o := outcomeOf(rep, "light.salotto")
	if o.Outcome != OutcomeApplied || o.Evidence != "indirect" {
		t.Fatalf("outcome = %+v, want applied/indirect", o)
	}
}

func TestExecuteCommand_ForeignContextWrongStateIsNotEvidence(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", EntityIDs: []string{"light.spenta"},
			VerifyWindow: 300 * time.Millisecond,
		})
		done <- rep
	}()

	_ = srv.answerCallService(t, "ctx-own")
	// An automation changes the entity under a foreign context to a state
	// other than the expected one: not evidence.
	srv.sendStateChanged(t, "light.spenta", "off", "ctx-foreign")

	rep := <-done
	o := outcomeOf(rep, "light.spenta")
	if o.Outcome != OutcomeUnchanged {
		t.Fatalf("outcome = %q, want unchanged", o.Outcome)
	}
}

func TestExecuteCommand_AlreadyInState(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", EntityIDs: []string{"light.salotto"},
			VerifyWindow: 300 * time.Millisecond,
		})
		done <- rep
	}()

	_ = srv.answerCallService(t, "ctx-own")

	rep := <-done
	o := outcomeOf(rep, "light.salotto")
	if o.Outcome != OutcomeAlready {
		t.Fatalf("outcome = %q, want already (pre-state on, no event)", o.Outcome)
	}
}

func TestExecuteCommand_PendingTransitional(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "open", Domain: "cover", EntityIDs: []string{"cover.tenda"},
			VerifyWindow: 300 * time.Millisecond,
		})
		done <- rep
	}()

	_ = srv.answerCallService(t, "ctx-own")
	srv.sendStateChanged(t, "cover.tenda", "opening", "ctx-own")

	rep := <-done
	o := outcomeOf(rep, "cover.tenda")
	if o.Outcome != OutcomePending {
		t.Fatalf("outcome = %q, want pending (transitional at window end)", o.Outcome)
	}
}

func TestExecuteCommand_UnavailableTargetNotSent(t *testing.T) {
	t.Parallel()

	twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	errCh := make(chan error, 1)
	go func() {
		rep, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", EntityIDs: []string{"light.morta"},
			VerifyWindow: 300 * time.Millisecond,
		})
		done <- rep
		errCh <- err
	}()

	rep := <-done
	if err := <-errCh; err != nil {
		t.Fatalf("ExecuteCommand: %v", err)
	}
	o := outcomeOf(rep, "light.morta")
	if o.Outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", o.Outcome)
	}
	if rep.Sent {
		t.Fatal("Sent = true, want false: unavailable targets are not sent")
	}
}

func TestExecuteCommand_NoBlindRetryCarriesCallID(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		// Device dead: HA accepts, no evidence ever arrives.
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", EntityIDs: []string{"light.spenta"},
			VerifyWindow: 200 * time.Millisecond,
		})
		done <- rep
	}()

	_ = srv.answerCallService(t, "ctx-own")

	rep := <-done
	if rep.CallID == "" {
		t.Fatal("empty call_id after send")
	}
	o := outcomeOf(rep, "light.spenta")
	if o.Outcome != OutcomeUnchanged {
		t.Fatalf("outcome = %q, want unchanged", o.Outcome)
	}

	// The call stays verifiable via VerifyCommand.
	vrep, err := twin.VerifyCommand(rep.CallID)
	if err != nil {
		t.Fatalf("VerifyCommand: %v", err)
	}
	if outcomeOf(vrep, "light.spenta").Outcome != OutcomeUnchanged {
		t.Fatalf("verify outcome = %q, want unchanged", outcomeOf(vrep, "light.spenta").Outcome)
	}
}

func TestExecuteCommand_MultiDomainSplitSingleCallID(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action:       "on",
			EntityIDs:    []string{"light.cucina", "switch.cucina"},
			VerifyWindow: time.Second,
		})
		done <- rep
	}()

	srv.answerCallService(t, "ctx-1")
	srv.answerCallService(t, "ctx-2")

	rep := <-done
	if len(rep.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (one per domain)", len(rep.Calls))
	}
	domains := map[string]bool{}
	for _, c := range rep.Calls {
		domains[c.Domain] = true
	}
	if !domains["light"] || !domains["switch"] {
		t.Fatalf("expected one call per domain (light, switch), got %v", domains)
	}
}

// --- selector rules (D6) ---

func TestExecuteCommand_SelectorRules(t *testing.T) {
	t.Parallel()

	t.Run("selector without domain", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{Action: "on", Area: "Cucina"})
		if !strings.Contains(err.Error(), "domain") {
			t.Fatalf("err = %v, want domain-required guidance", err)
		}
	})

	t.Run("zero matches", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{Action: "on", Domain: "light", Area: "bagno"})
		if !errors.Is(err, ErrNoMatch) {
			t.Fatalf("err = %v, want ErrNoMatch", err)
		}
	})

	t.Run("unknown explicit id", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", EntityIDs: []string{"light.cucina", "light.inventata"},
		})
		if !errors.Is(err, ErrUnknownEntity) {
			t.Fatalf("err = %v, want ErrUnknownEntity", err)
		}
		if !strings.Contains(err.Error(), "light.inventata") {
			t.Fatalf("err = %v, must name the unknown id", err)
		}
	})

	t.Run("cap exceeded", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", MaxTargets: 2,
		})
		if !errors.Is(err, ErrOverCap) {
			t.Fatalf("err = %v, want ErrOverCap", err)
		}
		if !strings.Contains(err.Error(), "3") { // light.cucina, light.salotto, light.spenta (group de-duplicated)
			t.Fatalf("err = %v, must state the match count", err)
		}
	})

	t.Run("multi-target toggle", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "toggle", Domain: "light", MaxTargets: 20,
		})
		if !errors.Is(err, ErrMultiTargetToggle) {
			t.Fatalf("err = %v, want ErrMultiTargetToggle", err)
		}
	})

	t.Run("group dropped when members also selected", func(t *testing.T) {
		twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		done := make(chan CommandReport, 1)
		go func() {
			rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
				Action: "on", Domain: "light", MaxTargets: 20,
				VerifyWindow: 300 * time.Millisecond,
			})
			done <- rep
		}()
		srv.answerCallService(t, "ctx-1")

		rep := <-done
		ids := map[string]bool{}
		for _, o := range rep.Outcomes {
			if o.Outcome != OutcomeUnavailable {
				ids[o.EntityID] = true
			}
		}
		if ids["light.gruppo"] {
			t.Fatal("group light.gruppo should be de-duplicated when its members are selected")
		}
		if !ids["light.cucina"] || !ids["light.salotto"] {
			t.Fatalf("members missing from selection: %v", ids)
		}
	})

	t.Run("disabled entity excluded from selector", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		done := make(chan CommandReport, 1)
		go func() {
			rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
				Action: "on", Domain: "switch", MaxTargets: 20,
				VerifyWindow: 300 * time.Millisecond,
			})
			done <- rep
		}()
		// No call_service expected: the only switch is registry-disabled.
		rep := <-done
		if rep.Sent {
			t.Fatal("Sent = true, want false: selector matches exclude registry-disabled entities")
		}
	})

	t.Run("unavailable reported by selector", func(t *testing.T) {
		twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		// When every match is unavailable nothing is sent; the refusal names
		// the unavailable match (explicit ids of unavailable entities keep
		// the outcome-based reporting path — see UnavailableTargetNotSent).
		_, err := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "on", Domain: "light", Name: "morta", MaxTargets: 20,
		})
		if err == nil {
			t.Fatal("expected a refusal for an all-unavailable selection")
		}
		if !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("err = %v, must name the unavailable condition", err)
		}
	})

	t.Run("explicit unavailable id is reported", func(t *testing.T) {
		twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
		defer cancel()
		defer twin.Close()

		repCh := make(chan CommandReport, 1)
		errCh := make(chan error, 1)
		go func() {
			rep, err := twin.ExecuteCommand(context.Background(), CommandSpec{
				Action: "on", EntityIDs: []string{"light.morta", "light.cucina"},
			})
			repCh <- rep
			errCh <- err
		}()

		_ = srv.answerCallService(t, "ctx-1")

		rep := <-repCh
		if err := <-errCh; err != nil {
			t.Fatalf("mixed batch: %v", err)
		}
		if outcomeOf(rep, "light.morta").Outcome != OutcomeUnavailable {
			t.Fatal("unavailable target not reported")
		}
	})
}

// --- vocabulary (D5) ---

func TestVocabularyRejections(t *testing.T) {
	t.Parallel()

	twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	cases := []struct {
		name string
		spec CommandSpec
		sent error
	}{
		{"lock is never in the table", CommandSpec{Action: "on", Domain: "lock"}, ErrOutOfVocabulary},
		{"alarm panel excluded", CommandSpec{Action: "on", Domain: "alarm_control_panel"}, ErrOutOfVocabulary},
		{"siren inactive by default", CommandSpec{Action: "on", Domain: "siren"}, ErrOutOfVocabulary},
		{"invalid set param on switch", CommandSpec{Action: "set", Domain: "switch", EntityIDs: []string{"switch.presa"}, Params: map[string]any{"brightness": 100.0}}, ErrInvalidParams},
		{"out of range brightness", CommandSpec{Action: "set", Domain: "light", EntityIDs: []string{"light.cucina"}, Params: map[string]any{"brightness": 999.0}}, ErrInvalidParams},
	}

	for _, tc := range cases {
		_, err := twin.ExecuteCommand(context.Background(), tc.spec)
		if !errors.Is(err, tc.sent) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.sent)
		}
	}

	// Guided text names the valid parameters.
	_, err := twin.ExecuteCommand(context.Background(), CommandSpec{
		Action: "set", Domain: "switch", EntityIDs: []string{"switch.presa"},
		Params: map[string]any{"brightness": 100.0},
	})
	if err != nil && !strings.Contains(err.Error(), "valid parameters") {
		t.Errorf("guided text missing valid-parameter list: %v", err)
	}
}

func TestExecuteCommand_InactiveDomainRefused(t *testing.T) {
	t.Parallel()

	twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{ActiveCommandDomains: []string{"light"}})
	defer cancel()
	defer twin.Close()

	_, err := twin.ExecuteCommand(context.Background(), CommandSpec{Action: "open", Domain: "cover", EntityIDs: []string{"cover.tenda"}})
	if !errors.Is(err, ErrOutOfVocabulary) {
		t.Fatalf("err = %v, want ErrOutOfVocabulary for inactive domain", err)
	}
}

func TestExecuteCommand_ParametricSet(t *testing.T) {
	t.Parallel()

	twin, cancel, srv := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	done := make(chan CommandReport, 1)
	go func() {
		rep, _ := twin.ExecuteCommand(context.Background(), CommandSpec{
			Action: "set", Domain: "light", EntityIDs: []string{"light.cucina"},
			Params:       map[string]any{"brightness": 128.0},
			VerifyWindow: 300 * time.Millisecond,
		})
		done <- rep
	}()

	msg := srv.answerCallService(t, "ctx-1")
	if msg["service"] != "turn_on" {
		t.Fatalf("service = %v, want turn_on", msg["service"])
	}
	data, _ := msg["service_data"].(map[string]any)
	if data["brightness"] != 128.0 {
		t.Fatalf("brightness = %v, want 128", data["brightness"])
	}

	rep := <-done
	if !rep.Sent {
		t.Fatal("Sent = false, want true")
	}
}

// --- evidence store bounds ---

func TestEvidenceStoreTTLAndCapEviction(t *testing.T) {
	t.Parallel()

	twin, cancel, _ := startCommandTwin(t, defaultHome(), Options{})
	defer cancel()
	defer twin.Close()

	// Fill the store past the cap with fresh entries.
	twin.cmdMu.Lock()
	for i := 0; i < evidenceMaxEntries+5; i++ {
		e := &evidenceEntry{callID: newCallID(), armedAt: time.Now(), ctxIDs: map[string]bool{}, buffered: map[string][]map[string]any{}, done: make(chan struct{})}
		twin.evidenceStore[e.callID] = e
		twin.evidenceOrder = append(twin.evidenceOrder, e.callID)
	}
	// One expired entry.
	old := &evidenceEntry{callID: "cmd_expired", armedAt: time.Now().Add(-2 * evidenceTTL), ctxIDs: map[string]bool{}, buffered: map[string][]map[string]any{}, done: make(chan struct{})}
	twin.evidenceStore[old.callID] = old
	twin.evidenceOrder = append(twin.evidenceOrder, old.callID)
	twin.cmdMu.Unlock()

	twin.armEvidence(nil, nil, map[string]string{}) // triggers eviction

	twin.cmdMu.Lock()
	defer twin.cmdMu.Unlock()
	if _, ok := twin.evidenceStore["cmd_expired"]; ok {
		t.Fatal("expired entry not evicted")
	}
	if len(twin.evidenceStore) > evidenceMaxEntries {
		t.Fatalf("store size = %d, cap is %d", len(twin.evidenceStore), evidenceMaxEntries)
	}
}

// --- semantic table unit checks (D8) ---

func TestSemanticTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		domain, state, semantic string
		want                    bool
	}{
		{"light", "on", "on", true},
		{"light", "off", "on", false},
		{"cover", "closed", "off", true},
		{"cover", "closed", "on", false},
		{"cover", "opening", "on", false},
		{"cover", "opening", "off", false},
		{"climate", "heat", "on", true},
		{"climate", "off", "on", false},
		{"climate", "off", "off", true},
		{"media_player", "playing", "on", true},
		{"media_player", "standby", "on", false},
		{"media_player", "standby", "off", true},
		{"sensor", "42", "on", false}, // unmapped domain: literal only
		{"sensor", "on", "on", true},
	}
	for _, tc := range cases {
		if got := matchesSemantic(tc.domain, tc.state, tc.semantic); got != tc.want {
			t.Errorf("matchesSemantic(%s, %s, %s) = %v, want %v", tc.domain, tc.state, tc.semantic, got, tc.want)
		}
	}
}

func TestExpectedStatePredicate(t *testing.T) {
	t.Parallel()

	if expect, ok := expectedStateForAction("light", "on", nil); !ok || !expect("on") || expect("off") {
		t.Fatal("light.on expectation wrong")
	}
	if expect, ok := expectedStateForAction("cover", "close", nil); !ok || !expect("closed") {
		t.Fatal("cover.close expectation wrong")
	}
	if _, ok := expectedStateForAction("scene", "activate", nil); ok {
		t.Fatal("scene.activate must have no expected state")
	}
	if expect, ok := expectedStateForAction("climate", "set", map[string]any{"hvac_mode": "heat"}); !ok || !expect("heat") {
		t.Fatal("climate.set with hvac_mode expectation wrong")
	}
	if _, ok := expectedStateForAction("climate", "set", map[string]any{"temperature": 21.0}); ok {
		t.Fatal("climate.set temperature-only must have no expected state")
	}
}

// --- helpers ---

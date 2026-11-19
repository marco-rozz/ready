package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// ── test doubles ─────────────────────────────────────────────────────────

// fakeSkills is an inert SkillsService.
type fakeSkills struct{}

func (fakeSkills) Session(context.Context, string) (string, error)        { return "", nil }
func (fakeSkills) MetadataPrompt(context.Context, string) (string, error) { return "", nil }
func (fakeSkills) LoadSkill(context.Context, string, string) error        { return nil }
func (fakeSkills) SkillBody(context.Context, string) (string, bool)       { return "", false }
func (fakeSkills) Close(context.Context, string) error                    { return nil }

// scriptedGen returns a GenEnv whose ExecuteTurn follows a script.
type scriptedGen struct {
	outcomes []genStep
	calls    int
}

type genStep struct {
	outcome agent.TurnOutcome
	err     error
}

func (g *scriptedGen) env() GenEnv {
	return GenEnv{
		ChainRefs:       []string{tChainRef},
		OwnerTools:      nil,
		BootstrapSkills: nil,
		ExecuteTurn: func(_ context.Context, _ orchestrator.TurnRequest, _ func(string)) (agent.TurnOutcome, error) {
			step := g.outcomes[min(g.calls, len(g.outcomes)-1)]
			g.calls++

			return step.outcome, step.err
		},
		RunnerSet: func(_ context.Context, _ Definition, _ string, _ []tool.BaseTool) (map[string]agent.Runner, error) {
			return map[string]agent.Runner{tChainRef: nil}, nil
		},
	}
}

// memoryHistoryFactory returns in-memory histories (no files).
func memoryHistoryFactory() func(ctx context.Context, path, childSessionID string) (*persistence.ConversationHistory, error) {
	return func(_ context.Context, _, childSessionID string) (*persistence.ConversationHistory, error) {
		return persistence.NewConversationHistory(childSessionID, nil).WithLogger(testLogger()), nil
	}
}

// ── 2.2: AppendBookkeeping invariants ────────────────────────────────────

func TestAppendBookkeepingDuringOpenGroup(t *testing.T) {
	// The owner's Turn has an unflushed open tool-call group; a handback
	// arrives. The bookkeeping append must NOT flush the open group and
	// must not land between a tool call and its result on disk.
	ctx := context.Background()

	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	// An owner turn: user + assistant(tool call), all UNSAVED.
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "do it", Timestamp: time.Now()})
	h.Add(persistence.HistoryMessage{
		Role:      persistence.RoleAssistant,
		Content:   "",
		ToolCalls: []persistence.ToolCallRecord{{ID: "c1", Name: "t", Arguments: "{}"}},
		Timestamp: time.Now(),
	})

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	hb := Handback{HandbackID: tHB1, DispatchID: "d1", SubagentID: "c1", Status: HandbackCompleted, Report: "ok", CreatedAt: time.Now()}
	if err := rt.appendHandback(ctx, owner, hb); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The record is on disk; the open group is not flushed around it.
	data, err := persistence.NewSessionFile(owner.History.Path()).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}

	text := string(data)
	if !strings.Contains(text, `"kind":"handback"`) {
		t.Fatalf("handback record missing on disk: %q", text)
	}

	if strings.Contains(text, `"tool_calls"`) {
		t.Fatalf("open group flushed by the bookkeeping append: %q", text)
	}

	// The order: the record must come AFTER the last flushed line — the
	// handback is the ONLY line on disk (the group was never written).
	if got := strings.Count(text, "\n"); got != 1 {
		t.Fatalf("unexpected disk state (%d lines): %q", got, text)
	}
}

func TestAppendBookkeepingAfterCloseRefused(t *testing.T) {
	ctx := context.Background()
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	// A Turn, saved, then the closing record.
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "x", Timestamp: time.Now()})
	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	if err := h.AppendRecord(ctx, persistence.HistoryMessage{Kind: persistence.KindSessionClosed, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	// Silent discard (the error is the closed refusal; appendHandback
	// treats it as a discard and returns nil).
	err := rt.appendHandback(ctx, owner, Handback{HandbackID: "hb", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("a handback after the closing record must be a silent discard, got %v", err)
	}

	if rt.reg.PendingCount("s1") != 0 {
		t.Fatalf("pending set gained the discarded handback")
	}
}

// ── 5.7: delivery at the head of the next Turn ───────────────────────────

func TestLateHandbackDeliveredAtNextTurn(t *testing.T) {
	ctx := context.Background()
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	if err := rt.appendHandback(ctx, owner, Handback{HandbackID: tHB1, DispatchID: "d1", SubagentID: "c1", Name: "explore-1", Status: HandbackCompleted, Report: "the result", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// DeliverPending (the pre-turn hook): deliveries land BEFORE the user
	// message.
	rt.DeliverPending(ctx, owner, h, 16384)

	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "thanks", Timestamp: time.Now()})

	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	data, _ := persistence.NewSessionFile(h.Path()).Read(ctx)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Expected order: handback (arrival, bookkeeping) → handback_delivery → user.
	if len(lines) != 3 {
		t.Fatalf("unexpected line count %d: %v", len(lines), lines)
	}

	var recs [3]persistence.HistoryMessage
	for i, l := range lines {
		if err := jsonUnmarshal(l, &recs[i]); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}

	if recs[0].Kind != persistence.KindHandback {
		t.Fatalf("first record = %s", recs[0].Kind)
	}

	if recs[1].Kind != persistence.KindHandbackDelivery || recs[1].DeliveredHandbackID != tHB1 {
		t.Fatalf("second record = %+v", recs[1])
	}

	if !recs[2].IsTurnBoundary() || recs[2].Content != "thanks" {
		t.Fatalf("third record = %+v", recs[2])
	}

	// The model view carries the delivery then the user message.
	view := h.Messages()
	if len(view) != 2 || view[0].Kind != persistence.KindHandbackDelivery {
		t.Fatalf("view = %+v", view)
	}

	// Consumed: no longer pending.
	if rt.reg.PendingCount("s1") != 0 {
		t.Fatalf("pending count after delivery = %d", rt.reg.PendingCount("s1"))
	}
}

// ── 5.7: the wait door consumes and never re-injects ─────────────────────

func TestWaitConsumesAndNeverReinjects(t *testing.T) {
	ctx := context.Background()
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	rt := testRuntime()
	// Join the background dispatch before t.TempDir is removed (LIFO: this
	// cleanup runs first), so it cannot write into a vanishing directory.
	t.Cleanup(func() { rt.Shutdown(ctx) })

	owner := testOwner("s1", "cli")
	owner.History = h

	// The child is registered (the real flow always has a spawned child).
	st, spawnErr := rt.Spawn(ctx, owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask})
	if spawnErr != nil {
		t.Fatal(spawnErr)
	}

	if appendErr := rt.appendHandback(ctx, owner, Handback{HandbackID: tHB1, SubagentID: st.ID, Status: HandbackCompleted, Report: "ok", CreatedAt: time.Now()}); appendErr != nil {
		t.Fatal(appendErr)
	}

	res, werr := rt.Wait(ctx, owner, []string{"c1"}, time.Second)
	if werr != nil {
		t.Fatal(werr)
	}

	if len(res.Handbacks) != 1 || res.Handbacks[0].HandbackID != tHB1 {
		t.Fatalf("wait result = %+v", res)
	}

	// Consumed: the pre-turn hook delivers nothing.
	rt.DeliverPending(ctx, owner, h, 16384)

	if rt.reg.PendingCount("s1") != 0 {
		t.Fatalf("handback re-pending after the wait door")
	}

	view := h.Messages()
	for _, m := range view {
		if m.Kind == persistence.KindHandbackDelivery {
			t.Fatal("wait-consumed handback re-injected at the next Turn")
		}
	}
}

// ── 5.7: the budget keeps a small context window safe ────────────────────

func TestDeliveryBudgetAndTruncation(t *testing.T) {
	ctx := context.Background()
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	// Three pending handbacks of ~2000 tokens each; the budget is 2048.
	big := strings.Repeat("x", 2000*4)
	for i := 0; i < 3; i++ {
		if err := rt.appendHandback(ctx, owner, Handback{
			HandbackID: string(rune('a'+i)) + "-hb",
			SubagentID: "c1",
			Status:     HandbackCompleted,
			Report:     big,
			CreatedAt:  time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	budget := 2048
	delivered, remaining := rt.DeliverPending(ctx, owner, h, budget)

	if delivered != 1 || remaining != 2 {
		t.Fatalf("delivered = %d, remaining = %d", delivered, remaining)
	}

	if rt.reg.PendingCount("s1") != 2 {
		t.Fatalf("pending after budget = %d", rt.reg.PendingCount("s1"))
	}

	// An oversized single handback is truncated, never stuck.
	h2 := persistence.NewConversationHistory("s2", persistence.NewSessionFile(t.TempDir()+"/s2.jsonl")).
		WithLogger(testLogger())
	owner2 := testOwner("s2", "cli")
	owner2.History = h2

	if err := rt.appendHandback(ctx, owner2, Handback{HandbackID: "big", SubagentID: "c2", Status: HandbackCompleted, Report: big, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	delivered, _ = rt.DeliverPending(ctx, owner2, h2, budget)
	if delivered != 1 {
		t.Fatal("the oldest pending handback must always be delivered")
	}

	if rt.reg.PendingCount("s2") != 0 {
		t.Fatalf("an oversized handback stuck pending")
	}
}

// ── 4.7: the owner path with nothing pending is untouched ────────────────

func TestNoPendingHandbackLeavesOwnerPathUntouched(t *testing.T) {
	ctx := context.Background()
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	before, _ := h.View()

	rt.DeliverPending(ctx, owner, h, 16384)

	after, _ := h.View()
	if len(after) != len(before) {
		t.Fatalf("the no-pending hook changed the transcript: %d → %d", len(before), len(after))
	}
}

// ── 5b: internal jobs ────────────────────────────────────────────────────

func TestRunJobCallbackAndClosure(t *testing.T) {
	rt := testRuntimeWithScript(genStep{outcome: agent.TurnOutcome{Reply: "job done"}})

	var got []Handback
	err := rt.RunJob(context.Background(), JobSpec{
		Name: tJobName,
		Type: tTypeGP,
		Task: tJobTask,
		OnHandback: func(_ context.Context, hb Handback) {
			got = append(got, hb)
		},
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if len(got) != 1 || got[0].Status != HandbackCompleted || got[0].Report != "job done" {
		t.Fatalf("callback = %+v", got)
	}
}

func TestRunJobFailureStillCallsBack(t *testing.T) {
	rt := testRuntimeWithScript(genStep{err: llm.ErrChainExhausted})

	var got []Handback
	err := rt.RunJob(context.Background(), JobSpec{
		Name: tJobName,
		Type: tTypeGP,
		Task: tJobTask,
		OnHandback: func(_ context.Context, hb Handback) {
			got = append(got, hb)
		},
	})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if len(got) != 1 || got[0].Status != HandbackFailed {
		t.Fatalf("callback = %+v", got)
	}
}

func TestRunJobWithFeatureDisabled(t *testing.T) {
	// Jobs run regardless of subagents.enabled (D13).
	rt := testRuntimeWithScript(genStep{outcome: agent.TurnOutcome{Reply: string(StatusDone)}})
	rt.deps.Enabled = false

	var got []Handback
	err := rt.RunJob(context.Background(), JobSpec{
		Name:       tJobName,
		Type:       "general-purpose",
		Task:       tJobTask,
		OnHandback: func(_ context.Context, hb Handback) { got = append(got, hb) },
	})
	if err != nil {
		t.Fatalf("jobs must run with the feature disabled: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("callback = %+v", got)
	}
}

func TestRunJobJobsCap(t *testing.T) {
	deps := testDeps(genStep{outcome: agent.TurnOutcome{Reply: string(StatusDone)}})
	deps.Limits.MaxConcurrentJobs = 1
	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))

	if err := rt.reg.BeginJob(); err != nil {
		t.Fatal(err)
	}
	defer rt.reg.EndJob()

	err := rt.RunJob(context.Background(), JobSpec{Name: "j", Type: tTypeGP, Task: "t"})
	if !errors.Is(err, ErrJobsCap) {
		t.Fatalf("expected ErrJobsCap, got %v", err)
	}
}

func TestRunJobInternalDefinitionHiddenFromModelSpawns(t *testing.T) {
	rt := testRuntimeWithScript(genStep{outcome: agent.TurnOutcome{Reply: string(StatusDone)}})

	// An internal definition is refused for a channel owner (D20).
	defs := NewDefinitions([]Definition{
		{Name: tTypeGP, Description: "g"},
		{Name: "catalog-curator", Description: "c", Visibility: VisibilityInternal},
	})
	rt.SetDefinitions(defs)

	owner := testOwner("s1", "cli")
	if _, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: dTypeCurator, Task: "x"}); !errors.Is(err, ErrDefinitionUnknown) {
		t.Fatalf("expected ErrDefinitionUnknown, got %v", err)
	}

	// …and absent from the model-facing type list.
	for _, d := range defs.PublicTypes() {
		if d.Name == "catalog-curator" {
			t.Fatal("internal definition leaked into the public type list")
		}
	}
}

// ── 5.7: handback is not activity for the reset rules ────────────────────

func TestHandbackIsNotActivity(t *testing.T) {
	h := persistence.NewConversationHistory("s1", persistence.NewSessionFile(t.TempDir()+"/s1.jsonl")).
		WithLogger(testLogger())

	ctx := context.Background()
	turnAt := time.Now().Add(-time.Hour)
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "x", Timestamp: turnAt})
	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	rt := testRuntime()
	owner := testOwner("s1", "cli")
	owner.History = h

	if err := rt.appendHandback(ctx, owner, Handback{HandbackID: "hb", CreatedAt: time.Now(), Report: "r", Status: HandbackCompleted}); err != nil {
		t.Fatal(err)
	}

	if got := h.LastActivity(); !got.Equal(turnAt) {
		t.Fatalf("lastActivity moved to %v (want %v): the handback must not be activity", got, turnAt)
	}
}

// ── D6b: a delivery run opens the Turn it precedes ───────────────────────

func TestDeliveryRunTurnOpener(t *testing.T) {
	delivery := persistence.HistoryMessage{Role: persistence.RoleUser, Kind: persistence.KindHandbackDelivery, Content: "d", Timestamp: time.Now()}
	user := persistence.HistoryMessage{Role: persistence.RoleUser, Content: "u", Timestamp: time.Now()}

	msgs := []persistence.HistoryMessage{
		{Role: persistence.RoleUser, Content: "turn1", Timestamp: time.Now()},
		{Role: persistence.RoleAssistant, Content: "a", Timestamp: time.Now()},
		delivery, delivery, user,
	}

	starts := persistence.TurnStarts(msgs)
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 2 {
		t.Fatalf("starts = %v, want [0 2] (the delivery run opens the second Turn)", starts)
	}

	// OmitOldestTurns never separates a delivery run from its Turn.
	kept := persistence.OmitOldestTurns(msgs, 1)
	if len(kept) != 3 || kept[0].Kind != persistence.KindHandbackDelivery {
		t.Fatalf("kept = %+v", kept)
	}

	// A trailing delivery run without a user message is NOT an opener.
	trailing := []persistence.HistoryMessage{
		{Role: persistence.RoleUser, Content: "t", Timestamp: time.Now()},
		{Role: persistence.RoleAssistant, Content: "a", Timestamp: time.Now()},
		delivery,
	}
	if s := persistence.TurnStarts(trailing); len(s) != 1 || s[0] != 0 {
		t.Fatalf("trailing deliveries must not open a Turn: %v", s)
	}
}

// ── test runtime constructors ────────────────────────────────────────────

// testRuntime builds a Runtime with inert dependencies (no generation use,
// in-memory histories).
func testRuntime() *Runtime {
	rt := testRuntimeWithScript(genStep{})
	rt.nowForTest()

	return rt
}

// testRuntimeWithScript builds a Runtime whose generation lease returns a
// scripted GenEnv.
func testRuntimeWithScript(steps ...genStep) *Runtime {
	return New(testDeps(steps...), NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))
}

// testDeps builds inert Deps with a scripted generation.
func testDeps(steps ...genStep) Deps {
	script := &scriptedGen{outcomes: steps}

	return Deps{
		Logger:                   testLogger(),
		Limits:                   Limits{MaxDepth: 2, MaxConcurrent: 8, MaxConcurrentJobs: 4, MailboxBuffer: 4},
		WaitTimeout:              time.Second,
		HandbackMaxTokens:        4096,
		HandbackTurnBudgetTokens: 16384,
		Enabled:                  true,
		Generation: func(_ context.Context) (GenEnv, func()) {
			return script.env(), func() {}
		},
		Skills:              fakeSkills{},
		ChildHistoryFactory: memoryHistoryFactory(),
		OwnerUsage: func(string) *usage.Accumulator {
			return nil
		},
		JobSessionFactory: func(_ context.Context, jobID string, _ time.Time) (Owner, error) {
			return Owner{
				SessionID: jobID,
				Address:   AddressSystem,
				Kind:      OwnerJob,
				History:   persistence.NewConversationHistory(jobID, nil).WithLogger(testLogger()),
				Delivery:  Delivery{Kind: DeliveryCallback},
			}, nil
		},
	}
}

// nowForTest replaces the clock (a stable point for the tests).
func (r *Runtime) nowForTest() {
	r.now = func() time.Time { return time.Unix(1700000000, 0) }
}

// jsonUnmarshal wraps json.Unmarshal for the record round-trip checks.
func jsonUnmarshal(line string, v any) error {
	return json.Unmarshal([]byte(line), v)
}

// The user-facing gate reads the reloadable flag (D14), not the boot-time
// Deps value: a false→true flip after a config reload must open
// Spawn/Send/Interrupt without a restart, and true→false must close them.
func TestSetEnabledReloadFlip(t *testing.T) {
	deps := testDeps(genStep{outcome: agent.TurnOutcome{Reply: string(StatusDone)}})
	deps.Enabled = false
	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))
	owner := testOwner("s1", "cli")

	if _, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Task: "x"}); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("spawn while disabled = %v, want ErrFeatureDisabled", err)
	}

	if err := rt.Send(context.Background(), owner, nil, "any", "x"); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("send while disabled = %v, want ErrFeatureDisabled", err)
	}

	if err := rt.Interrupt(context.Background(), owner, "any"); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("interrupt while disabled = %v, want ErrFeatureDisabled", err)
	}

	// false→true flip (the reload path): the gate opens without a restart
	// and the first real dispatch starts.
	rt.SetEnabled(true)

	st, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Name: "c1", Task: tTask})
	if err != nil {
		t.Fatalf("spawn after enabling = %v, want the gate open", err)
	}

	// The dispatch goroutine writes Status under the registry lock, so the
	// read must take it too.
	rt.reg.mu.Lock()
	status := st.Status
	rt.reg.mu.Unlock()

	if status != StatusRunning && status != StatusDone {
		t.Fatalf("spawned child status = %q", status)
	}

	// true→false flip: the gate closes again (the fleet cascade is a
	// separate, reload-driven step — DisableChannelFleets — out of scope
	// here).
	rt.SetEnabled(false)

	if _, err := rt.Spawn(context.Background(), owner, nil, SpawnRequest{Type: tTypeGP, Task: "x"}); !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("spawn after disabling = %v, want ErrFeatureDisabled", err)
	}
}

// A job bypasses the gate in both flag states (D13): jobs are
// infrastructure, always available.
func TestSetEnabledDoesNotGateJobs(t *testing.T) {
	deps := testDeps(genStep{outcome: agent.TurnOutcome{Reply: string(StatusDone)}})
	deps.Enabled = false
	rt := New(deps, NewDefinitions([]Definition{{Name: tTypeGP, Description: "g"}}))

	var got []Handback
	if err := rt.RunJob(context.Background(), JobSpec{
		Name:       "flip-job",
		Type:       tTypeGP,
		Task:       "x",
		OnHandback: func(_ context.Context, hb Handback) { got = append(got, hb) },
	}); err != nil {
		t.Fatalf("job while the feature is disabled = %v, want it to run", err)
	}

	if len(got) != 1 {
		t.Fatalf("handbacks = %+v", got)
	}
}

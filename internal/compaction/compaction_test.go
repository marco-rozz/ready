package compaction

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

func u(s string) persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleUser, Content: s}
}

func tc(s string) persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleUser, Kind: persistence.KindTurnContext, Content: s}
}

func a(s string, calls ...persistence.ToolCallRecord) persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: s, ToolCalls: calls}
}

func res(id, s string) persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleTool, ToolCallID: id, Content: s}
}

func lines(n, offset int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = offset + i
	}

	return out
}

// big returns text worth n tokens.
func big(n int) string { return strings.Repeat("x", 4*n) }

// A view of `turns` Turns, each ~tokens tokens: user + ctx + reply.
func simpleView(turns, tokens int) []persistence.HistoryMessage {
	var out []persistence.HistoryMessage
	for i := 0; i < turns; i++ {
		out = append(out, u("q"), tc("c"), a(big(tokens)))
	}

	return out
}

// --- 7.1 planner ---

func TestPlanCut_TokenModeCutsAtTurnBoundary(t *testing.T) {
	entries := simpleView(10, 1000) // ~1000 tokens per Turn
	p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 5), KeepRecentTokens: 2500})
	if !ok {
		t.Fatal("expected a cut")
	}

	if !entries[p.Cut].IsTurnBoundary() {
		t.Fatalf("cut at %d is not a Turn boundary", p.Cut)
	}

	// Walking back 2500 tokens reaches Turn 7's reply; its boundary keeps
	// Turns 7-9 (at least the budget, whole Turns).
	if p.Cut != 21 || p.FirstKept != 26 || len(p.ToSummarise) != 21 {
		t.Errorf("plan = cut %d first_kept %d summarised %d, want 21/26/21", p.Cut, p.FirstKept, len(p.ToSummarise))
	}
}

func TestPlanCut_WholeViewFitsNothingToCompact(t *testing.T) {
	entries := simpleView(3, 100)
	if _, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: 20000}); ok {
		t.Error("a view under the kept budget has nothing to compact")
	}
}

// The budget edge falls between a tool call and its result: the cut moves so
// both are kept or both summarised.
func TestPlanCut_NeverSplitsToolGroup(t *testing.T) {
	call := persistence.ToolCallRecord{ID: "c1", Name: "t", Arguments: "{}"}
	entries := []persistence.HistoryMessage{
		u("q1"), tc("c"), a(big(500)),
		u("q2"), tc("c"), a("", call), res("c1", big(900)), a(big(10)),
	}

	for keep := 1; keep < 1600; keep += 37 {
		p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: keep})
		if !ok {
			continue
		}

		if entries[p.Cut].Role == persistence.RoleTool || entries[p.Cut].Kind == persistence.KindTurnContext {
			t.Fatalf("keep=%d: cut at %d starts the tail with %s/%s", keep, p.Cut, entries[p.Cut].Role, entries[p.Cut].Kind)
		}
	}
}

// A single Turn larger than the kept budget is split at an assistant message.
func TestPlanCut_SplitsOversizedTurn(t *testing.T) {
	c1 := persistence.ToolCallRecord{ID: "c1", Name: "t", Arguments: "{}"}
	c2 := persistence.ToolCallRecord{ID: "c2", Name: "t", Arguments: "{}"}
	entries := []persistence.HistoryMessage{
		u("only turn"), tc("c"),
		a("", c1), res("c1", big(3000)),
		a("", c2), res("c2", big(3000)),
		a(big(100)),
	}

	p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: 2000})
	if !ok {
		t.Fatal("expected a split-turn cut")
	}

	if entries[p.Cut].Role != persistence.RoleAssistant {
		t.Fatalf("split cut must start at an assistant entry, got %s", entries[p.Cut].Role)
	}

	if p.Cut != 4 {
		t.Errorf("cut = %d, want 4 (after the first group)", p.Cut)
	}
}

func TestPlanCut_TurnMode(t *testing.T) {
	entries := simpleView(20, 10)
	p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), TurnMode: true, KeepRecentTurns: 5})
	if !ok || p.Cut != 45 {
		t.Fatalf("turn-mode cut = %d (%v), want 45 (last 5 Turns kept)", p.Cut, ok)
	}

	if _, ok := PlanCut(PlanInput{Entries: simpleView(5, 10), Lines: lines(15, 0), TurnMode: true, KeepRecentTurns: 5}); ok {
		t.Error("five Turns with keep_recent_turns 5 leave nothing to compact")
	}
}

// Input guard: only the oldest slice that fits the window is summarised.
func TestPlanCut_InputGuardShrinksToOldestSlice(t *testing.T) {
	entries := simpleView(10, 1000)
	p, ok := PlanCut(PlanInput{
		Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: 1500,
		MaxInputTokens: 3500, OverheadTokens: 500,
	})
	if !ok {
		t.Fatal("expected a guarded cut")
	}

	if !entries[p.Cut].IsTurnBoundary() || p.Cut > 9 {
		t.Errorf("guarded cut = %d, want a boundary summarising at most ~3 Turns", p.Cut)
	}

	if _, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: 1500, MaxInputTokens: 100, OverheadTokens: 100}); ok {
		t.Error("no slice fits when the overhead alone fills the input budget")
	}
}

// --- 7.2 serialiser ---

func TestSerialise(t *testing.T) {
	load := persistence.ToolCallRecord{ID: "l", Name: SkillsLoadTool, Arguments: `{"skill_name":"home-provisioning"}`}
	query := persistence.ToolCallRecord{ID: "q", Name: "ha_state_query", Arguments: `{"domain":"light"}`}
	entries := []persistence.HistoryMessage{
		u("turn on the kitchen light"), tc("<turn_context_x>…</turn_context_x>"),
		a("checking", load, query),
		res("l", "# Provisioning\nlong body"),
		res("q", strings.Repeat("y", 9000)),
		a("done"),
	}

	got := Serialise(entries)

	for _, want := range []string{
		"[User]: turn on the kitchen light",
		"[Assistant]: checking",
		`[Assistant tool calls]: skills-load({"skill_name":"home-provisioning"}); ha_state_query({"domain":"light"})`,
		`[Tool result]: skill "home-provisioning" loaded; preserved verbatim, do not summarise`,
		"[Tool result]: " + strings.Repeat("y", 2000) + "… [truncated: 7000 more characters]",
		"[Assistant]: done",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("serialised text lacks %q", want[:min(len(want), 80)])
		}
	}

	if strings.Contains(got, "turn_context") || strings.Contains(got, "long body") {
		t.Error("turn context and skill bodies must not be serialised")
	}

	if order := LoadOrder(entries); len(order) != 1 || order[0] != "home-provisioning" {
		t.Errorf("LoadOrder = %v", order)
	}
}

// --- trigger / backoff ---

func TestTrigger(t *testing.T) {
	tr := Trigger{Window: 131072, Reserve: 16384}
	if tr.Threshold() != 114688 {
		t.Fatalf("threshold = %d", tr.Threshold())
	}

	// Post-step formatter does not hide the size: the measurement is the
	// main call's 120000.
	if !tr.Due(Compared(120000, true, 50, 30000), 0) {
		t.Error("120000 main prompt tokens must trigger")
	}

	// Stale measurement ignored: the estimate alone decides.
	if tr.Due(Compared(120000, false, 0, 30000), 0) {
		t.Error("an invalid measurement must not trigger")
	}

	// Under-reporting provider: the estimate wins.
	if !tr.Due(Compared(1000, true, 0, 120000), 0) {
		t.Error("the estimate must win over an under-reported measurement")
	}

	turns := Trigger{MaxTurns: 20}
	if turns.Due(0, 19) || !turns.Due(0, 20) {
		t.Error("turn mode triggers at max_turns")
	}
}

func TestBackoff(t *testing.T) {
	var b Backoff

	b.Failed() // skip 1
	if !b.Skip() || b.Skip() {
		t.Fatal("after one failure exactly one evaluation is skipped")
	}

	b.Failed() // skip 2
	first, second, third := b.Skip(), b.Skip(), b.Skip()
	if !first || !second || third {
		t.Fatal("after two failures two evaluations are skipped")
	}

	for i := 0; i < 5; i++ {
		b.Failed()
	}

	n := 0
	for b.Skip() {
		n++
	}

	if n != maxSkip {
		t.Errorf("skip capped at %d, got %d", maxSkip, n)
	}

	b.Failed()
	b.Succeeded()

	if b.Skip() {
		t.Error("a success resets the backoff")
	}
}

func TestSummaryMessageAndPreservedSkills(t *testing.T) {
	msg := SummaryMessage("did things", []SkillBlock{{Name: "home-provisioning", Body: "# P"}})
	if !strings.HasPrefix(msg, "<summary>\ndid things\n</summary>\n\n<loaded_skills>\n<skill name=\"home-provisioning\">\n# P\n</skill>\n</loaded_skills>") {
		t.Errorf("summary message = %q", msg)
	}

	if SummaryMessage("", nil) != "" {
		t.Error("empty summary message expected")
	}

	got := PreservedSkills(
		[]string{"home-topology", "telegram-formatter", "b", "a", "home-compaction"},
		map[string]bool{"home-topology": true, "telegram-formatter": true, "home-compaction": true},
		[]string{"a"}, []string{"b", "unloaded"},
	)
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("PreservedSkills = %v, want [a b] (unloaded and excluded skills dropped)", got)
	}
}

// --- 7.3 summariser ---

type fakeModel struct {
	model.BaseChatModel
	got   []*schema.Message
	reply string
	err   error
	delay time.Duration
}

func (f *fakeModel) Generate(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.got = in

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if f.err != nil {
		return nil, f.err
	}

	return schema.AssistantMessage(f.reply, nil), nil
}

func TestSummariser_SkillPromptAndRequest(t *testing.T) {
	m := &fakeModel{reply: "  the summary  "}
	s := NewSummariser(m, func(context.Context) (string, error) { return "SKILL BODY", nil }, time.Second, zerolog.Nop())

	got, err := s.Summarise(context.Background(), Request{PreviousSummary: "old", Conversation: "[User]: hi", Instructions: "keep the heating schedule decisions"})
	if err != nil || got != "the summary" {
		t.Fatalf("Summarise = (%q, %v)", got, err)
	}

	if m.got[0].Content != "SKILL BODY" {
		t.Errorf("system prompt = %q, want the skill", m.got[0].Content)
	}

	for _, want := range []string{"<previous_summary>\nold", "[User]: hi", "keep the heating schedule decisions"} {
		if !strings.Contains(m.got[1].Content, want) {
			t.Errorf("user message lacks %q", want)
		}
	}
}

func TestSummariser_FallbackWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	m := &fakeModel{reply: "s"}
	s := NewSummariser(m, func(context.Context) (string, error) { return "", errors.New("skill not found") }, time.Second, zerolog.New(&buf))

	for i := 0; i < 3; i++ {
		if _, err := s.Summarise(context.Background(), Request{Conversation: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	if !strings.Contains(m.got[0].Content, "ha_state_query") {
		t.Error("fallback prompt expected")
	}

	if n := strings.Count(buf.String(), "compaction skill unavailable"); n != 1 {
		t.Errorf("fallback WARN logged %d times, want 1", n)
	}
}

func TestSummariser_FailuresAndTimeout(t *testing.T) {
	if _, err := NewSummariser(&fakeModel{reply: "  "}, nil, time.Second, zerolog.Nop()).Summarise(context.Background(), Request{}); !errors.Is(err, ErrEmptySummary) {
		t.Errorf("empty reply err = %v", err)
	}

	if _, err := NewSummariser(&fakeModel{err: errors.New("chain exhausted")}, nil, time.Second, zerolog.Nop()).Summarise(context.Background(), Request{}); err == nil {
		t.Error("model error must fail")
	}

	start := time.Now()
	if _, err := NewSummariser(&fakeModel{reply: "x", delay: time.Minute}, nil, 50*time.Millisecond, zerolog.Nop()).Summarise(context.Background(), Request{}); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("timeout not enforced: err=%v after %s", err, time.Since(start))
	}
}

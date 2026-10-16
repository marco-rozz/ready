package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/formatter"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/usage"
)

// scriptRunner is an agent.Runner recording every input and optionally
// attributing main-agent usage (PromptTokens) to the turn's accumulator.
type scriptRunner struct {
	mu     sync.Mutex
	inputs [][]*schema.Message
	calls  int

	reply  string
	prompt int   // main-agent prompt tokens recorded per call (0 = none)
	err    error // returned after recording usage

	// started/release block the FIRST call (FIFO tests).
	started chan struct{}
	release chan struct{}

	// beforeCall runs at the start of every call.
	beforeCall func()
}

var _ agent.Runner = (*scriptRunner)(nil)

func (r *scriptRunner) RunMessagesStreaming(ctx context.Context, _ string, msgs []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
	r.mu.Lock()
	r.inputs = append(r.inputs, msgs)
	r.calls++
	n := r.calls
	prompt, reply, err, hook := r.prompt, r.reply, r.err, r.beforeCall
	r.mu.Unlock()

	if hook != nil {
		hook()
	}

	if n == 1 && r.started != nil {
		close(r.started)
		<-r.release
	}

	if prompt > 0 {
		usage.FromContext(ctx).AddUsage(schema.TokenUsage{PromptTokens: prompt, CompletionTokens: 10}, false)
	}

	if err != nil {
		return agent.TurnOutcome{}, err
	}

	if onChunk != nil {
		onChunk(reply)
	}

	return agent.TurnOutcome{Reply: reply, ContentDelivered: onChunk != nil}, nil
}

func (r *scriptRunner) set(fn func(r *scriptRunner)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r)
}

func (r *scriptRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

func (r *scriptRunner) lastInput() []*schema.Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.inputs[len(r.inputs)-1]
}

// summariserModel is the fake auxiliary chain used by the summariser.
type summariserModel struct {
	mu     sync.Mutex
	calls  int
	inputs [][]*schema.Message
	reply  string
	err    error
}

var _ model.BaseChatModel = (*summariserModel)(nil)

func (m *summariserModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	m.inputs = append(m.inputs, in)

	if m.err != nil {
		return nil, m.err
	}

	return schema.AssistantMessage(m.reply, nil), nil
}

func (m *summariserModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("not used")
}

func (m *summariserModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

// lifecycleEnv is a termEnv driven by a scriptRunner with compaction wired.
type lifecycleEnv struct {
	*termEnv
	runner *scriptRunner
	sum    *summariserModel
	buf    *termBuffer
}

func newLifecycleEnv(t *testing.T, channels ...channel.Channel) *lifecycleEnv {
	t.Helper()

	buf := &termBuffer{}
	if len(channels) == 0 {
		channels = []channel.Channel{newTestCLIChannel(buf)}
	}

	env := newTermEnv(t, channelFormatterCfg{}, channels...)
	runner := &scriptRunner{reply: "ok"}
	env.agentReg = &agentRegistry{exec: &plainRunnerExec{runner: runner, layout: llm.LayoutTailDynamic}, layout: llm.LayoutTailDynamic}

	sum := &summariserModel{reply: "SUMMARY"}
	rt := env.runtime()
	rt.compactCfg = config.CompactionConfig{KeepRecentTokens: 200, KeepRecentTurns: 1, MaxTurns: 1000, Timeout: time.Second}
	rt.summariser = compaction.NewSummariser(sum, rt.compactionSkillPrompt, time.Second, zerolog.Nop())

	return &lifecycleEnv{termEnv: env, runner: runner, sum: sum, buf: buf}
}

func (e *lifecycleEnv) cur() *session.Session {
	return e.sessions.Current(context.Background(), testCLIChannelName)
}

func lastFileLine(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	return lines[len(lines)-1]
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()

	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			n++
		}

		return nil
	})

	return n
}

// ── 5.1/5.2: /new ────────────────────────────────────────────────────────

func TestSessionCommand_NewEndsSessionWithoutReachingTheModel(t *testing.T) {
	env := newLifecycleEnv(t)

	env.run(cliMsg("hello"))
	old := env.cur()
	calls := env.runner.callCount()

	env.run(cliMsg("/new"))

	if env.runner.callCount() != calls {
		t.Fatal("/new must never reach the model")
	}

	if !strings.Contains(env.buf.String(), msgSessionStarted) {
		t.Fatalf("expected %q, got %q", msgSessionStarted, env.buf.String())
	}

	last := lastFileLine(t, old.Path)
	if !strings.Contains(last, `"kind":"session_closed"`) || !strings.Contains(last, `"reason":"command"`) || !strings.Contains(last, `"turns":1`) {
		t.Fatalf("closing record = %s", last)
	}

	if env.cur().ID == old.ID {
		t.Fatal("expected a new current session")
	}

	// Repeated /new: no-op, no extra file or closing record.
	env.run(cliMsg("/new"))

	if !strings.Contains(env.buf.String(), msgSessionAlreadyNew) {
		t.Fatalf("expected %q, got %q", msgSessionAlreadyNew, env.buf.String())
	}

	if n := countFiles(t, env.sessionsDir); n != 1 {
		t.Errorf("expected only the ended session file, got %d files", n)
	}

	data, _ := os.ReadFile(old.Path)
	if strings.Count(string(data), "session_closed") != 1 || strings.Count(string(data), "turn_usage") != 1 {
		t.Errorf("expected one closing record and one turn_usage (commands record nothing):\n%s", data)
	}
}

// /new is queued FIFO behind the Turn in flight: the Turn is recorded in the
// old Session, and only then is it closed.
func TestSessionCommand_NewQueuedBehindInFlightTurn(t *testing.T) {
	env := newLifecycleEnv(t)
	env.runner.started = make(chan struct{})
	env.runner.release = make(chan struct{})

	incoming := make(chan channel.Message, 4)
	d := &dispatcher{
		ctx:       context.Background(),
		incoming:  incoming,
		processFn: env.runtime().handleItem,
		keyFn:     func(m channel.Message) string { return string(session.AddressOf(m)) },
		logger:    zerolog.Nop(),
		workers:   map[string]chan channel.Message{},
		dropped:   map[string]int{},
	}

	done := make(chan struct{})
	go func() { d.run(); close(done) }()

	incoming <- cliMsg("slow request")
	<-env.runner.started
	old := env.cur()
	incoming <- cliMsg("/new")

	time.Sleep(50 * time.Millisecond)
	close(env.runner.release)
	close(incoming)
	<-done

	data, err := os.ReadFile(old.Path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[len(lines)-1], "session_closed") {
		t.Fatalf("the closing record must be last: %v", lines)
	}

	if !strings.Contains(string(data), `"content":"ok"`) {
		t.Errorf("the in-flight Turn must be recorded in the old session:\n%s", data)
	}
}

// ── 4.3: usage persisted per Turn, restored at restart ───────────────────

func TestTurnUsage_PersistedAndRestoredAcrossRestart(t *testing.T) {
	env := newLifecycleEnv(t)
	env.runner.set(func(r *scriptRunner) { r.prompt = 100 })

	env.run(cliMsg("one"))

	env.runner.set(func(r *scriptRunner) { r.err = errors.New("error 400: bad request") })
	env.run(cliMsg("two (fails)"))

	recs := env.jsonlRecords(t)
	if len(recs) != 2 {
		t.Fatalf("expected two turn_usage records (failed Turn included), got %+v", recs)
	}

	for _, r := range recs {
		if r.Kind != persistence.KindTurnUsage || r.Usage == nil || r.Usage.Turns != 1 || r.Usage.ReadTokens != 100 {
			t.Errorf("unexpected record %+v", r)
		}
	}

	// Restart: the totals are rebuilt from the file.
	mgr2, hist2, usage2 := testSessions(t, env.sessionsDir)
	cur := mgr2.Current(context.Background(), testCLIChannelName)
	hist2.For(context.Background(), cur)

	totals := usage2.Snapshot(cur.ID)
	if totals == nil || totals.TurnCount != 2 || totals.ReadTokens != 200 || totals.LLMCalls != 2 {
		t.Fatalf("restored totals = %+v", totals)
	}

	if totals.LastMainPromptTokens != 0 {
		t.Error("the occupancy measurement is never restored")
	}

	// The closing record spans the restart.
	mgr2.StartNew(context.Background(), testCLIChannelName)

	if last := lastFileLine(t, cur.Path); !strings.Contains(last, `"turns":2`) || !strings.Contains(last, `"read_tokens":200`) {
		t.Errorf("closing record after restart = %s", last)
	}
}

// ── 5.5: message-oriented reset with notice ──────────────────────────────

func seedOldTurn(t *testing.T, mgr *session.Manager, addr string, at time.Time) *session.Session {
	t.Helper()

	cur, h := mgr.History(context.Background(), session.Address(addr))
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "old", Timestamp: at})
	h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "old reply", Timestamp: at})

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	return cur
}

func TestReset_MessageOrientedSendsNotice(t *testing.T) {
	for _, notify := range []bool{true, false} {
		tg := &termTGChannel{}
		env := newLifecycleEnv(t, tg)
		env.runtime().resets = map[string]session.Rule{
			telegram.ChannelName: {Mode: config.ResetModeDaily, DailyAt: "04:00", Notify: notify},
		}

		old := seedOldTurn(t, env.sessions, "telegram-42", time.Now().Add(-30*time.Hour))

		env.run(channel.Message{Content: "hi", ChannelName: telegram.ChannelName, Metadata: map[string]string{telegram.MetadataChatID: "42"}})

		tg.mu.Lock()
		sent := append([]string(nil), tg.sent...)
		tg.mu.Unlock()

		if gotNotice := len(sent) > 0 && sent[0] == msgSessionReset; gotNotice != notify {
			t.Errorf("notify=%v: sent %v", notify, sent)
		}

		if last := lastFileLine(t, old.Path); !strings.Contains(last, `"reason":"reset_daily"`) {
			t.Errorf("closing record = %s", last)
		}

		if env.sessions.Current(context.Background(), "telegram-42").ID == old.ID {
			t.Error("the Turn must start a new session")
		}
	}
}

// The stdin CLI evaluates its rule once, at the first message after start.
func TestReset_StdinCLIEvaluatedOnceSilently(t *testing.T) {
	env := newLifecycleEnv(t)
	env.runtime().resets = map[string]session.Rule{testCLIChannelName: {Mode: config.ResetModeIdle, IdleAfter: time.Hour, Notify: true}}

	old := seedOldTurn(t, env.sessions, testCLIChannelName, time.Now().Add(-2*time.Hour))
	env.run(cliMsg("first"))

	if env.cur().ID == old.ID || strings.Contains(env.buf.String(), msgSessionReset) {
		t.Fatalf("expected a silent reset at the first message (buf %q)", env.buf.String())
	}

	second := env.cur()
	h := currentHistory(t, env.sessions, testCLIChannelName)
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "backdated", Timestamp: time.Now().Add(-3 * time.Hour)})

	env.run(cliMsg("second"))

	if env.cur().ID != second.ID {
		t.Error("the stdin CLI never resets again while running")
	}
}

// ── 7.4/7.5: post-reply compaction ───────────────────────────────────────

// testQuestion is the user text of the compaction scenarios.
const testQuestion = "question"

// bigReply is ~150 tokens of reply text.
var bigReply = strings.Repeat("word ", 120) //nolint:gochecknoglobals // shared test fixture

// useSmallWindow declares a 2000-token window with a 500-token reserve
// (threshold 1500).
func (e *lifecycleEnv) useSmallWindow() {
	e.runtime().trigger = compaction.Trigger{Window: 2000, Reserve: 500}
}

func TestCompaction_PostReplyTokenTrigger(t *testing.T) {
	env := newLifecycleEnv(t)
	env.useSmallWindow()
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply; r.prompt = 1800 })

	for i := 0; i < 3; i++ {
		env.run(cliMsg(testQuestion))
	}

	if env.sum.callCount() == 0 {
		t.Fatal("expected an automatic compaction after a reply over the threshold")
	}

	var rec *persistence.HistoryMessage
	for _, r := range env.jsonlRecords(t) {
		if r.Kind == persistence.KindCompaction {
			rec = &r
		}
	}

	if rec == nil || rec.Summary != "SUMMARY" || rec.FirstKept == 0 || rec.TokensBefore <= rec.TokensAfter {
		t.Fatalf("compaction record = %+v", rec)
	}

	// The next Turn sees the summary right after the prefix, then the kept
	// tail — never a summarised entry.
	env.run(cliMsg("after"))

	in := env.runner.lastInput()

	idx := -1
	for i, m := range in {
		if m.Role == schema.User && strings.HasPrefix(m.Content, "<summary>\nSUMMARY") {
			idx = i

			break
		}
	}

	if idx < 0 {
		t.Fatalf("summary message missing from %+v", in)
	}

	for _, m := range in[:idx] {
		if m.Role != schema.System {
			t.Fatalf("only the stable prefix may precede the summary, found %s", m.Role)
		}
	}

	users := 0
	for _, m := range in[idx+1:] {
		if m.Role == schema.User && m.Content == testQuestion {
			users++
		}
	}

	if users >= 3 {
		t.Errorf("summarised Turns must not be sent: found %d original questions", users)
	}
}

// A compaction followed by a Turn failing before its first main call does
// not compact again on the summariser's measurement.
func TestCompaction_NoRepeatOnStaleMeasurement(t *testing.T) {
	env := newLifecycleEnv(t)
	env.useSmallWindow()
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply; r.prompt = 1800 })

	for i := 0; i < 3 && env.sum.callCount() == 0; i++ {
		env.run(cliMsg(testQuestion))
	}

	calls := env.sum.callCount()
	if calls == 0 {
		t.Fatal("setup: expected a compaction")
	}

	env.runner.set(func(r *scriptRunner) { r.prompt = 0; r.err = errors.New("error 400: bad request") })
	env.run(cliMsg("fails before the main call"))

	if env.sum.callCount() != calls {
		t.Error("an invalid measurement must not trigger another compaction")
	}
}

// A post-step formatter's aux call does not hide the context size.
func TestCompaction_PostStepChannelStillCompacts(t *testing.T) {
	env := newLifecycleEnv(t)
	env.reg[testCLIChannelName] = channelFormatterCfg{skillName: "fmt", postStep: true}
	env.useSmallWindow()
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply; r.prompt = 1800 })

	env.fmtModel.generateFunc = func(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
		usage.FromContext(ctx).AddUsage(schema.TokenUsage{PromptTokens: 900}, true)

		return schema.AssistantMessage(bigReply, nil), nil
	}

	for i := 0; i < 3; i++ {
		env.run(cliMsg(testQuestion))
	}

	if env.sum.callCount() == 0 {
		t.Error("expected a compaction despite the formatter's smaller aux measurement")
	}
}

func TestCompaction_FailureAppendsNothingAndBacksOff(t *testing.T) {
	env := newLifecycleEnv(t)
	env.useSmallWindow()
	env.sum.err = errors.New("chain exhausted")
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply; r.prompt = 1800 })

	var logs bytes.Buffer
	env.runtime().logger = zerolog.New(&logs)

	for i := 0; i < 3 && env.sum.callCount() == 0; i++ {
		env.run(cliMsg(testQuestion))
	}

	if env.sum.callCount() != 1 {
		t.Fatalf("setup: expected one failed attempt, got %d", env.sum.callCount())
	}

	for _, r := range env.jsonlRecords(t) {
		if r.Kind == persistence.KindCompaction {
			t.Fatal("a failed compaction appends nothing")
		}
	}

	if !strings.Contains(logs.String(), `"consumer":"compaction"`) {
		t.Errorf("expected a WARN naming the consumer: %s", logs.String())
	}

	env.run(cliMsg("next")) // skipped (backoff 1)

	if env.sum.callCount() != 1 {
		t.Error("the evaluation after one failure must be skipped")
	}

	if !strings.Contains(env.buf.String(), "ok") && !strings.Contains(env.buf.String(), "word") {
		t.Error("the next message is answered normally")
	}

	env.run(cliMsg("again")) // evaluated again

	if env.sum.callCount() != 2 {
		t.Errorf("expected the backoff to expire, summariser calls = %d", env.sum.callCount())
	}
}

// 7.4a — restart over the threshold: the first Turn runs on a projection
// without waiting for a summariser.
func TestCompaction_TurnStartGuardAfterRestart(t *testing.T) {
	env := newLifecycleEnv(t)
	h := currentHistory(t, env.sessions, testCLIChannelName)

	for i := 0; i < 12; i++ {
		h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: testQuestion, Timestamp: time.Now()})
		h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: bigReply, Timestamp: time.Now()})
	}

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	env.useSmallWindow()

	var summariserCallsAtTurn atomic.Int64
	summariserCallsAtTurn.Store(-1)
	env.runner.set(func(r *scriptRunner) {
		r.reply = "short"
		r.beforeCall = func() { summariserCallsAtTurn.Store(int64(env.sum.callCount())) }
	})

	var logs bytes.Buffer
	env.runtime().logger = zerolog.New(&logs)

	env.run(cliMsg("after restart"))

	if got := summariserCallsAtTurn.Load(); got != 0 {
		t.Fatalf("the Turn must not wait for a summariser (calls before the Turn: %d)", got)
	}

	in := env.runner.inputs[0]

	questions := 0
	for _, m := range in {
		if m.Content == testQuestion {
			questions++
		}
	}

	if questions == 0 || questions >= 12 {
		t.Fatalf("expected a projection omitting the oldest Turns, prompt carries %d of 12", questions)
	}

	if !strings.Contains(logs.String(), "runs on a projection") {
		t.Errorf("expected a WARN for the projection: %s", logs.String())
	}

	// The projection never touches the transcript: every original Turn is
	// still in the file (a post-reply compaction may only append).
	data, err := os.ReadFile(env.cur().Path)
	if err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(string(data), `"content":"question"`); n != 12 {
		t.Errorf("the transcript must keep every Turn, found %d of 12", n)
	}
}

// ── 7.6: /compact ────────────────────────────────────────────────────────

func TestCompactCommand(t *testing.T) {
	env := newLifecycleEnv(t)
	env.run(cliMsg("/compact"))

	if !strings.Contains(env.buf.String(), msgNothingToCompact) {
		t.Fatalf("expected %q, got %q", msgNothingToCompact, env.buf.String())
	}

	env.runner.set(func(r *scriptRunner) { r.reply = bigReply })
	for i := 0; i < 3; i++ {
		env.run(cliMsg(testQuestion))
	}

	calls := env.runner.callCount()
	env.run(cliMsg("/compact keep the heating schedule decisions"))

	if env.runner.callCount() != calls {
		t.Fatal("/compact must not run a Turn")
	}

	if !strings.Contains(env.buf.String(), msgCompacted) {
		t.Fatalf("expected %q, got %q", msgCompacted, env.buf.String())
	}

	if in := env.sum.inputs[len(env.sum.inputs)-1]; !strings.Contains(in[1].Content, "keep the heating schedule decisions") {
		t.Errorf("the instructions must reach the summariser: %s", in[1].Content)
	}

	env.sum.err = errors.New("chain exhausted")
	before := len(env.jsonlRecords(t))

	env.runner.set(func(r *scriptRunner) { r.reply = bigReply })
	for i := 0; i < 3; i++ {
		env.run(cliMsg("more"))
	}

	recs := len(env.jsonlRecords(t))
	env.run(cliMsg("/compact"))

	if !strings.Contains(env.buf.String(), msgCompactionFailed) {
		t.Fatalf("expected %q", msgCompactionFailed)
	}

	if got := len(env.jsonlRecords(t)); got != recs || recs < before {
		t.Errorf("a failed /compact appends nothing (%d → %d)", recs, got)
	}
}

// ── 7.7a: summary message on a head-safe protocol ────────────────────────

func TestAssembleHeadSafe_SummaryThenToolGroupTail(t *testing.T) {
	prefix := withLead([]*schema.Message{schema.SystemMessage("static")}, "<summary>\nS\n</summary>")
	view := []persistence.HistoryMessage{
		{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{{ID: "c1", Name: "t", Arguments: "{}"}}},
		{Role: persistence.RoleTool, ToolCallID: "c1", Content: "ok"},
		{Role: persistence.RoleAssistant, Content: "done"},
		{Role: persistence.RoleUser, Content: "next"},
		{Role: persistence.RoleUser, Kind: persistence.KindTurnContext, Content: formatter.BuildTurnContext("abcd0001", formatter.TurnContextInput{Timestamp: "T"})},
	}

	got := assembleHeadSafe(prefix, view, "HEAD")

	roles := make([]schema.RoleType, 0, len(got))
	for _, m := range got {
		roles = append(roles, m.Role)
	}

	want := []schema.RoleType{schema.System, schema.System, schema.User, schema.Assistant, schema.Tool, schema.Assistant, schema.User}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}

	if !strings.HasPrefix(got[2].Content, "<summary>") {
		t.Errorf("the summary must be the first non-system message, got %q", got[2].Content)
	}
}

// ── 7.7b/7.8: skill pinning ──────────────────────────────────────────────

func writeSkill(t *testing.T, base, name, body string) {
	t.Helper()

	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	md := "---\nname: " + name + "\ndescription: \"" + name + " skill\"\nversion: \"1.0\"\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withSkills rebinds the env to a real skills tree containing the named
// skills (a fresh agentskills runtime, like a daemon restart).
func (e *lifecycleEnv) withSkills(t *testing.T, skillsDir string) {
	t.Helper()

	mgr, err := skills.NewManager(context.Background(), &config.SkillsConfig{Path: skillsDir}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	sc, err := skillcache.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	rt := e.runtime()
	rt.skillsMgr = mgr
	rt.skillCache = sc
	rt.bootstrapper = bootstrap.New(mgr, sc, map[string][]string{}, zerolog.Nop())
	rt.compactCfg.Skill = "home-compaction"
}

func TestCompaction_PinsLoadedSkillsAcrossRestart(t *testing.T) {
	skillsDir := t.TempDir()
	writeSkill(t, skillsDir, "home-provisioning", "PROVISIONING BODY")
	writeSkill(t, skillsDir, "home-compaction", "COMPACTION FORMAT")
	writeSkill(t, skillsDir, "other", "OTHER BODY")

	env := newLifecycleEnv(t)
	env.withSkills(t, skillsDir)
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply })

	env.run(cliMsg("q1"))

	rt := env.runtime()
	cur, h := env.sessions.History(context.Background(), testCLIChannelName)
	sid := rt.agentSkillsSession(context.Background(), cur, h, testCLIChannelName)

	// The Compaction skill is never offered by the catalog (7.8).
	meta, _ := rt.skillsMgr.GetMetadataPrompt(context.Background(), sid)
	if strings.Contains(meta, "home-compaction") {
		t.Error("the compaction skill must be hidden from the catalog")
	}

	if _, err := rt.skillsMgr.LoadSkill(context.Background(), sid, "home-provisioning"); err != nil {
		t.Fatal(err)
	}

	if _, err := rt.skillsMgr.LoadSkill(context.Background(), sid, "other"); err != nil {
		t.Fatal(err)
	}

	if err := rt.skillsMgr.UnloadSkill(context.Background(), sid, "other"); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		env.run(cliMsg("q"))
	}

	env.run(cliMsg("/compact"))

	if !strings.Contains(env.buf.String(), msgCompacted) {
		t.Fatalf("setup: expected a compaction, got %q", env.buf.String())
	}

	if sys := env.sum.inputs[0][0].Content; !strings.Contains(sys, "COMPACTION FORMAT") {
		t.Errorf("the summariser must use the Compaction skill as system prompt, got %q", sys)
	}

	_, preserved, _ := h.Summary()
	if len(preserved) != 1 || preserved[0] != "home-provisioning" {
		t.Fatalf("preserved skills = %v (the unloaded skill must not be pinned)", preserved)
	}

	env.run(cliMsg("after"))

	lead := ""
	for _, m := range env.runner.lastInput() {
		if strings.HasPrefix(m.Content, "<summary>") {
			lead = m.Content
		}
	}

	if !strings.Contains(lead, `<skill name="home-provisioning">`) || !strings.Contains(lead, "PROVISIONING BODY") {
		t.Fatalf("the summary message must carry the pinned SKILL.md verbatim: %q", lead)
	}

	// Restart: a fresh skills runtime and Session state.
	env2 := newLifecycleEnv(t)
	env2.sessionsDir = env.sessionsDir
	env2.sessions, env2.hist, env2.usageStore = testSessions(t, env.sessionsDir)
	env2.rt = nil
	env2.withSkills(t, skillsDir)

	cur2, h2 := env2.sessions.History(context.Background(), testCLIChannelName)
	sid2 := env2.runtime().agentSkillsSession(context.Background(), cur2, h2, testCLIChannelName)

	meta2, _ := env2.runtime().skillsMgr.GetMetadataPrompt(context.Background(), sid2)
	if strings.Contains(meta2, "home-provisioning") {
		t.Error("a pinned skill stays Active after a restart (not listed as available)")
	}

	if !strings.Contains(meta2, "other") {
		t.Error("an unpinned skill is listed as available")
	}
}

func TestCompaction_OversizedPinnedSkillsUnloaded(t *testing.T) {
	skillsDir := t.TempDir()
	writeSkill(t, skillsDir, "huge", strings.Repeat("x", 4000))

	env := newLifecycleEnv(t)
	env.withSkills(t, skillsDir)
	env.runtime().compactCfg.KeepRecentTokens = 200 // budget for pinned skills: 100 tokens
	env.runner.set(func(r *scriptRunner) { r.reply = bigReply })

	env.run(cliMsg("q"))

	rt := env.runtime()
	cur, h := env.sessions.History(context.Background(), testCLIChannelName)
	sid := rt.agentSkillsSession(context.Background(), cur, h, testCLIChannelName)

	if _, err := rt.skillsMgr.LoadSkill(context.Background(), sid, "huge"); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	rt.logger = zerolog.New(&logs)

	for i := 0; i < 3; i++ {
		env.run(cliMsg("q"))
	}

	env.run(cliMsg("/compact"))

	if _, preserved, _ := h.Summary(); len(preserved) != 0 {
		t.Errorf("preserved = %v, want the oversized skill unloaded", preserved)
	}

	if !strings.Contains(logs.String(), "oldest-loaded skill unloaded") {
		t.Errorf("expected a WARN, got %s", logs.String())
	}

	meta, _ := rt.skillsMgr.GetMetadataPrompt(context.Background(), sid)
	if !strings.Contains(meta, "huge") {
		t.Error("the unloaded skill is available again in the catalog")
	}
}

// connTermChannel is a connection-oriented fake: the Connection start hook is
// captured so the test can simulate a handshake.
type connTermChannel struct {
	*termZMQChannel

	hook channel.ConnectionStartHook
}

var _ channel.ConnectionOriented = (*connTermChannel)(nil)

func (c *connTermChannel) OnConnectionStart(h channel.ConnectionStartHook) { c.hook = h }

// 5.6 — a connection-oriented channel is never reset while its Connection is
// live; a reset found due at the next Connection start is applied silently
// before the next item.
func TestReset_ConnectionOrientedOnlyAtConnectionStart(t *testing.T) {
	setup := func(t *testing.T) (*lifecycleEnv, *connTermChannel) {
		t.Helper()

		ch := &connTermChannel{termZMQChannel: &termZMQChannel{}}
		env := newLifecycleEnv(t, ch)
		rt := env.runtime()
		rt.resets = map[string]session.Rule{testCLIChannelName: {Mode: config.ResetModeDaily, DailyAt: "04:00", Notify: true}}

		ch.OnConnectionStart(func(hctx context.Context, msg channel.Message) channel.ConnectionInfo {
			id, pending := rt.sessions.RequestReset(hctx, session.AddressOf(msg), rt.resetRule(testCLIChannelName), time.Now())

			return channel.ConnectionInfo{SessionID: id, ResetPending: pending}
		})

		return env, ch
	}

	t.Run("live connection keeps the session", func(t *testing.T) {
		env, _ := setup(t)
		old := seedOldTurn(t, env.sessions, testCLIChannelName, time.Now().Add(-30*time.Hour))

		env.run(cliMsg("still here"))

		if env.cur().ID != old.ID {
			t.Fatal("a live Connection must never be reset automatically")
		}
	})

	t.Run("reconnect after the boundary resets silently", func(t *testing.T) {
		env, ch := setup(t)
		old := seedOldTurn(t, env.sessions, testCLIChannelName, time.Now().Add(-30*time.Hour))

		info := ch.hook(context.Background(), channel.Message{ChannelName: testCLIChannelName})
		if !info.ResetPending || info.SessionID == old.ID {
			t.Fatalf("handshake info = %+v, want a pending reset", info)
		}

		if again := ch.hook(context.Background(), channel.Message{ChannelName: testCLIChannelName}); again.SessionID != info.SessionID {
			t.Errorf("a second handshake must return the same pending id")
		}

		env.run(cliMsg("after reconnect"))

		if env.cur().ID != info.SessionID {
			t.Errorf("current = %s, want the handshake's %s", env.cur().ID, info.SessionID)
		}

		if last := lastFileLine(t, old.Path); !strings.Contains(last, `"reason":"reset_daily"`) {
			t.Errorf("closing record = %s", last)
		}

		ch.mu.Lock()
		defer ch.mu.Unlock()

		for _, sent := range ch.sends {
			if sent == msgSessionReset {
				t.Error("a connection-oriented reset is silent")
			}
		}
	})
}

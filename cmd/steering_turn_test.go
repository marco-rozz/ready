package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/steer"
)

// recordingObserver captures turn-observer events.
type recordingObserver struct {
	mu       sync.Mutex
	started  []turnStartEvent
	injected []steerInjectedEvent
}

func (o *recordingObserver) TurnStarted(_ channel.Message, ev turnStartEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.started = append(o.started, ev)
}

func (o *recordingObserver) SteerInjected(_ channel.Message, ev steerInjectedEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.injected = append(o.injected, ev)
}

func humanMsg(text string) channel.Message {
	return channel.Message{ChannelName: testCLIChannelName, Content: text, Origin: channel.OriginHuman}
}

// steerEnv is a termEnv whose runtime has steering enabled and an inbox.
type steerEnv struct {
	*termEnv
	buf *termBuffer
	in  *steer.Inbox
	obs *recordingObserver
}

func steeringTestPolicy(mutate func(*config.SteeringConfig)) steeringPolicy {
	zero := time.Duration(0)
	cfg := config.SteeringConfig{MaxSteersPerTurn: 8, MaxRestartsPerTurn: 4, MinRemainingBudget: &zero}

	if mutate != nil {
		mutate(&cfg)
	}

	return steeringPolicy{cfg: cfg, channels: map[string]bool{testCLIChannelName: true}}
}

func newSteerEnv(t *testing.T, fmtCfg channelFormatterCfg, mutate func(*config.SteeringConfig)) *steerEnv {
	t.Helper()

	buf := &termBuffer{}
	env := newTermEnv(t, fmtCfg, newTestCLIChannel(buf))
	obs := &recordingObserver{}

	rt := env.runtime()
	rt.steering = steeringTestPolicy(mutate)
	rt.observer = obs

	return &steerEnv{
		termEnv: env,
		buf:     buf,
		in:      steer.New(isSessionCommandContent, steer.WithStopCommand()),
		obs:     obs,
	}
}

func (e *steerEnv) work(w steer.Work) {
	e.runtime().handleWork(context.Background(), e.in, w)
}

// admitTool is a tool that admits the given messages into the inbox when it
// executes — a message arriving while a tool call runs.
type admitTool struct {
	in   *steer.Inbox
	msgs []string
	runs int
	mu   sync.Mutex
}

func (a *admitTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: lampToolName, Desc: lampDesc}, nil
}

func (a *admitTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	a.mu.Lock()
	a.runs++
	a.mu.Unlock()

	for _, m := range a.msgs {
		a.in.Admit(channel.Message{ChannelName: testCLIChannelName, Content: m, Origin: channel.OriginHuman})
	}

	return lampOnJSON, ctx.Err()
}

func (a *admitTool) runCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.runs
}

func lastUserText(msgs []*schema.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.User {
			return msgs[i].Content
		}
	}

	return ""
}

func kindsOf(msgs []persistence.HistoryMessage) string {
	parts := make([]string, 0, len(msgs))

	for _, m := range msgs {
		switch {
		case m.Kind != "":
			parts = append(parts, string(m.Kind))
		default:
			parts = append(parts, string(m.Role))
		}
	}

	return strings.Join(parts, ",")
}

// 9.7 — after_tools: a message arriving while a tool call runs lets the call
// complete, then preempts before the next model call; the next assembly
// carries the steer after the completed group.
func TestSteerTurn_AfterToolsClaim(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	tl := &admitTool{in: env.in, msgs: []string{"no, the garage"}}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("garage it is"),
	}}
	env.useOrchestrator(t, m, tl, 0)

	env.work(steer.Work{Msg: humanMsg("turn on the kitchen light")})

	if tl.runCount() != 1 {
		t.Fatalf("tool runs = %d, want 1 (a running tool is never cancelled)", tl.runCount())
	}

	if m.callCount() != 2 {
		t.Fatalf("model calls = %d, want 2", m.callCount())
	}

	if got := lastUserText(m.input(1)); !strings.Contains(got, "<user_steer_") || !strings.Contains(got, "no, the garage") {
		t.Fatalf("second prompt must end with the steer envelope, got %q", got)
	}

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,assistant,tool,steer,assistant" {
		t.Fatalf("transcript order = %s", got)
	}

	if !strings.Contains(env.buf.String(), "garage it is") {
		t.Fatalf("reply not delivered: %q", env.buf.String())
	}

	if len(env.obs.injected) != 1 || len(env.obs.injected[0].Claimed) != 1 || env.obs.injected[0].Superseded != 0 {
		t.Fatalf("observer = %+v", env.obs.injected)
	}

	// 9.5: one Turn, one turn_context, one turn_usage.
	if n := strings.Count(kindsOf(env.jsonlAll(t)), "turn_usage"); n != 1 {
		t.Fatalf("turn_usage records = %d, want 1", n)
	}
}

// A candidate already pending at the first assembly (pre-loaded from a
// deferral) is in the very first prompt, after the turn-context line, and a
// batch is claimed in admission order with one assembly.
func TestSteerTurn_PreloadedBatchClaimedAtFirstAssembly(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("done")}}
	env.useOrchestrator(t, m, &lampTool{}, 0)

	now := time.Now()
	env.work(steer.Work{
		Msg:      humanMsg("A"),
		Deferred: true,
		Preload: []steer.Candidate{
			{Msg: humanMsg("B"), ReceivedAt: now},
			{Msg: humanMsg("C"), ReceivedAt: now},
		},
	})

	if m.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1 (single restart-free assembly)", m.callCount())
	}

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,steer,steer,assistant" {
		t.Fatalf("transcript = %s", got)
	}

	in := m.input(0)
	last := in[len(in)-1].Content
	prev := in[len(in)-2].Content

	if !strings.Contains(prev, "\nB\n") || !strings.Contains(last, "\nC\n") {
		t.Fatalf("steers not in admission order: %q / %q", prev, last)
	}

	if len(env.obs.started) != 1 || !env.obs.started[0].Deferred || len(env.obs.started[0].Messages) != 3 {
		t.Fatalf("turn started = %+v", env.obs.started)
	}

	if len(env.obs.injected) != 0 {
		t.Fatalf("deferred candidates are announced by turn_started, not steer_injected: %+v", env.obs.injected)
	}
}

// Limits defer without errors: beyond max_steers_per_turn the extra
// candidate opens the next Turn.
func TestSteerTurn_MaxStepsDefersTheRest(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, func(c *config.SteeringConfig) { c.MaxSteersPerTurn = 2 })

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("done")}}
	env.useOrchestrator(t, m, &lampTool{}, 0)

	now := time.Now()
	env.work(steer.Work{
		Msg:      humanMsg("A"),
		Deferred: true,
		Preload:  []steer.Candidate{{Msg: humanMsg("B"), ReceivedAt: now}, {Msg: humanMsg("C"), ReceivedAt: now}, {Msg: humanMsg("D"), ReceivedAt: now}},
	})

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,steer,steer,assistant" {
		t.Fatalf("transcript = %s, want exactly two steers claimed (cap 2)", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	next, err := env.in.Next(ctx)
	if err != nil || next.Msg.Content != "D" || !next.Deferred {
		t.Fatalf("deferred = %+v err=%v, want D deferred to the next Turn", next, err)
	}
}

// min_remaining_budget: no claim when less than the budget remains of the
// Turn's deadline; the candidate defers.
func TestSteerTurn_MinRemainingBudgetDefers(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, func(c *config.SteeringConfig) {
		big := 2 * agentRunTimeout
		c.MinRemainingBudget = &big
	})

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("done")}}
	env.useOrchestrator(t, m, &lampTool{}, 0)

	env.work(steer.Work{Msg: humanMsg("A"), Deferred: true, Preload: []steer.Candidate{{Msg: humanMsg("B"), ReceivedAt: time.Now()}}})

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,assistant" {
		t.Fatalf("transcript = %s, want no steer (budget too low)", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if next, err := env.in.Next(ctx); err != nil || next.Msg.Content != "B" {
		t.Fatalf("deferred = %+v err=%v", next, err)
	}
}

// stubExec is a scripted turnExecutor.
type stubExec struct {
	calls   int
	fn      func(n int, assemble func(llm.PromptLayout, string) []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error)
	prompts [][]*schema.Message
}

func (s *stubExec) ExecuteTurn(_ context.Context, _ string, assemble func(llm.PromptLayout, string) []*schema.Message, _ func() bool, onChunk func(string)) (agent.TurnOutcome, error) {
	s.calls++

	return s.fn(s.calls, assemble, onChunk)
}

func (s *stubExec) ExecuteTurnRequest(context.Context, orchestrator.TurnRequest, func(string)) (agent.TurnOutcome, error) {
	return agent.TurnOutcome{}, nil
}

func (e *steerEnv) useStub(s *stubExec) {
	e.agentReg = &agentRegistry{exec: s, layout: llm.LayoutTailDynamic}
	e.runtime().agentReg = e.agentReg
}

// 9.4 — a preemption at before_tools closes the proposed group by append with
// the superseded results (counted), then the restart's assembly carries them
// followed by the steer.
func TestSteerTurn_BeforeToolsSupersededThenSteer(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	stub := &stubExec{}
	stub.fn = func(n int, assemble func(llm.PromptLayout, string) []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
		if n == 1 {
			// The attempt recorded the assistant message with two proposed
			// calls (what the transcript sink does) and was preempted before
			// any executed.
			h := currentHistory(t, env.sessions, testCLIChannelName)
			h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{
				{ID: "c1", Name: lampToolName, Arguments: `{}`}, {ID: "c2", Name: "cover_close", Arguments: `{}`},
			}, Timestamp: time.Now()})

			env.in.Admit(humanMsg("no, upstairs"))

			return agent.TurnOutcome{ToolCallStarted: true}, &agent.PreemptError{Reason: agent.PreemptSteer, Point: agent.PointBeforeTools}
		}

		stub.prompts = append(stub.prompts, assemble(llm.LayoutTailDynamic, ""))
		onChunk("fixed")

		return agent.TurnOutcome{Reply: "fixed", ContentDelivered: true}, nil
	}
	env.useStub(stub)

	env.work(steer.Work{Msg: humanMsg("lights and shutters")})

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,assistant,tool,tool,steer,assistant" {
		t.Fatalf("transcript = %s", got)
	}

	lines := env.jsonlLines(t)
	for _, i := range []int{3, 4} {
		if lines[i].Content != persistence.SupersededToolResult {
			t.Errorf("line %d = %q, want the superseded result", i, lines[i].Content)
		}
	}

	if len(env.obs.injected) != 1 || env.obs.injected[0].Superseded != 2 {
		t.Fatalf("observer = %+v, want 2 superseded", env.obs.injected)
	}
}

// 9.7 — final_undelivered: on a post-step channel (no streaming) a complete
// but undelivered answer is discarded unrecorded and the Turn re-runs with
// the steer.
func TestSteerTurn_FinalUndeliveredIsDiscardedAndRerun(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{skillName: fmtSkillName, postStep: true}, nil)

	stub := &stubExec{}
	stub.fn = func(n int, assemble func(llm.PromptLayout, string) []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
		if onChunk != nil {
			t.Error("post-step channels must not stream")
		}

		if n == 1 {
			env.in.Admit(humanMsg("also the garage"))

			return agent.TurnOutcome{Reply: "stale answer", ContentDelivered: true}, nil
		}

		stub.prompts = append(stub.prompts, assemble(llm.LayoutTailDynamic, ""))

		return agent.TurnOutcome{Reply: "fresh answer", ContentDelivered: true}, nil
	}
	env.useStub(stub)

	env.work(steer.Work{Msg: humanMsg("lights")})

	if stub.calls != 2 {
		t.Fatalf("executor calls = %d, want 2", stub.calls)
	}

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,steer,assistant" {
		t.Fatalf("transcript = %s", got)
	}

	for _, l := range env.jsonlLines(t) {
		if strings.Contains(l.Content, "stale answer") {
			t.Fatal("the discarded answer must never be recorded")
		}
	}
}

// A streaming channel never interrupts a delivered answer: the candidate is
// deferred and opens the next Turn.
func TestSteerTurn_StreamingFinalIsNeverInterrupted(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	stub := &stubExec{}
	stub.fn = func(_ int, _ func(llm.PromptLayout, string) []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
		onChunk("the answer")
		env.in.Admit(humanMsg("late message"))

		return agent.TurnOutcome{Reply: "the answer", ContentDelivered: true}, nil
	}
	env.useStub(stub)

	env.work(steer.Work{Msg: humanMsg("question")})

	if stub.calls != 1 {
		t.Fatalf("executor calls = %d, want 1 (no re-run after delivery)", stub.calls)
	}

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,assistant" {
		t.Fatalf("transcript = %s", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	next, err := env.in.Next(ctx)
	if err != nil || next.Msg.Content != "late message" || !next.Deferred {
		t.Fatalf("deferred = %+v err=%v", next, err)
	}
}

// 10 — /stop during a running tool: the tool completes and its result is
// recorded, no further model call, the Turn ends with the fixed reply and a
// turn_stopped marker.
func TestSteerTurn_StopDuringTool(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	tl := &admitTool{in: env.in, msgs: []string{stopCommand}}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("must not be reached"),
	}}
	env.useOrchestrator(t, m, tl, 0)

	env.work(steer.Work{Msg: humanMsg("turn on the light")})

	if m.callCount() != 1 || tl.runCount() != 1 {
		t.Fatalf("model calls = %d tool runs = %d, want 1/1", m.callCount(), tl.runCount())
	}

	if got := kindsOf(env.jsonlLines(t)); got != kindsStopped {
		t.Fatalf("transcript = %s", got)
	}

	if !strings.Contains(env.buf.String(), msgStopped) {
		t.Fatalf("stopped reply missing: %q", env.buf.String())
	}

	// The next Turn's projection carries turn_stopped before the new user
	// message.
	m2 := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("ok")}}
	env.useOrchestrator(t, m2, &lampTool{}, 0)
	env.work(steer.Work{Msg: humanMsg(nextOneText)})

	in := m2.input(0)
	stoppedAt, nextAt := -1, -1

	for i, msg := range in {
		if msg.Role == schema.User && msg.Content == persistence.TurnStoppedContent {
			stoppedAt = i
		}

		if msg.Role == schema.User && msg.Content == nextOneText {
			nextAt = i
		}
	}

	if stoppedAt < 0 || nextAt < 0 || stoppedAt > nextAt {
		t.Fatalf("turn_stopped at %d, next at %d: stopped must precede the new opener", stoppedAt, nextAt)
	}
}

// 9.8 — with steering enabled and no mid-turn message, the Session file and
// every assembled prompt are byte-identical to the pre-steering baseline.
func TestGolden_NoSteerThroughSteeringPath(t *testing.T) {
	got := goldenScenario(t, func(env *termEnv, msg string) {
		env.runtime().steering = steeringTestPolicy(nil)
		in := steer.New(isSessionCommandContent, steer.WithStopCommand())
		env.runtime().handleWork(context.Background(), in, steer.Work{Msg: humanMsg(msg)})
	})

	checkGolden(t, goldenFile, got)
}

// blockingTool blocks its execution until released and reports when it started.
type blockingTool struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: lampToolName, Desc: lampDesc}, nil
}

func (b *blockingTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release

	return lampOnJSON, ctx.Err()
}

// stepDispatcher builds a real dispatcher over the env's runtime, with the
// steering handlers wired like production.
func (e *steerEnv) dispatcher() *dispatcher {
	return &dispatcher{
		ctx:       context.Background(),
		incoming:  make(chan channel.Message),
		keyFn:     func(channel.Message) string { return testCLIChannelName },
		isCommand: isSessionCommandContent,
		logger:    e.logger,
		workers:   map[string]*steer.Inbox{},
		handle:    e.runtime().handleWork,
		stopFn:    e.runtime().handleStop,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// 10.1/10.2/10.4 — /stop through the real dispatcher while a tool runs: the
// tool completes and is recorded, candidates sent meanwhile are dropped and
// reported, no further model call, and a queued /compact does not overtake it.
func TestStop_ThroughDispatcherDuringRunningTool(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	bt := &blockingTool{started: make(chan struct{}), release: make(chan struct{})}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("must not be reached"),
	}}
	env.useOrchestrator(t, m, bt, 0)

	d := env.dispatcher()
	d.enqueue(humanMsg("turn on the light"))
	<-bt.started

	d.enqueue(humanMsg("and the garage")) // a candidate that /stop will drop
	d.enqueue(humanMsg(stopCommand))
	d.enqueue(humanMsg("/compact")) // queued behind the Turn: /stop overtakes it

	close(bt.release)

	waitFor(t, "the stopped reply", func() bool { return strings.Contains(env.buf.String(), msgStopped) })

	out := env.buf.String()
	if !strings.Contains(out, "1 message(s) sent meanwhile were not processed") {
		t.Fatalf("dropped candidates not reported: %q", out)
	}

	waitFor(t, "the Turn to finish", func() bool {
		return strings.Contains(kindsOf(env.jsonlLines(t)), "turn_stopped")
	})

	if m.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1", m.callCount())
	}

	if got := kindsOf(env.jsonlLines(t)); got != kindsStopped {
		t.Fatalf("transcript = %s", got)
	}

	for _, l := range env.jsonlLines(t) {
		if strings.Contains(l.Content, "and the garage") {
			t.Fatal("a dropped candidate must never reach the transcript")
		}
	}
}

// /stop with nothing to stop answers with the fixed text and writes nothing.
func TestStop_NothingToStopWhenIdle(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	d := env.dispatcher()
	d.enqueue(humanMsg(stopCommand))

	waitFor(t, "the nothing-to-stop reply", func() bool { return strings.Contains(env.buf.String(), msgNothingToStop) })

	if cur := env.sessions.Current(context.Background(), testCLIChannelName); cur != nil && countFiles(t, env.sessionsDir) != 0 {
		t.Fatal("nothing to stop must write nothing")
	}
}

// 2.4 — the limits are read per Turn from the generation's policy: a reload
// (a new policy installed under the generation lock) applies from the next
// Turn, never to the one already open.
func TestSteeringLimits_ReadPerTurnReloadAppliesToNextTurn(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)
	rt := env.runtime()

	first := rt.openTurn(env.in, steer.Work{Msg: humanMsg(oneText)})
	first.close(env.logger)

	rt.steering = steeringTestPolicy(func(c *config.SteeringConfig) { c.MaxSteersPerTurn = 1; c.MaxRestartsPerTurn = 2 })

	second := rt.openTurn(env.in, steer.Work{Msg: humanMsg("two")})
	defer second.close(env.logger)

	// The second Turn absorbs one candidate (new cap), the first absorbed up
	// to 8 (old cap) — pinned through admission.
	if a := env.in.Admit(humanMsg("a")); a.Disposition != steer.Steer {
		t.Fatalf("first candidate = %s", a.Disposition)
	}

	if a := env.in.Admit(humanMsg("b")); a.Disposition != steer.Queued {
		t.Fatalf("second candidate = %s, want queued (new cap 1)", a.Disposition)
	}

	if second.maxRestarts != 2 {
		t.Fatalf("restart cap = %d, want 2 from the new generation", second.maxRestarts)
	}
}

// /stop also stops a harness-opened Turn (it is the user's address), though
// such a Turn is never steerable.
func TestStop_StopsAHarnessTurn(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	tl := &admitTool{in: env.in, msgs: []string{stopCommand}}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("must not be reached"),
	}}
	env.useOrchestrator(t, m, tl, 0)

	env.work(steer.Work{Msg: channel.Message{ChannelName: testCLIChannelName, Content: "alert: door open", Origin: channel.OriginHarness}})

	if got := kindsOf(env.jsonlLines(t)); got != kindsStopped {
		t.Fatalf("transcript = %s", got)
	}

	if m.callCount() != 1 {
		t.Fatalf("model calls = %d, want 1", m.callCount())
	}
}

// A harness Turn is not steerable: a human message during it is queued.
func TestSteerTurn_HarnessTurnQueuesHumanMessage(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	tl := &admitTool{in: env.in, msgs: []string{"hello?"}}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("alert handled"),
	}}
	env.useOrchestrator(t, m, tl, 0)

	env.work(steer.Work{Msg: channel.Message{ChannelName: testCLIChannelName, Content: "alert", Origin: channel.OriginHarness}})

	if got := kindsOf(env.jsonlLines(t)); got != kindsToolAnswer {
		t.Fatalf("transcript = %s, want no steer on a harness Turn", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if next, err := env.in.Next(ctx); err != nil || next.Msg.Content != "hello?" || next.Deferred {
		t.Fatalf("queued = %+v err=%v, want an ordinary queued message", next, err)
	}
}

// /stop at final_undelivered: the complete answer is discarded and the Turn
// ends stopped.
func TestStop_AtFinalUndelivered(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{skillName: fmtSkillName, postStep: true}, nil)

	stub := &stubExec{}
	stub.fn = func(int, func(llm.PromptLayout, string) []*schema.Message, func(string)) (agent.TurnOutcome, error) {
		env.in.Admit(humanMsg(stopCommand))

		return agent.TurnOutcome{Reply: "stale", ContentDelivered: true}, nil
	}
	env.useStub(stub)

	env.work(steer.Work{Msg: humanMsg("question")})

	if stub.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", stub.calls)
	}

	if got := kindsOf(env.jsonlLines(t)); got != "user,turn_context,turn_stopped" {
		t.Fatalf("transcript = %s", got)
	}

	if !strings.Contains(env.buf.String(), msgStopped) || strings.Contains(env.buf.String(), "stale") {
		t.Fatalf("output = %q", env.buf.String())
	}
}

// 11.3 — a claim changes the view, so the Turn-start guard is re-evaluated on
// it: a steer that pushes the prompt over the compaction threshold makes the
// attempt run on the projection omitting the oldest Turns (the transcript is
// untouched).
func TestSteerTurn_ClaimReevaluatesTheTurnStartGuard(t *testing.T) {
	env := newSteerEnv(t, channelFormatterCfg{}, nil)

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("ok")}}
	env.useOrchestrator(t, m, &lampTool{}, 0)

	// Three small prior Turns.
	for _, q := range []string{oneText, "two", "three"} {
		env.work(steer.Work{Msg: humanMsg(q)})
	}

	rt := env.runtime()
	calls := m.callCount()

	// A threshold the next Turn fits under until the steer arrives.
	rt.trigger = compaction.Trigger{Window: 6000, Reserve: 0, MaxTurns: 1000}

	huge := strings.Repeat("z", 40000) // ~10k tokens
	env.work(steer.Work{Msg: humanMsg("four"), Deferred: true, Preload: []steer.Candidate{{Msg: humanMsg(huge), ReceivedAt: time.Now()}}})

	in := m.input(calls)

	var hasOne bool

	for _, msg := range in {
		if msg.Role == schema.User && msg.Content == oneText {
			hasOne = true
		}
	}

	if hasOne {
		t.Fatal("the oldest Turn must be omitted from the projection once the steer pushed the prompt over the threshold")
	}

	// The transcript itself is unchanged: the oldest Turn is still on disk.
	var onDisk bool

	for _, l := range env.jsonlLines(t) {
		if l.Role == persistence.RoleUser && l.Content == oneText {
			onDisk = true
		}
	}

	if !onDisk {
		t.Fatal("the projection must never touch the Session file")
	}
}

// recordingChannel is a "cli" channel that records what Send delivers.
type recordingChannel struct {
	mu   sync.Mutex
	sent []channel.Message
}

func (r *recordingChannel) Name() string { return testCLIChannelName }
func (r *recordingChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}

func (r *recordingChannel) Send(_ context.Context, m channel.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sent = append(r.sent, m)

	return nil
}
func (r *recordingChannel) Close() error { return nil }

func (r *recordingChannel) last() channel.Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.sent[len(r.sent)-1]
}

func zmqMsg(text, req string) channel.Message {
	return channel.Message{
		ChannelName: testCLIChannelName, Content: text, Origin: channel.OriginHuman,
		Metadata: map[string]string{metaTransport: transportZMQ, "request_type": "message", zmqcli.ConnectionID: testConn, zmqcli.RequestID: req},
	}
}

// 12.5 (daemon side) — the final reply keeps the opener's request id and
// carries the Turn's id and every consumed request id in consumption order;
// a stopped Turn adds the /stop request and lists the dropped candidates.
func TestFinalReplyMetadata_ConsumedAndStoppedSets(t *testing.T) {
	rec := &recordingChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, rec)
	env.runtime().steering = steeringTestPolicy(nil)
	in := steer.New(isSessionCommandContent, steer.WithStopCommand())

	// Consumed: opener r1 plus a pre-loaded candidate r2.
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){finalAnswer("both")}}
	env.useOrchestrator(t, m, &lampTool{}, 0)

	env.runtime().handleWork(context.Background(), in, steer.Work{
		Msg:      zmqMsg("first", "r1"),
		Deferred: true,
		Preload:  []steer.Candidate{{Msg: zmqMsg("second", "r2"), ReceivedAt: time.Now()}},
	})

	meta := rec.last().Metadata
	if meta[zmqcli.RequestID] != "r1" || meta[zmqcli.MetaTurnID] == "" || meta[zmqcli.MetaTurnRequestIDs] != zmqcli.EncodeIDs([]string{"r1", "r2"}) {
		t.Fatalf("reply metadata = %v", meta)
	}

	if meta[zmqcli.MetaTurnDroppedIDs] != "" {
		t.Fatal("no dropped ids on a completed Turn")
	}

	// Stopped: /stop (r4) arrives while the Turn runs with candidate r3 pending.
	m2 := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("unreachable"),
	}}

	stopTool := &funcTool{fn: func() {
		in.Admit(zmqMsg("extra", "r3"))
		in.Admit(zmqMsg(stopCommand, "r4"))
	}}
	env.useOrchestrator(t, m2, stopTool, 0)

	env.runtime().handleWork(context.Background(), in, steer.Work{Msg: zmqMsg("go", "r5")})

	meta = rec.last().Metadata
	if meta[zmqcli.RequestID] != "r5" ||
		meta[zmqcli.MetaTurnRequestIDs] != zmqcli.EncodeIDs([]string{"r5", "r4"}) ||
		meta[zmqcli.MetaTurnDroppedIDs] != zmqcli.EncodeIDs([]string{"r3"}) {
		t.Fatalf("stopped reply metadata = %v", meta)
	}

	if rec.last().Content != stoppedReply(1) {
		t.Fatalf("content = %q", rec.last().Content)
	}
}

// funcTool runs fn when executed.
type funcTool struct{ fn func() }

func (f *funcTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: lampToolName, Desc: lampDesc}, nil
}

func (f *funcTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	f.fn()

	return lampOnJSON, ctx.Err()
}

// steering.enabled: false (or the channel override) restores plain FIFO
// queueing: a message sent during a Turn never steers it and runs next.
func TestSteerTurn_DisabledRestoresQueueing(t *testing.T) {
	off := false
	env := newSteerEnv(t, channelFormatterCfg{}, func(c *config.SteeringConfig) { c.Enabled = &off })
	env.runtime().steering.channels = map[string]bool{testCLIChannelName: false}

	tl := &admitTool{in: env.in, msgs: []string{"a correction"}}
	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("first answer"),
	}}
	env.useOrchestrator(t, m, tl, 0)

	env.work(steer.Work{Msg: humanMsg("first request")})

	if got := kindsOf(env.jsonlLines(t)); got != kindsToolAnswer {
		t.Fatalf("transcript = %s, want no steer when steering is disabled", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	next, err := env.in.Next(ctx)
	if err != nil || next.Msg.Content != "a correction" || next.Deferred {
		t.Fatalf("next = %+v err=%v, want the message queued as an ordinary next item", next, err)
	}
}

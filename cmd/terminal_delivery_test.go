package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/formatter"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/skillcache"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/usage"
	"github.com/rs/zerolog"
)

// This file exercises the llm-outage-delivery dispatcher contract end to end
// through processMessage: terminal failure delivery per channel, terminal
// selection from the outcome metadata, post-step formatter bypass, the
// agent-run-deadline context rule (design A4), and auxiliary-consumer
// degradation. Channels are fakes implementing the streaming surfaces
// (streamWriter / progressNotifier / telegramStreamSender) that
// buildStreamingCallback matches by capability.

// Shared literals for the terminal-delivery tests (goconst: repeated
// channel name / payload strings become named constants).
const (
	testCLIChannelName = "cli" // zmqcli shares the logical channel name "cli"
	testPartialAnswer  = "partial answer"
	testProgressChunk  = "progress chunk"
)

// termRunner implements agent.Runner with a scripted outcome, optionally
// forwarding a chunk to onChunk (simulating streamed partial delivery) or
// blocking until the context is cancelled (simulating the agent-run
// deadline).
type termRunner struct {
	outcome        agent.TurnOutcome
	err            error
	deliverChunk   string // forwarded to onChunk before returning, when non-empty
	blockUntilDone bool   // wait for ctx cancellation, then return ctx.Err()

	mu            sync.Mutex
	sawSessionKey string
	sawOnChunk    bool
	sawCtxErrNil  bool
}

var _ agent.Runner = (*termRunner)(nil)

func (f *termRunner) RunMessagesStreaming(ctx context.Context, sessionKey string, _ []*schema.Message, onChunk func(string)) (agent.TurnOutcome, error) {
	f.mu.Lock()
	f.sawSessionKey = sessionKey
	f.sawOnChunk = onChunk != nil
	f.sawCtxErrNil = ctx.Err() == nil
	f.mu.Unlock()

	if f.blockUntilDone {
		<-ctx.Done()

		return agent.TurnOutcome{}, ctx.Err()
	}

	if f.deliverChunk != "" && onChunk != nil {
		onChunk(f.deliverChunk)
	}

	return f.outcome, f.err
}

// termBaseChatModel implements model.BaseChatModel for the auxiliary LLM
// consumers (post-step formatter, compaction summariser), recording every
// Generate call so tests can assert the formatter was bypassed.
type termBaseChatModel struct {
	generateFunc func(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
	calls        atomic.Int32
}

var _ model.BaseChatModel = (*termBaseChatModel)(nil)

func (m *termBaseChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.calls.Add(1)

	if m.generateFunc != nil {
		return m.generateFunc(ctx, input, opts...)
	}

	return &schema.Message{Role: schema.Assistant, Content: "formatted reply"}, nil
}

func (m *termBaseChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

// termSkillResolver implements formatter.PathResolver.
type termSkillResolver struct{}

func (termSkillResolver) SkillFilePath(skillName string) (string, error) {
	return "/fake/skills/" + skillName + "/SKILL.md", nil
}

// termSkillLoader implements formatter.ContentLoader with fixed content.
type termSkillLoader struct{ content string }

func (l termSkillLoader) Load(_ context.Context, _ string) (string, error) {
	return l.content, nil
}

// termTGChannel implements channel.Channel plus the telegramStreamSender
// surface, recording every delivery with the context error observed at call
// time (to assert the A4 message-scoped-context rule).
type termTGChannel struct {
	mu sync.Mutex

	sent        []string // Channel.Send deliveries
	streamMsgs  []string // SendStreamMessage deliveries
	edits       []string // SendStreamEdit deliveries
	callCtxErrs []error  // ctx.Err() observed per stream call, in order
}

var (
	_ channel.Channel      = (*termTGChannel)(nil)
	_ telegramStreamSender = (*termTGChannel)(nil)
)

func (c *termTGChannel) Name() string { return telegram.ChannelName }

func (c *termTGChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}

func (c *termTGChannel) Send(_ context.Context, msg channel.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, msg.Content)

	return nil
}

func (c *termTGChannel) Close() error { return nil }

func (c *termTGChannel) SendStreamMessage(ctx context.Context, _ int64, content string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamMsgs = append(c.streamMsgs, content)
	c.callCtxErrs = append(c.callCtxErrs, ctx.Err())

	return len(c.streamMsgs), nil
}

func (c *termTGChannel) SendStreamEdit(ctx context.Context, _ int64, _ int, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, content)
	c.callCtxErrs = append(c.callCtxErrs, ctx.Err())

	return nil
}

// termZMQChannel implements channel.Channel plus the progressNotifier
// surface, mirroring the ZMQ channel's contract: SendNotification never
// consumes the pending reply slot; Send delivers exactly one final reply.
type termZMQChannel struct {
	mu            sync.Mutex
	sends         []string
	sendErrCodes  []string // error_code metadata observed per Send (llm-status-visibility)
	notifications []string
	notifyCtxErr  error
}

var (
	_ channel.Channel  = (*termZMQChannel)(nil)
	_ progressNotifier = (*termZMQChannel)(nil)
)

func (c *termZMQChannel) Name() string { return testCLIChannelName } // zmqcli shares the logical channel name

func (c *termZMQChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}

func (c *termZMQChannel) Send(_ context.Context, msg channel.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, msg.Content)
	c.sendErrCodes = append(c.sendErrCodes, msg.Metadata[zmqcli.ErrorCode])

	return nil
}

func (c *termZMQChannel) Close() error { return nil }

func (c *termZMQChannel) SendNotification(ctx context.Context, _, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notifications = append(c.notifications, content)
	c.notifyCtxErr = ctx.Err()

	return nil
}

// termEnv wires every processMessage dependency with lightweight fakes and
// temp-dir stores.
type termEnv struct {
	mgr          *channel.Manager
	runner       *termRunner
	agentReg     *agentRegistry
	fmtr         *formatter.Formatter
	fmtModel     *termBaseChatModel
	reg          formatterRegistry
	bootstrapper *bootstrap.Bootstrapper
	skillsMgr    *skills.Manager
	hist         *historyStore
	sessions     *session.Manager
	sessionsDir  string
	usageStore   *usage.Store
	sanitizer    *privacy.Sanitizer
	logger       zerolog.Logger
	rt           *sessionRuntime
}

func newTermEnv(t *testing.T, fmtCfg channelFormatterCfg, channels ...channel.Channel) *termEnv {
	t.Helper()

	ctx := context.Background()
	logger := zerolog.New(io.Discard)

	mgr := channel.NewManager(logger, channels...)

	runner := &termRunner{}
	agentReg := &agentRegistry{exec: &plainRunnerExec{runner: runner, layout: llm.LayoutTailDynamic}, layout: llm.LayoutTailDynamic}

	skillsMgr, err := skills.NewManager(ctx, &config.SkillsConfig{
		Path:      t.TempDir(),
		HotReload: false,
	}, logger)
	if err != nil {
		t.Fatalf("skills.NewManager() error: %v", err)
	}

	sc, err := skillcache.New()
	if err != nil {
		t.Fatalf("skillcache.New() error: %v", err)
	}

	bootstrapper := bootstrap.New(skillsMgr, sc, map[string][]string{}, logger)

	sessionsDir := t.TempDir()
	sessions, hist, usageStore := testSessions(t, sessionsDir)

	fmtModel := &termBaseChatModel{}
	fmtr := formatter.New(fmtModel, termSkillResolver{}, termSkillLoader{content: "# formatter rules"})

	reg := formatterRegistry{}
	for _, ch := range channels {
		reg[ch.Name()] = fmtCfg
	}

	return &termEnv{
		mgr:          mgr,
		runner:       runner,
		agentReg:     agentReg,
		fmtr:         fmtr,
		fmtModel:     fmtModel,
		reg:          reg,
		bootstrapper: bootstrapper,
		skillsMgr:    skillsMgr,
		hist:         hist,
		sessions:     sessions,
		sessionsDir:  sessionsDir,
		usageStore:   usageStore,
		sanitizer:    privacy.FromHome(nil),
		logger:       logger,
	}
}

// runtime builds (once) the worker runtime over the env's wiring; the
// agentReg is read at build time, so swap it (useOrchestrator) before the
// first run.
func (e *termEnv) runtime() *sessionRuntime {
	if e.rt == nil {
		e.rt = &sessionRuntime{
			mgr:          e.mgr,
			agentReg:     e.agentReg,
			fmtr:         e.fmtr,
			reg:          e.reg,
			bootstrapper: e.bootstrapper,
			skillsMgr:    e.skillsMgr,
			skillCache:   termSkillLoader{content: "# skill"},
			hist:         e.hist,
			sessions:     e.sessions,
			usageStore:   e.usageStore,
			sanitizer:    e.sanitizer,
			logger:       e.logger,
		}
	}

	return e.rt
}

// run processes one item through the address worker's handler.
func (e *termEnv) run(msg channel.Message) {
	e.runtime().handleItem(context.Background(), msg)
}

// TestTerminalMessageFor covers the two-outcome selection table (design A2):
// the discriminator is observable side effects, never the error class.
func TestTerminalMessageFor(t *testing.T) {
	cases := []struct {
		name    string
		outcome agent.TurnOutcome
		want    string
	}{
		{"nothing ran", agent.TurnOutcome{}, msgLLMUnavailable},
		{"tool started, result lost", agent.TurnOutcome{ToolCallStarted: true}, msgIncompleteResponse},
		{"tool completed, no content", agent.TurnOutcome{ToolCallCompleted: true}, msgIncompleteResponse},
		{"content delivered, no tool", agent.TurnOutcome{ContentDelivered: true}, msgIncompleteResponse},
		{"tool completed and content delivered", agent.TurnOutcome{ToolCallCompleted: true, ContentDelivered: true}, msgIncompleteResponse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := terminalMessageFor(tc.outcome); got != tc.want {
				t.Errorf("terminalMessageFor(%+v) = %q, want %q", tc.outcome, got, tc.want)
			}
		})
	}
}

// TestProcessMessage_TerminalDelivery_CLI_NoOutput fixes the CLI defect
// (task 3.1): a failed turn with no streamed chunk must print the
// temporary-unavailability message followed by a newline — not a bare
// newline — and must print it exactly once.
func TestProcessMessage_TerminalDelivery_CLI_NoOutput(t *testing.T) {
	var buf termBuffer

	cliCh := newTestCLIChannel(&buf)
	env := newTermEnv(t, channelFormatterCfg{}, cliCh)

	env.runner.err = errors.New("connection refused")
	env.runner.outcome = agent.TurnOutcome{}

	env.run(channel.Message{Content: "hi", ChannelName: testCLIChannelName})

	out := buf.String()
	if got := strings.Count(out, msgLLMUnavailable); got != 1 {
		t.Fatalf("expected exactly one terminal message, got %d in %q", got, out)
	}

	if strings.TrimSpace(out) != msgLLMUnavailable {
		t.Errorf("expected terminal message + newline, got %q", out)
	}
}

// TestProcessMessage_TerminalDelivery_CLI_PartialThenFailure covers the
// partial-stream-then-failure combination on CLI (tasks 2.3/3.1): the
// partial output stays on screen and the incomplete-response message is
// appended after a separating newline, never replacing the partial.
func TestProcessMessage_TerminalDelivery_CLI_PartialThenFailure(t *testing.T) {
	var buf termBuffer

	cliCh := newTestCLIChannel(&buf)
	env := newTermEnv(t, channelFormatterCfg{}, cliCh)

	env.runner.deliverChunk = testPartialAnswer
	env.runner.outcome = agent.TurnOutcome{Reply: testPartialAnswer, ContentDelivered: true}
	env.runner.err = errors.New("stream boom")

	env.run(channel.Message{Content: "hi", ChannelName: testCLIChannelName})

	out := buf.String()
	if !strings.HasPrefix(out, testPartialAnswer) {
		t.Errorf("expected partial output retained at the start, got %q", out)
	}

	if got := strings.Count(out, msgIncompleteResponse); got != 1 {
		t.Fatalf("expected exactly one incomplete-response message, got %d in %q", got, out)
	}

	if !strings.Contains(out, "partial answer\n"+msgIncompleteResponse) {
		t.Errorf("expected partial + separator + notice, got %q", out)
	}
}

// TestProcessMessage_TerminalDelivery_CLI_SuccessUnchanged guards the
// success path: streamed chunks plus a trailing newline, no terminal
// message, exactly-once delivery.
func TestProcessMessage_TerminalDelivery_CLI_SuccessUnchanged(t *testing.T) {
	var buf termBuffer

	cliCh := newTestCLIChannel(&buf)
	env := newTermEnv(t, channelFormatterCfg{}, cliCh)

	env.runner.deliverChunk = "all good"
	env.runner.outcome = agent.TurnOutcome{Reply: "all good", ContentDelivered: true}

	env.run(channel.Message{Content: "hi", ChannelName: testCLIChannelName})

	if out := buf.String(); out != "all good\n" {
		t.Errorf("expected streamed content + newline, got %q", out)
	}
}

// TestProcessMessage_TerminalDelivery_Telegram_NoStreamMessage fixes the
// Telegram defect (a failure before the first chunk must not be silent): when no stream message exists, the channel creates
// exactly one new message carrying the temporary-unavailability text.
func TestProcessMessage_TerminalDelivery_Telegram_NoStreamMessage(t *testing.T) {
	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, tg)

	env.runner.err = errors.New("connection refused")
	env.runner.outcome = agent.TurnOutcome{}

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: telegram.ChannelName,
		Metadata:    map[string]string{telegram.MetadataChatID: "4242"},
	})

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if len(tg.streamMsgs) != 1 || tg.streamMsgs[0] != msgLLMUnavailable {
		t.Errorf("expected exactly one new stream message with the terminal text, got %v", tg.streamMsgs)
	}

	if len(tg.edits) != 0 {
		t.Errorf("expected no edits, got %v", tg.edits)
	}

	if len(tg.sent) != 0 {
		t.Errorf("expected no duplicate Send (streamed delivery skips it), got %v", tg.sent)
	}
}

// TestProcessMessage_TerminalDelivery_Telegram_PartialAppend covers task
// A partial response is retained and marked: the
// incomplete-response message is appended after the partial output via a
// final edit, without replacing the partial content.
func TestProcessMessage_TerminalDelivery_Telegram_PartialAppend(t *testing.T) {
	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, tg)

	env.runner.deliverChunk = testPartialAnswer
	env.runner.outcome = agent.TurnOutcome{Reply: testPartialAnswer, ContentDelivered: true}
	env.runner.err = errors.New("stream boom")

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: telegram.ChannelName,
		Metadata:    map[string]string{telegram.MetadataChatID: "4242"},
	})

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if len(tg.streamMsgs) != 1 || tg.streamMsgs[0] != testPartialAnswer {
		t.Fatalf("expected first chunk to create the stream message, got %v", tg.streamMsgs)
	}

	if len(tg.edits) != 1 {
		t.Fatalf("expected exactly one final edit, got %v", tg.edits)
	}

	edit := tg.edits[0]
	if !strings.HasPrefix(edit, testPartialAnswer) {
		t.Errorf("expected partial content retained as prefix, got %q", edit)
	}

	if !strings.Contains(edit, msgIncompleteResponse) {
		t.Errorf("expected incomplete-response notice appended, got %q", edit)
	}

	if len(tg.sent) != 0 {
		t.Errorf("expected no duplicate Send, got %v", tg.sent)
	}
}

// TestProcessMessage_TerminalDelivery_ZMQ_ExactlyOnce covers task 3.4
// (a ZMQ terminal reply consumes the pending slot exactly
// once): progress chunks ride SendNotification, and the failed turn's
// terminal message is delivered through exactly one final reply.
func TestProcessMessage_TerminalDelivery_ZMQ_ExactlyOnce(t *testing.T) {
	z := &termZMQChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, z)

	env.runner.deliverChunk = testProgressChunk
	env.runner.outcome = agent.TurnOutcome{Reply: testProgressChunk, ContentDelivered: true}
	env.runner.err = errors.New("model died")

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: testCLIChannelName,
		Metadata:    map[string]string{"connection_id": "sess-1"},
	})

	z.mu.Lock()
	defer z.mu.Unlock()

	if len(z.notifications) != 1 || z.notifications[0] != testProgressChunk {
		t.Errorf("expected one progress notification, got %v", z.notifications)
	}

	if len(z.sends) != 1 || z.sends[0] != msgIncompleteResponse {
		t.Errorf("expected exactly one final reply carrying the terminal message, got %v", z.sends)
	}
}

// TestProcessMessage_TerminalBypassesPostStepFormatter covers task 2.4
// (the post-step formatter is bypassed for terminal messages):
// on a formatter_post_step: true channel, a failed turn makes NO formatter
// LLM call and the fixed message text is delivered unchanged.
func TestProcessMessage_TerminalBypassesPostStepFormatter(t *testing.T) {
	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{skillName: "fmt-skill", postStep: true}, tg)

	env.runner.err = errors.New("connection refused")
	env.runner.outcome = agent.TurnOutcome{}

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: telegram.ChannelName,
		Metadata:    map[string]string{telegram.MetadataChatID: "4242"},
	})

	if calls := env.fmtModel.calls.Load(); calls != 0 {
		t.Errorf("expected no post-step formatter LLM call for a terminal message, got %d", calls)
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if len(tg.sent) != 1 || tg.sent[0] != msgLLMUnavailable {
		t.Errorf("expected exactly one Send with the unchanged terminal text, got %v", tg.sent)
	}
}

// TestProcessMessage_TerminalSurvivesAgentRunDeadline covers task 2.5
// (the terminal message survives an agent-run timeout): when the turn fails BECAUSE the agent-run deadline expired, the
// terminal message is still delivered on a live (message-scoped) context.
func TestProcessMessage_TerminalSurvivesAgentRunDeadline(t *testing.T) {
	origTimeout := agentRunTimeout
	agentRunTimeout = 150 * time.Millisecond
	t.Cleanup(func() { agentRunTimeout = origTimeout })

	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{}, tg)
	env.runner.blockUntilDone = true

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: telegram.ChannelName,
		Metadata:    map[string]string{telegram.MetadataChatID: "4242"},
	})

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if len(tg.streamMsgs) != 1 || tg.streamMsgs[0] != msgLLMUnavailable {
		t.Fatalf("expected the terminal message delivered despite the run deadline, got %v", tg.streamMsgs)
	}

	// The delivery context must NOT be the cancelled agent-run context: the
	// fake records ctx.Err() at call time; a cancelled context here means
	// the terminal send rode runCtx and would have been dropped in production.
	for _, cerr := range tg.callCtxErrs {
		if cerr != nil {
			t.Errorf("terminal delivery ran on a cancelled context: %v", cerr)
		}
	}
}

// TestProcessMessage_FormatterFailureReturnsUnformatted covers task 4.1
// (a formatter outage preserves the main response): when the main
// agent succeeds but the post-step formatter call fails, the user receives
// the unformatted main response and the turn is not a failure.
func TestProcessMessage_FormatterFailureReturnsUnformatted(t *testing.T) {
	tg := &termTGChannel{}
	env := newTermEnv(t, channelFormatterCfg{skillName: "fmt-skill", postStep: true}, tg)

	env.fmtModel.generateFunc = func(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
		return nil, errors.New("formatter model unreachable")
	}

	env.runner.outcome = agent.TurnOutcome{Reply: "the raw answer", ContentDelivered: true}

	env.run(channel.Message{
		Content:     "hi",
		ChannelName: telegram.ChannelName,
		Metadata:    map[string]string{telegram.MetadataChatID: "4242"},
	})

	if calls := env.fmtModel.calls.Load(); calls == 0 {
		t.Error("expected the formatter to have been attempted on a successful turn")
	}

	tg.mu.Lock()
	defer tg.mu.Unlock()

	if len(tg.sent) != 1 || tg.sent[0] != "the raw answer" {
		t.Errorf("expected the unformatted main response delivered, got %v", tg.sent)
	}
}

// termBuffer is a mutex-guarded strings.Builder (the CLI streaming
// callbacks write from the dispatcher goroutine while the test reads).
type termBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *termBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *termBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// newTestCLIChannel builds a real cli.Channel over an endless-empty reader
// and the shared locked writer, so streaming chunks and terminal messages
// land in the same buffer the test asserts on.
func newTestCLIChannel(buf *termBuffer) *cli.Channel {
	return cli.NewWithIO(strings.NewReader(""), buf)
}

// TestProcessMessage_LLMUnavailableErrorCode pins the llm-status-visibility
// dispatcher contract (task 2.4): a turn whose failure satisfies
// errors.Is(err, llm.ErrChainExhausted) delivers the final reply with the
// structured llm_unavailable error code in the channel metadata (the ZMQ
// channel translates it to Status=error + ErrorCode on the wire); a failure
// that does not satisfy the predicate keeps the generic path and must not
// use the code.
func TestProcessMessage_LLMUnavailableErrorCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			"exhausted chain uses the code",
			fmt.Errorf("turn failed: %w", llm.ErrChainExhausted),
			protocol.ErrorCodeLLMUnavailable,
		},
		{
			"generic failure does not use the code",
			errors.New("some other failure"), // test fixture, not a production error path
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			zmqCh := &termZMQChannel{}
			env := newTermEnv(t, channelFormatterCfg{}, zmqCh)
			env.runner.err = tc.err
			env.runner.outcome = agent.TurnOutcome{} // nothing ran → msgLLMUnavailable

			env.run(channel.Message{
				Content:     "llm outage probe",
				ChannelName: testCLIChannelName,
				Metadata:    map[string]string{"connection_id": "s1"},
			})

			if len(zmqCh.sends) != 1 {
				t.Fatalf("expected exactly one final reply, got %d: %v", len(zmqCh.sends), zmqCh.sends)
			}

			if got := zmqCh.sendErrCodes[0]; got != tc.wantCode {
				t.Errorf("final reply error_code metadata = %q, want %q", got, tc.wantCode)
			}

			// Nothing ran, so the terminal message must be the
			// temporary-unavailability text (llm-outage-delivery A2).
			if zmqCh.sends[0] != msgLLMUnavailable {
				t.Errorf("terminal message = %q, want %q", zmqCh.sends[0], msgLLMUnavailable)
			}
		})
	}
}

// TestProcessMessage_RunnerReceivesPartitionedSessionKey pins the
// react-thought-logging correlation contract: the runner (and so every
// thought-tap record) sees the partitioned session key, not the bare
// transport channel name.
func TestProcessMessage_RunnerReceivesPartitionedSessionKey(t *testing.T) {
	cases := []struct {
		name string
		ch   channel.Channel
		msg  channel.Message
		want string
	}{
		{testCLIChannelName, newTestCLIChannel(&termBuffer{}), channel.Message{Content: "hi", ChannelName: testCLIChannelName}, testCLIChannelName},
		{telegram.ChannelName, &termTGChannel{}, channel.Message{
			Content:     "hi",
			ChannelName: telegram.ChannelName,
			Metadata:    map[string]string{telegram.MetadataChatID: "123456789"},
		}, "telegram-123456789"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTermEnv(t, channelFormatterCfg{}, tc.ch)
			env.runner.outcome = agent.TurnOutcome{Reply: "ok"}

			env.run(tc.msg)

			env.runner.mu.Lock()
			defer env.runner.mu.Unlock()
			if env.runner.sawSessionKey != tc.want {
				t.Errorf("runner session key = %q, want %q", env.runner.sawSessionKey, tc.want)
			}
		})
	}
}

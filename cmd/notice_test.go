package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/tools"
)

const (
	noticeTelegramChannel   = "telegram"
	noticeChatIDKey         = "chat_id"
	noticeLockedProfile     = "locked"
	noticeConfigManagerTool = "config-manager"
	noticeWaitTool          = "wait"
)

// noticeChannel is a Channel + NoticeSender recording what it was asked to show.
type noticeChannel struct {
	name    string
	mu      sync.Mutex
	texts   []string
	metas   []map[string]string
	running atomic.Int32
	overlap atomic.Bool
	hold    time.Duration
	block   bool // block until the delivery context ends
	res     channel.NoticeResult
	err     error
	ctxErr  error
}

func (c *noticeChannel) Name() string { return c.name }
func (c *noticeChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}
func (c *noticeChannel) Send(context.Context, channel.Message) error { return nil }
func (c *noticeChannel) Close() error                                { return nil }

func (c *noticeChannel) SendNotice(ctx context.Context, meta map[string]string, text string) (channel.NoticeResult, error) {
	if c.running.Add(1) > 1 {
		c.overlap.Store(true)
	}
	defer c.running.Add(-1)

	if c.block {
		<-ctx.Done()

		return channel.NoticeResult{}, ctx.Err()
	}

	time.Sleep(c.hold)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.ctxErr = ctx.Err()
	c.texts = append(c.texts, text)
	c.metas = append(c.metas, meta)

	if c.err != nil {
		return channel.NoticeResult{}, c.err
	}

	if c.res.Outcome == "" {
		return channel.NoticeResult{Outcome: channel.NoticeDelivered, Parts: 1, DeliveredParts: 1}, nil
	}

	return c.res, nil
}

func newNoticeRuntime(t *testing.T, ch channel.Channel) *sessionRuntime {
	t.Helper()

	return &sessionRuntime{
		mgr:       channel.NewManager(zerolog.Nop(), ch),
		sanitizer: privacy.New(map[string]string{"/home/secret": "~"}),
	}
}

func TestNoticeDeliverer_RedactsAndPassesRoute(t *testing.T) {
	ch := &noticeChannel{name: noticeTelegramChannel}
	rt := newNoticeRuntime(t, ch)
	msg := channel.Message{ChannelName: noticeTelegramChannel, Metadata: map[string]string{noticeChatIDKey: "42", "turn_id": "t-1"}}

	d := rt.newNoticeDeliverer(context.Background(), "telegram-42", msg, zerolog.Nop())

	res, err := d(context.Background(), "see /home/secret/file")
	if err != nil || res.Outcome != channel.NoticeDelivered {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	if len(ch.texts) != 1 || ch.texts[0] != "see ~/file" {
		t.Fatalf("redaction not applied before the channel: %q", ch.texts)
	}

	if ch.metas[0][noticeChatIDKey] != "42" || ch.metas[0]["turn_id"] != "t-1" {
		t.Fatalf("route metadata lost: %v", ch.metas[0])
	}
}

func TestNoticeDeliverer_UnsupportedChannel(t *testing.T) {
	rt := &sessionRuntime{mgr: channel.NewManager(zerolog.Nop(), &bareChannel{name: "serial"})}
	d := rt.newNoticeDeliverer(context.Background(), "serial", channel.Message{ChannelName: "serial"}, zerolog.Nop())

	res, err := d(context.Background(), "x")
	if err != nil || res.Outcome != channel.NoticeNotShown || res.Reason != channel.NoticeReasonChannelUnsupported {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

type bareChannel struct{ name string }

func (c *bareChannel) Name() string { return c.name }
func (c *bareChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}
func (c *bareChannel) Send(context.Context, channel.Message) error { return nil }
func (c *bareChannel) Close() error                                { return nil }

// A tool-call-group's concurrent say calls on one address never interleave.
func TestNoticeDeliverer_SerialisesPerAddress(t *testing.T) {
	ch := &noticeChannel{name: testCLIChannelName, hold: 20 * time.Millisecond}
	rt := newNoticeRuntime(t, ch)
	d := rt.newNoticeDeliverer(context.Background(), testCLIChannelName, channel.Message{ChannelName: testCLIChannelName}, zerolog.Nop())

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _ = d(context.Background(), "n")
		}()
	}

	wg.Wait()

	if ch.overlap.Load() {
		t.Fatal("two deliveries of one address overlapped")
	}

	if len(ch.texts) != 4 {
		t.Fatalf("delivered %d of 4", len(ch.texts))
	}
}

// A /stop, preemption or agent-run deadline cancels the tool's context, never
// a delivery already started.
func TestNoticeDeliverer_SurvivesTurnCancellation(t *testing.T) {
	ch := &noticeChannel{name: testCLIChannelName}
	rt := newNoticeRuntime(t, ch)
	d := rt.newNoticeDeliverer(context.Background(), testCLIChannelName, channel.Message{ChannelName: testCLIChannelName}, zerolog.Nop())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := d(cancelled, "whole")
	if err != nil || res.Outcome != channel.NoticeDelivered || ch.ctxErr != nil {
		t.Fatalf("res=%+v err=%v ctxErr=%v", res, err, ch.ctxErr)
	}
}

func TestNoticeDeliverer_TimeoutAndShutdown(t *testing.T) {
	old := noticeDeliveryTimeout
	noticeDeliveryTimeout = 30 * time.Millisecond

	t.Cleanup(func() { noticeDeliveryTimeout = old })

	ch := &noticeChannel{name: testCLIChannelName, block: true}
	rt := newNoticeRuntime(t, ch)

	d := rt.newNoticeDeliverer(context.Background(), testCLIChannelName, channel.Message{ChannelName: testCLIChannelName}, zerolog.Nop())
	if _, err := d(context.Background(), "x"); err == nil {
		t.Fatal("a delivery over the timeout must fail")
	}

	// Daemon shutdown (the base context) cancels it even before the timeout.
	noticeDeliveryTimeout = time.Minute

	base, stop := context.WithCancel(context.Background())
	d = rt.newNoticeDeliverer(base, testCLIChannelName, channel.Message{ChannelName: testCLIChannelName}, zerolog.Nop())

	go func() {
		time.Sleep(20 * time.Millisecond)
		stop()
	}()

	if _, err := d(context.Background(), "x"); err == nil {
		t.Fatal("shutdown must cancel a delivery")
	}
}

// Through the tool and the digest: a transport failure is [TRANSIENT], a
// refusal [PERMANENT], not_shown carries no prefix.
func TestSayThroughDigest(t *testing.T) {
	tracked := errdigest.NewTrackedTool(tools.NewSayTool(), errDigestClassifier(),
		func(context.Context) string { return testCLIChannelName }, zerolog.Nop())

	failing := tools.WithNoticeDeliverer(context.Background(), func(context.Context, string) (channel.NoticeResult, error) {
		return channel.NoticeResult{}, errors.New("boom")
	})

	out, err := tracked.InvokableRun(failing, `{"text":"hi"}`)
	if err != nil || !strings.HasPrefix(out, "[TRANSIENT]") || !strings.Contains(out, "nothing was shown") {
		t.Fatalf("failure: out=%q err=%v", out, err)
	}

	out, err = tracked.InvokableRun(failing, `{"text":"   "}`)
	if err != nil || !strings.HasPrefix(out, "[PERMANENT]") {
		t.Fatalf("refusal: out=%q err=%v", out, err)
	}

	out, err = tracked.InvokableRun(context.Background(), `{"text":"hi"}`)
	if err != nil || strings.HasPrefix(out, "[") || !strings.HasPrefix(out, "not_shown") {
		t.Fatalf("not_shown must carry no severity prefix: out=%q err=%v", out, err)
	}
}

// Gating: the code-owned default (denies only config-manager) exposes say; an
// explicit allow-list that omits it removes it and the daemon keeps serving.
func TestBuildTools_SayGating(t *testing.T) {
	base := func(profiles map[string]config.OwnerProfileConfig) *config.Config {
		return &config.Config{
			BuiltinTools: config.BuiltinToolsConfig{Profiles: profiles},
			Profiles:     map[string]config.ProfileConfig{okfProfileDefault: {}, noticeLockedProfile: {}},
		}
	}

	implicit := base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Deny: []string{noticeConfigManagerTool}}})
	if !okfToolNames(t, implicit, okfProfileDefault)["say"] {
		t.Error("code-owned default must expose say")
	}

	// config.applyDefaults installs exactly this entry for both built-in
	// profiles when the builtin_tools block is absent (fresh init config).
	both := base(map[string]config.OwnerProfileConfig{
		okfProfileDefault: {Deny: []string{noticeConfigManagerTool}},
		"edit":            {Deny: []string{noticeConfigManagerTool}},
	})
	if !okfToolNames(t, both, "edit")["say"] {
		t.Error("the implicit default must expose say under the edit profile too")
	}

	allow := base(map[string]config.OwnerProfileConfig{okfProfileDefault: {Allow: []string{"*"}}, noticeLockedProfile: {Allow: []string{noticeWaitTool}}})
	if okfToolNames(t, allow, noticeLockedProfile)["say"] {
		t.Error("an explicit allow-list without say must exclude it")
	}

	if !okfToolNames(t, allow, okfProfileDefault)["say"] {
		t.Error("allow * must expose say")
	}
}

// A Turn that delivered a Notice and then failed terminally got a completed
// tool call: the user is told the response could not be completed, never the
// "try again" outage text (say-mid-turn-notice D9).
func TestTerminalMessage_AfterDeliveredNotice(t *testing.T) {
	outcome := agent.TurnOutcome{ToolCallStarted: true, ToolCallCompleted: true, ContentDelivered: false}

	if got := terminalMessageFor(outcome, errors.New("error 503: overloaded")); got != msgIncompleteResponse {
		t.Fatalf("terminal message = %q", got)
	}
}

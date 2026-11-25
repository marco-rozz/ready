package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/okf"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/steer"
)

const verifyConcept = "---\ntype: Metric\ntitle: Revenue\ngenerated: { by: \"ready/1\", at: \"2026-11-01T00:00:00Z\" }\n---\n<!-- hidden <b>comment</b> -->\nBody\n"

var tokenRE = regexp.MustCompile(`/verify \S+ \S+ ([0-9a-f]{16})`)

// verifyEnv is a lifecycleEnv with a knowledge bundle and the OKF writer.
type verifyEnv struct {
	*lifecycleEnv
	agent string
	file  string
}

func newVerifyEnv(t *testing.T, channels ...channel.Channel) *verifyEnv {
	t.Helper()
	env := newLifecycleEnv(t, channels...)
	agent := t.TempDir()
	dir := filepath.Join(agent, okf.KnowledgeDirName, "home", "metrics")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "revenue.md")
	if err := os.WriteFile(file, []byte(verifyConcept), 0o600); err != nil {
		t.Fatal(err)
	}
	env.runtime().okf = okf.NewWriter(okf.WriterOptions{AgentDir: agent, Version: "test", Logger: zerolog.Nop()})

	return &verifyEnv{lifecycleEnv: env, agent: agent, file: file}
}

func (e *verifyEnv) content(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.file)
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func TestParseSessionCommand_Verify(t *testing.T) {
	cases := []struct {
		in   string
		cmd  string
		arg  string
		want bool
	}{
		{"/verify", cmdVerify, "", true},
		{"  /verify home/x marco  ", cmdVerify, "home/x marco", true},
		{"/verify\thome/x", cmdVerify, "home/x", true},
		{"/verify\nhome/x", cmdVerify, "home/x", true},
		{"/verifyall home", "", "", false},
		{"/verify@bot home", "", "", false},
		{"please /verify home", "", "", false},
	}
	for _, c := range cases {
		cmd, arg, ok := parseSessionCommand(c.in)
		if ok != c.want || cmd != c.cmd || arg != c.arg {
			t.Errorf("%q → (%q,%q,%v), want (%q,%q,%v)", c.in, cmd, arg, ok, c.cmd, c.arg, c.want)
		}
	}
}

// Two-phase verification on the stdin/ZMQ "cli" channel: no model call, no
// transcript, only the concept's verified key and the bundle log change.
func TestVerify_PreviewThenConfirm(t *testing.T) {
	env := newVerifyEnv(t)

	env.run(cliMsg("/verify home/metrics/revenue"))
	out := env.buf.String()
	if !strings.Contains(out, "<!-- hidden <b>comment</b> -->") || !strings.Contains(out, "Trust: unverified; status: stable") {
		t.Fatalf("preview must show the raw bytes verbatim:\n%s", out)
	}
	m := tokenRE.FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, "/verify home/metrics/revenue <your-id> "+m[1]) {
		t.Fatalf("preview must end with the confirmation line:\n%s", out)
	}
	if env.content(t) != verifyConcept {
		t.Fatal("a preview must not write")
	}

	env.run(cliMsg("/verify home/metrics/revenue marco " + m[1]))
	if !strings.Contains(env.buf.String(), "Verification recorded. Tier: human-reviewed.") {
		t.Fatalf("reply: %s", env.buf.String())
	}
	got := env.content(t)
	if !strings.Contains(got, `verified:`) || !strings.Contains(got, `by: "human:marco"`) || !strings.Contains(got, `x-ready-via: "cli"`) {
		t.Fatalf("concept:\n%s", got)
	}
	log, err := os.ReadFile(filepath.Join(env.agent, okf.KnowledgeDirName, "home", "log.md"))
	if err != nil || !strings.Contains(string(log), "* **Verification**: [Revenue](metrics/revenue.md) confirmed by human:marco.") {
		t.Fatalf("log: %v\n%s", err, log)
	}
	if env.runner.callCount() != 0 {
		t.Fatal("/verify must never reach the model")
	}
	if n := countFiles(t, env.sessionsDir); n != 0 {
		t.Fatalf("/verify must record nothing in any transcript, found %d session files", n)
	}
}

func TestVerify_ChangedSinceAndDigestRejected(t *testing.T) {
	env := newVerifyEnv(t)
	env.run(cliMsg("/verify home/metrics/revenue"))
	tok := tokenRE.FindStringSubmatch(env.buf.String())[1]

	// the digest okf_query prints is not a token
	digest := okf.Digest([]byte(verifyConcept))
	env.run(cliMsg("/verify home/metrics/revenue marco " + digest))
	want := "Concept changed since the preview; review it again with /verify home/metrics/revenue."
	if !strings.Contains(env.buf.String(), want) {
		t.Fatalf("digest as token: %s", env.buf.String())
	}

	if err := os.WriteFile(env.file, []byte(verifyConcept+"more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := env.content(t)
	env.buf.mu.Lock()
	env.buf.buf.Reset()
	env.buf.mu.Unlock()
	env.run(cliMsg("/verify home/metrics/revenue marco " + tok))
	if !strings.Contains(env.buf.String(), want) || env.content(t) != before {
		t.Fatalf("stale token: %s", env.buf.String())
	}
}

func TestVerify_FixedReplies(t *testing.T) {
	env := newVerifyEnv(t)
	if err := os.WriteFile(filepath.Join(env.agent, okf.KnowledgeDirName, "home", "plain.md"), []byte("no frontmatter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"/verify", msgVerifyUsage},
		{"/verify a b c d", msgVerifyUsage},
		{"/verify nobundle", msgVerifyBadRef},
		{"/verify home/../x", msgVerifyBadRef},
		{"/verify home/metrics/revenue bad!id", msgVerifyBadID},
		{"/verify home/metrics/revenue " + strings.Repeat("a", 65), msgVerifyBadID},
		{"/verify home/metrics/revenue marco nothex", msgVerifyBadToken},
		{"/verify ghost/x", msgVerifyBundle},
		{"/verify home/metrics/missing", msgVerifyConcept},
		{"/verify home/plain", msgVerifyNotConcept},
	}
	for _, c := range cases {
		env.buf.mu.Lock()
		env.buf.buf.Reset()
		env.buf.mu.Unlock()
		env.run(cliMsg(c.in))
		if !strings.Contains(env.buf.String(), c.want) {
			t.Errorf("%q → %q, want %q", c.in, env.buf.String(), c.want)
		}
	}
	if env.content(t) != verifyConcept {
		t.Fatal("refusals must not write")
	}
}

func TestVerify_LookAlikeIsATurn(t *testing.T) {
	env := newVerifyEnv(t)
	env.run(cliMsg("/verifyall home"))
	if env.runner.callCount() != 1 {
		t.Fatalf("a look-alike command is an ordinary Turn, model calls = %d", env.runner.callCount())
	}
}

// verifyTGChannel is a Telegram channel double with a configurable principal.
type verifyTGChannel struct {
	termTGChannel
	principal bool
}

func (c *verifyTGChannel) VerifyPrincipal(map[string]string) bool { return c.principal }

func tgMsg(text string) channel.Message {
	return channel.Message{Content: text, ChannelName: telegram.ChannelName, Metadata: map[string]string{
		telegram.MetadataChatID: "42", telegram.MetadataUserID: "42", telegram.MetadataChatType: "private",
	}}
}

func TestVerify_TelegramPrincipalRule(t *testing.T) {
	tg := &verifyTGChannel{}
	env := newVerifyEnv(t, tg)
	env.runtime().resets = map[string]session.Rule{}

	env.run(tgMsg("/verify home/metrics/revenue"))
	tg.mu.Lock()
	sent := append([]string(nil), tg.sent...)
	tg.mu.Unlock()
	if len(sent) != 1 || sent[0] != msgVerifyAuth {
		t.Fatalf("unauthenticated sender: %v", sent)
	}
	if env.content(t) != verifyConcept {
		t.Fatal("must not write")
	}

	tg.principal = true
	env.run(tgMsg("/verify home/metrics/revenue marco"))
	tg.mu.Lock()
	sent = append([]string(nil), tg.sent...)
	tg.mu.Unlock()
	joined := strings.Join(sent[1:], "\n")
	if !strings.Contains(joined, "<pre>") || !strings.Contains(joined, "&lt;!-- hidden &lt;b&gt;comment&lt;/b&gt; --&gt;") || strings.Contains(joined, "<b>comment") {
		t.Fatalf("telegram preview must be escaped inside <pre>:\n%s", joined)
	}
	m := tokenRE.FindStringSubmatch(joined)
	if m == nil || !strings.Contains(joined, "<code>/verify home/metrics/revenue marco "+m[1]+"</code>") {
		t.Fatalf("confirmation line missing:\n%s", joined)
	}

	env.run(tgMsg("/verify home/metrics/revenue marco " + m[1]))
	if got := env.content(t); !strings.Contains(got, `x-ready-via: "telegram"`) {
		t.Fatalf("concept:\n%s", got)
	}
}

// /verify is queued FIFO behind a Turn in flight.
func TestVerify_QueuedBehindInFlightTurn(t *testing.T) {
	env := newVerifyEnv(t)
	env.runner.started = make(chan struct{})
	env.runner.release = make(chan struct{})

	incoming := make(chan channel.Message, 4)
	d := &dispatcher{
		ctx:       context.Background(),
		incoming:  incoming,
		processFn: env.runtime().handleItem,
		keyFn:     func(m channel.Message) string { return string(session.AddressOf(m)) },
		logger:    zerolog.Nop(),
		workers:   map[string]*steer.Inbox{},
	}
	done := make(chan struct{})
	go func() { d.run(); close(done) }()

	incoming <- cliMsg("slow request")
	<-env.runner.started
	incoming <- cliMsg("/verify home/metrics/revenue")

	time.Sleep(50 * time.Millisecond)
	if strings.Contains(env.buf.String(), "Verify home/metrics/revenue") {
		t.Fatal("/verify must wait for the Turn in flight")
	}
	close(env.runner.release)
	close(incoming)
	<-done

	if !strings.Contains(env.buf.String(), "Verify home/metrics/revenue") {
		t.Fatalf("preview missing after the Turn: %s", env.buf.String())
	}
}

func TestFenceFor(t *testing.T) {
	if got := fenceFor("plain"); got != "```" {
		t.Errorf("got %q", got)
	}
	if got := fenceFor("a ```` b ``` c"); got != "`````" {
		t.Errorf("got %q", got)
	}
}

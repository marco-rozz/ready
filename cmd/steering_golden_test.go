package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/persistence"
)

// Golden baseline of a multi-tool Turn through the real ChatModelAgent path
// (turn-steering 1.1): the Session file and every assembled prompt, with the
// nondeterministic parts (turn nonce, clock, temp dirs) normalised. The same
// scenario must stay byte-identical once steering is enabled and no message
// arrives mid-turn (9.8). Regenerate with UPDATE_GOLDEN=1.

var (
	goldenNonce = regexp.MustCompile(`<(/?)(turn_context|final_reminder|formatting_directive|user_steer)_([0-9a-f]{8})>`)
	goldenClock = regexp.MustCompile(`Current date and time: [^\n<]*`)
	goldenCall  = regexp.MustCompile(`call_[0-9a-f]{8}_`)
)

type goldenNormalizer struct {
	nonce string
	dirs  []string
}

func (n *goldenNormalizer) norm(s string) string {
	if n.nonce == "" {
		if m := goldenNonce.FindStringSubmatch(s); m != nil {
			n.nonce = m[3]
		}
	}

	if n.nonce != "" {
		s = strings.ReplaceAll(s, n.nonce, "NONCE")
	}

	s = goldenClock.ReplaceAllString(s, "Current date and time: CLOCK")
	s = goldenCall.ReplaceAllString(s, "call_NONCE_")

	for _, d := range n.dirs {
		if d != "" {
			s = strings.ReplaceAll(s, d, "TMPDIR")
		}
	}

	return s
}

type goldenMsg struct {
	Role       string   `json:"role"`
	Kind       string   `json:"kind,omitempty"`
	Content    string   `json:"content,omitempty"`
	ToolCalls  []string `json:"tool_calls,omitempty"`
	ToolCallID string   `json:"tool_call_id,omitempty"`
}

func goldenFromHistory(n *goldenNormalizer, msgs []persistence.HistoryMessage) []goldenMsg {
	out := make([]goldenMsg, 0, len(msgs))

	for _, m := range msgs {
		g := goldenMsg{Role: string(m.Role), Kind: string(m.Kind), Content: n.norm(m.Content), ToolCallID: n.norm(m.ToolCallID)}
		for _, tc := range m.ToolCalls {
			g.ToolCalls = append(g.ToolCalls, n.norm(tc.ID)+":"+tc.Name+":"+tc.Arguments)
		}

		out = append(out, g)
	}

	return out
}

func goldenFromSchema(n *goldenNormalizer, msgs []*schema.Message) []goldenMsg {
	out := make([]goldenMsg, 0, len(msgs))

	for _, m := range msgs {
		g := goldenMsg{Role: string(m.Role), Content: n.norm(m.Content), ToolCallID: n.norm(m.ToolCallID)}
		for _, tc := range m.ToolCalls {
			g.ToolCalls = append(g.ToolCalls, n.norm(tc.ID)+":"+tc.Function.Name+":"+tc.Function.Arguments)
		}

		out = append(out, g)
	}

	return out
}

// goldenScenario runs the scripted multi-tool Turn through run and returns the
// normalised Session file + prompts as one JSON document.
func goldenScenario(t *testing.T, run func(env *termEnv, msg string)) []byte {
	t.Helper()

	buf := &termBuffer{}
	env := newTermEnv(t, channelFormatterCfg{}, newTestCLIChannel(buf))

	m := &transcriptModel{steps: []func([]*schema.Message) (*schema.Message, error){
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		func([]*schema.Message) (*schema.Message, error) { return lampCall() },
		finalAnswer("both lights are on"),
	}}
	lamp := &lampTool{}
	env.useOrchestrator(t, m, lamp, 0)

	run(env, "turn on the kitchen light, twice")

	if lamp.runs.Load() != 2 {
		t.Fatalf("lamp runs = %d, want 2", lamp.runs.Load())
	}

	n := &goldenNormalizer{dirs: []string{env.sessionsDir}}

	doc := struct {
		Transcript []goldenMsg   `json:"transcript"`
		Prompts    [][]goldenMsg `json:"prompts"`
	}{Transcript: goldenFromHistory(n, env.jsonlLines(t))}

	for i := 0; i < m.callCount(); i++ {
		doc.Prompts = append(doc.Prompts, goldenFromSchema(n, m.input(i)))
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	return append(out, '\n')
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", "steering", name)

	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with UPDATE_GOLDEN=1): %v", path, err)
	}

	if string(want) != string(got) {
		t.Fatalf("output differs from golden %s (run with UPDATE_GOLDEN=1 to regenerate if intended)\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

const goldenFile = "no_steer_multitool.golden.json"

func TestGolden_NoSteerMultiToolTurn(t *testing.T) {
	got := goldenScenario(t, func(env *termEnv, msg string) {
		env.run(cliMsg(msg))
	})

	// The normaliser must have found the nonce, or the golden would be
	// comparing live values.
	if !strings.Contains(string(got), "NONCE") {
		t.Fatal("golden contains no normalised nonce: normalisation failed")
	}

	checkGolden(t, goldenFile, got)
}

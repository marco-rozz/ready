//nolint:goconst // literal is a fixed skill name used in multiple scenarios
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/formatter"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/skillcache"
)

// fakeResolver satisfies formatter.PathResolver.
type fakeResolver struct{ path string }

func (f fakeResolver) SkillFilePath(_ string) (string, error) { return f.path, nil }

// newTestFormatter returns a formatter.Formatter backed by an on-disk SKILL.md
// (written into a temp dir) and a real skillcache. The nil chatModel is
// acceptable because these tests exercise only LoadSkillPrompt, not Format.
func newTestFormatter(t *testing.T, skillBody string) *formatter.Formatter {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := writeFileHelper(path, skillBody); err != nil {
		t.Fatalf("write skill: %v", err)
	}

	cache, err := skillcache.New()
	if err != nil {
		t.Fatalf("skillcache.New: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	return formatter.New(nil, fakeResolver{path: path}, cache)
}

func writeFileHelper(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// tailTurnContextInput returns the tail-dynamic turn-context input frozen now.
func tailTurnContextInput(cfg channelFormatterCfg) formatter.TurnContextInput {
	in, _ := freezeTurnContextInput(cfg, time.Now())

	return in
}

// Task: post-step mode — no <formatter_skill> wrap, raw last UserMessage.
func TestApplyFormatterInlineReinforcement_PostStepIsUntouched(t *testing.T) {
	ctx := context.Background()
	fmtr := newTestFormatter(t, "SKILL_BODY_DO_NOT_APPEAR")

	conv := []*schema.Message{
		schema.SystemMessage("existing-system"),
		schema.UserMessage("hello"),
	}

	got := applyFormatterInlineReinforcement(
		ctx, conv,
		channelFormatterCfg{skillName: "telegram-formatter", postStep: true},
		fmtr, "telegram",
		zerolog.New(io.Discard),
	)

	if len(got) != 2 {
		t.Fatalf("post-step must not prepend any message; len=%d", len(got))
	}
	for _, m := range got {
		if strings.Contains(m.Content, "<formatter_skill>") {
			t.Errorf("post-step must not wrap with <formatter_skill>; got: %s", m.Content)
		}
		if strings.Contains(m.Content, "SKILL_BODY_DO_NOT_APPEAR") {
			t.Errorf("post-step must not load the formatter skill body")
		}
	}
	if got[1].Content != "hello" {
		t.Errorf("expected raw user content, got %q", got[1].Content)
	}
}

// Task 3.2 — inline mode wraps position-1 with the formatter skill, but never
// rewrites the user message (the envelope is now a separate turn-context
// message built elsewhere).
func TestApplyFormatterInlineReinforcement_InlineWrapsOnlyNoEnvelope(t *testing.T) {
	ctx := context.Background()
	fmtr := newTestFormatter(t, "TELEGRAM_RULES_BODY")

	conv := []*schema.Message{
		schema.UserMessage("hi there"),
	}

	got := applyFormatterInlineReinforcement(
		ctx, conv,
		channelFormatterCfg{skillName: "telegram-formatter", postStep: false},
		fmtr, "telegram",
		zerolog.New(io.Discard),
	)

	if len(got) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(got))
	}
	if got[0].Role != schema.System {
		t.Fatalf("position 0 must be System, got role=%v", got[0].Role)
	}
	sys := got[0].Content
	if !strings.HasPrefix(sys, "<formatter_skill>") || !strings.HasSuffix(sys, "</formatter_skill>") {
		t.Errorf("position-1 wrap missing static tags; got: %s", sys)
	}
	if !strings.Contains(sys, "TELEGRAM_RULES_BODY") {
		t.Errorf("skill body missing inside wrap; got: %s", sys)
	}

	if got[1].Role != schema.User {
		t.Fatalf("last message must be User, got role=%v", got[1].Role)
	}
	if got[1].Content != "hi there" {
		t.Errorf("expected the user message to be passed through verbatim (raw), got %q", got[1].Content)
	}
}

// Empty formatter_skill: no wrap.
func TestApplyFormatterInlineReinforcement_EmptySkillIsNoop(t *testing.T) {
	ctx := context.Background()
	fmtr := newTestFormatter(t, "unused")

	conv := []*schema.Message{schema.UserMessage("hello")}
	got := applyFormatterInlineReinforcement(
		ctx, conv,
		channelFormatterCfg{skillName: "", postStep: false},
		fmtr, "cli", zerolog.New(io.Discard),
	)
	if len(got) != 1 || got[0].Content != "hello" {
		t.Errorf("empty formatter_skill must be a no-op; got: %+v", got)
	}
}

// openTurn records a turn's opening lines exactly as processMessage does:
// the verbatim user message, then the frozen turn-context line.
func openTurn(h *persistence.ConversationHistory, user, turnID string, in formatter.TurnContextInput) {
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: user, Timestamp: time.Now()})
	h.Add(persistence.HistoryMessage{
		Role:      persistence.RoleUser,
		Kind:      persistence.KindTurnContext,
		Content:   formatter.BuildTurnContext(turnID, in),
		Timestamp: time.Now(),
	})
}

// Task 3.4/D3 — under LayoutTailDynamic the recorded turn-context line is the
// last element on the first attempt and uses Role: user; nothing is appended.
func TestAssembleTailDynamic_TurnContextIsLastUserMessage(t *testing.T) {
	prefix := []*schema.Message{schema.SystemMessage("static")}
	h := persistence.NewConversationHistory("t", nil)
	openTurn(h, "hi", "deadbeef", formatter.TurnContextInput{Timestamp: "T", InlineFormatter: true})

	got := assembleTailDynamic(prefix, h.Messages())

	if len(got) != 3 {
		t.Fatalf("expected prefix + user + turn context, got %d messages", len(got))
	}
	last := got[len(got)-1]
	if last.Role != schema.User {
		t.Fatalf("expected last message role User, got %v", last.Role)
	}
	if !strings.HasSuffix(strings.TrimSpace(last.Content), "</formatting_directive_deadbeef>") {
		t.Errorf("expected the last block to be formatting_directive; got: %s", last.Content)
	}
	if got[1].Content != "hi" {
		t.Errorf("expected the user message untouched; got: %s", got[1].Content)
	}
}

// Spec "Resumed attempt reuses the recorded turn context": a retry assembles
// exactly one turn-context message for the turn, right after the user
// message and followed by the recorded tool-call groups.
func TestAssembleTailDynamic_ResumedAttemptReusesTurnContext(t *testing.T) {
	h := persistence.NewConversationHistory("t", nil)
	openTurn(h, "turn on the light", "cafe0001", formatter.TurnContextInput{Timestamp: "T"})
	first := assembleTailDynamic(nil, h.Messages())

	h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, ToolCalls: []persistence.ToolCallRecord{{ID: "call_cafe0001_1_0", Name: "light_on", Arguments: "{}"}}})
	h.Add(persistence.HistoryMessage{Role: persistence.RoleTool, ToolCallID: "call_cafe0001_1_0", Content: "ok"})
	retry := assembleTailDynamic(nil, h.Messages())

	var tcs []int
	for i, m := range retry {
		if strings.Contains(m.Content, "<turn_context_cafe0001>") {
			tcs = append(tcs, i)
		}
	}
	if len(tcs) != 1 || tcs[0] != 1 {
		t.Fatalf("expected exactly one turn context right after the user message, got positions %v", tcs)
	}
	if retry[1].Content != first[1].Content {
		t.Errorf("expected the retry's turn context byte-identical to the first attempt's")
	}
	if retry[2].Role != schema.Assistant || retry[2].ToolCalls[0].ID != "call_cafe0001_1_0" {
		t.Fatalf("expected the recorded tool call after the turn context, got %+v", retry[2])
	}
	if last := retry[3]; last.Role != schema.Tool || last.ToolCallID != "call_cafe0001_1_0" || last.ToolName != "light_on" {
		t.Fatalf("expected the recorded result (with its tool name) to end the prompt, got %+v", last)
	}
}

// Task 3.4/D5 + spec "Head-safe folds the current turn context and keeps past
// ones": the current turn context is folded into a leading system message and
// not repeated in history; past turn-context lines stay as user messages.
func TestAssembleHeadSafe_TurnContextIsLeadingSystemMessage(t *testing.T) {
	prefix := []*schema.Message{schema.SystemMessage("static")}
	h := persistence.NewConversationHistory("t", nil)
	openTurn(h, "earlier", "0ld0ld00", formatter.TurnContextInput{Timestamp: "T0"})
	h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "earlier reply"})
	openTurn(h, "hi", "deadbeef", formatter.TurnContextInput{Timestamp: "T1"})

	head := formatter.BuildTurnContext("deadbeef", formatter.TurnContextInput{Timestamp: "T1-hour", InlineFormatter: true})
	got := assembleHeadSafe(prefix, h.Messages(), head)

	if got[0].Role != schema.System || got[0].Content != head {
		t.Fatalf("expected the frozen head turn context as the leading System message, got %+v", got[0])
	}
	if got[1].Content != "static" {
		t.Errorf("expected the prefix to follow; got: %s", got[1].Content)
	}

	var current, past int
	for _, m := range got[1:] {
		if strings.Contains(m.Content, "<turn_context_deadbeef>") {
			current++
		}
		if strings.Contains(m.Content, "<turn_context_0ld0ld00>") {
			past++
			if m.Role != schema.User {
				t.Errorf("expected the past turn context as a user message, got %v", m.Role)
			}
		}
	}
	if current != 0 || past != 1 {
		t.Fatalf("expected the current turn context folded only (0 in history) and the past one kept (1), got %d/%d", current, past)
	}
	if last := got[len(got)-1]; last.Content != "hi" {
		t.Errorf("expected the current user message to end the prompt, got %q", last.Content)
	}
}

// Task 3.7 — directive recency under LayoutTailDynamic: the formatting
// directive stays the final block of the turn context.
func TestBuildTurnContextInput_DirectiveRecency(t *testing.T) {
	in := tailTurnContextInput(channelFormatterCfg{skillName: "s", postStep: false})

	body := formatter.BuildTurnContext("id1", in)
	if !strings.HasSuffix(strings.TrimSpace(body), "</formatting_directive_id1>") {
		t.Errorf("expected directive to remain the final block; got: %s", body)
	}
}

// Task 3.10 — post-step channels (or empty formatter_skill) get no
// reinforcement blocks, but the turn context still carries the timestamp.
func TestBuildTurnContextInput_PostStepOmitsReinforcement(t *testing.T) {
	fmtCfg := channelFormatterCfg{skillName: "petscii-formatter", postStep: true}

	in := tailTurnContextInput(fmtCfg)
	if in.InlineFormatter {
		t.Errorf("expected InlineFormatter=false under formatter_post_step: true")
	}

	body := formatter.BuildTurnContext("id4", in)
	if strings.Contains(body, "final_reminder") || strings.Contains(body, "formatting_directive") {
		t.Errorf("expected no reinforcement blocks; got: %s", body)
	}
	if !strings.Contains(body, "Current date and time:") {
		t.Errorf("expected timestamp to still be present; got: %s", body)
	}
}

// Task 3.8 — history identity: the user message bytes in the assembled
// conversation equal the bytes persisted for that turn (the turn-context
// message is a separate element, never folded into the user message).
func TestHistoryIdentity_AssembledUserBytesEqualPersistedBytes(t *testing.T) {
	ctx := context.Background()
	cfg := channelFormatterCfg{skillName: "telegram-formatter", postStep: false}
	lg := zerolog.New(io.Discard)
	fmtr := newTestFormatter(t, "SKILL_BODY")

	dir := t.TempDir()
	mgr, _, _ := testSessions(t, dir)

	sKey := "telegram-42"
	userTurns := []string{"first message", "second message", "third message"}

	for _, u := range userTurns {
		h := currentHistory(t, mgr, sKey)
		in := tailTurnContextInput(cfg)
		openTurn(h, u, formatter.NewTurnID(), in)

		prefix := applyFormatterInlineReinforcement(ctx, nil, cfg, fmtr, "telegram", lg)
		conv := assembleTailDynamic(prefix, h.Messages())

		// The user message sits right before the trailing turn-context
		// message, byte-identical to the persisted bytes.
		if lastHistoryUser := conv[len(conv)-2]; lastHistoryUser.Role != schema.User || lastHistoryUser.Content != u {
			t.Fatalf("expected the assembled user message to equal %q verbatim, got %+v", u, lastHistoryUser)
		}

		h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "reply-" + u, Timestamp: time.Now()})
		if saveErr := h.Save(ctx); saveErr != nil {
			t.Fatalf("history.Save: %v", saveErr)
		}
	}

	mgr2, _, _ := testSessions(t, dir) // restart
	reloaded := currentHistory(t, mgr2, sKey).Messages()

	var userCount int
	for _, m := range reloaded {
		if m.IsTurnBoundary() {
			userCount++
			if strings.Contains(m.Content, "<turn_context_") ||
				strings.Contains(m.Content, "<final_reminder_") ||
				strings.Contains(m.Content, "<formatting_directive_") ||
				strings.Contains(m.Content, "<formatter_skill>") {
				t.Errorf("stored user message must be raw; got: %q", m.Content)
			}
		}
	}
	if userCount != len(userTurns) {
		t.Errorf("expected %d persisted user turns, got %d", len(userTurns), userCount)
	}
}

// Task 6.1/6.2 + session-transcript-persistence §D9 — under
// LayoutTailDynamic, turn N's whole assembled conversation (its recorded
// turn-context message included) is a byte-identical prefix of turn N+1's:
// the cross-turn cache boundary is the end of turn N, not its user message.
// See tasks/spike-kv-cache-prompt-layout.md for the measured TTFT numbers.
func TestAssembleTailDynamic_PrefixStableAcrossTurns(t *testing.T) {
	ctx := context.Background()
	cfg := channelFormatterCfg{skillName: "telegram-formatter", postStep: false}
	lg := zerolog.New(io.Discard)
	fmtr := newTestFormatter(t, "SKILL_BODY")

	mgr, _, _ := testSessions(t, t.TempDir())
	sKey := "telegram-99"

	assembleTurn := func(userText string) []*schema.Message {
		h := currentHistory(t, mgr, sKey)
		in := tailTurnContextInput(cfg)
		openTurn(h, userText, formatter.NewTurnID(), in)

		prefix := applyFormatterInlineReinforcement(ctx, nil, cfg, fmtr, "telegram", lg)
		conv := assembleTailDynamic(prefix, h.Messages())

		h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "reply-" + userText, Timestamp: time.Now()})
		if saveErr := h.Save(ctx); saveErr != nil {
			t.Fatalf("history.Save: %v", saveErr)
		}

		return conv
	}

	turn1 := assembleTurn("first message")
	turn2 := assembleTurn("second message")

	if len(turn2) <= len(turn1) {
		t.Fatalf("expected turn 2 to be strictly longer than turn 1; got len(turn2)=%d, len(turn1)=%d", len(turn2), len(turn1))
	}
	for i, m := range turn1 {
		if turn2[i].Role != m.Role || turn2[i].Content != m.Content {
			t.Fatalf("expected turn 2 to extend turn 1 byte-identically at index %d; got %+v vs %+v", i, turn2[i], m)
		}
	}
}

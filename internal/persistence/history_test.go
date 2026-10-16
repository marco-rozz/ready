package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func newTestHistory(t *testing.T) (*ConversationHistory, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "cli", "2026-09-30T08-00-00_0192b7a0-0000-7000-8000-000000000001.jsonl")

	return NewConversationHistory("s1", NewSessionFile(path)), path
}

func reopen(t *testing.T, path string) *ConversationHistory {
	t.Helper()

	h := NewConversationHistory("s1", NewSessionFile(path))
	if err := h.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	return h
}

func user(s string) HistoryMessage {
	return HistoryMessage{Role: RoleUser, Content: s, Timestamp: time.Now()}
}

func turnCtx(s string) HistoryMessage {
	return HistoryMessage{Role: RoleUser, Kind: KindTurnContext, Content: s, Timestamp: time.Now()}
}

func assistant(s string, calls ...ToolCallRecord) HistoryMessage {
	return HistoryMessage{Role: RoleAssistant, Content: s, ToolCalls: calls, Timestamp: time.Now()}
}

func toolResult(id, s string) HistoryMessage {
	return HistoryMessage{Role: RoleTool, ToolCallID: id, Content: s, Timestamp: time.Now()}
}

func call(id, name string) ToolCallRecord {
	return ToolCallRecord{ID: id, Name: name, Arguments: `{}`}
}

func fileLines(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// addTurn records one complete Turn and saves it.
func addTurn(t *testing.T, h *ConversationHistory, text string) {
	t.Helper()

	h.Add(user(text))
	h.Add(turnCtx("ctx-" + text))
	h.Add(assistant("reply-" + text))

	if err := h.Save(context.Background()); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// 3.2 — the record kinds round-trip through JSON with a timestamp.
func TestRecordKinds_RoundTrip(t *testing.T) {
	ended := time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)
	records := []HistoryMessage{
		{Kind: KindCompaction, Summary: "sum", FirstKept: 7, PreservedSkills: []string{"home-provisioning"}, TokensBefore: 120000, TokensAfter: 30000, Usage: &UsageRecord{ReadTokens: 10, WrittenTokens: 2, AuxCalls: 1}, Timestamp: ended},
		{Kind: KindTurnUsage, Usage: &UsageRecord{ReadTokens: 100, WrittenTokens: 20, CachedTokens: 5, LLMCalls: 3, AuxCalls: 1, Turns: 1}, Timestamp: ended},
		{Kind: KindSessionClosed, EndedAt: &ended, Reason: ReasonResetDaily, Usage: &UsageRecord{ReadTokens: 1, Turns: 3}, Timestamp: ended},
	}

	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		if !bytes.Contains(data, []byte(`"timestamp"`)) || bytes.Contains(data, []byte(`"role"`)) {
			t.Errorf("%s: want a timestamp and no role, got %s", rec.Kind, data)
		}

		var got HistoryMessage
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}

		if !got.IsRecord() || got.Kind != rec.Kind || got.Summary != rec.Summary || got.FirstKept != rec.FirstKept ||
			got.Reason != rec.Reason || *got.Usage != *rec.Usage || !got.Timestamp.Equal(rec.Timestamp) {
			t.Errorf("round trip mismatch: got %+v want %+v", got, rec)
		}
	}
}

// Lazy creation: nothing on disk until the first append; 0700 dir, 0600 file.
func TestSessionFile_LazyCreateAndPermissions(t *testing.T) {
	h, path := newTestHistory(t)

	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("directory must not exist before the first append, stat err = %v", err)
	}

	addTurn(t, h, "hi")

	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}

	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("perms = dir %v file %v, want 0700/0600", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

// 3.4 — every change is an append: the earlier bytes are a prefix of the
// later file, across turns, a compaction, and the closing record.
func TestSessionFile_AppendOnly(t *testing.T) {
	h, path := newTestHistory(t)
	ctx := context.Background()

	var prev []byte
	check := func() {
		t.Helper()

		cur, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}

		if !bytes.HasPrefix(cur, prev) || len(cur) <= len(prev) {
			t.Fatalf("file was not appended to: prev %d bytes, cur %d bytes", len(prev), len(cur))
		}

		prev = cur
	}

	for i := 0; i < 5; i++ {
		addTurn(t, h, "turn")
		check()
	}

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindCompaction, Summary: "s", FirstKept: 9, Timestamp: time.Now()}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	check()

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindSessionClosed, Reason: ReasonCommand, Timestamp: time.Now()}); err != nil {
		t.Fatalf("AppendRecord: %v", err)
	}
	check()

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindTurnUsage, Timestamp: time.Now()}); err == nil {
		t.Fatal("a closed session must reject further records")
	}
}

// 3.5 — restore starts at the LATEST compaction: its summary, then the
// entries from its first kept line onward.
func TestRestore_FromLatestCompaction(t *testing.T) {
	h, path := newTestHistory(t)
	ctx := context.Background()

	for _, s := range []string{"a", "b", "c"} {
		addTurn(t, h, s) // lines 0-8
	}

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindCompaction, Summary: "first", FirstKept: 3, Timestamp: time.Now()}); err != nil { // line 9
		t.Fatalf("AppendRecord: %v", err)
	}

	addTurn(t, h, "d") // lines 10-12

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindCompaction, Summary: "second", FirstKept: 6, PreservedSkills: []string{"x"}, Timestamp: time.Now()}); err != nil { // line 13
		t.Fatalf("AppendRecord: %v", err)
	}

	r := reopen(t, path)

	summary, skills, ok := r.Summary()
	if !ok || summary != "second" || len(skills) != 1 || skills[0] != "x" {
		t.Fatalf("Summary() = %q %v %v, want the second compaction", summary, skills, ok)
	}

	view := r.Messages()
	got := make([]string, 0, len(view))
	for _, m := range view {
		got = append(got, m.Content)
	}

	want := "c ctx-c reply-c d ctx-d reply-d"
	if strings.Join(got, " ") != want {
		t.Errorf("view = %q, want %q", strings.Join(got, " "), want)
	}

	if n, _ := r.CompactionCount(); n != 2 {
		t.Errorf("CompactionCount = %d, want 2", n)
	}

	if r.TurnsSinceCompaction() != 0 {
		t.Errorf("TurnsSinceCompaction = %d, want 0", r.TurnsSinceCompaction())
	}

	// In-memory view after AppendRecord matches the restored one.
	liveView := h.Messages()
	live := make([]string, 0, len(liveView))
	for _, m := range liveView {
		live = append(live, m.Content)
	}

	if strings.Join(live, " ") != want {
		t.Errorf("live view = %q, want %q", strings.Join(live, " "), want)
	}
}

// 3.5 — a corrupt mid-file line is skipped; every valid line survives, and
// physical line indices still count it.
func TestRestore_SkipsCorruptLines(t *testing.T) {
	h, path := newTestHistory(t)
	addTurn(t, h, "a")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{not json\n")
	_ = f.Close()

	h2 := reopen(t, path)
	addTurn(t, h2, "b")

	r := reopen(t, path)
	if r.RestoredSkipped() != 1 {
		t.Errorf("RestoredSkipped = %d, want 1", r.RestoredSkipped())
	}

	if r.Len() != 6 {
		t.Fatalf("Len = %d, want 6", r.Len())
	}

	_, lines := r.View()
	if lines[3] != 4 { // "b" sits after the corrupt line 3
		t.Errorf("physical line of the first entry after the corrupt line = %d, want 4", lines[3])
	}
}

// 3.5b — a truncated tail without newline stays an isolated corrupt line:
// the next append starts on its own line and is restored.
func TestAppend_TruncatedTailDoesNotSwallowNextEntry(t *testing.T) {
	h, path := newTestHistory(t)
	addTurn(t, h, "a")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"role":"user","content":"trunc`)
	_ = f.Close()

	h2 := reopen(t, path) // new process: framing not yet checked
	if h2.RestoredSkipped() != 1 {
		t.Fatalf("RestoredSkipped = %d, want 1", h2.RestoredSkipped())
	}

	addTurn(t, h2, "b")

	lines := fileLines(t, path)
	if lines[3] != `{"role":"user","content":"trunc` {
		t.Fatalf("fragment must stay on its own line, got %q", lines[3])
	}

	r := reopen(t, path)
	if r.RestoredSkipped() != 1 || r.Len() != 6 || r.Messages()[3].Content != "b" {
		t.Errorf("restore after repair: skipped %d len %d, want 1/6 with 'b' restored", r.RestoredSkipped(), r.Len())
	}
}

// 3.5a — a failed turn keeps the completed result and gains one interrupted
// result for the unanswered call (never truncation).
func TestCloseOpenToolCallGroup_KeepsCompletedResults(t *testing.T) {
	h, path := newTestHistory(t)

	h.Add(user("turn on the light and heat"))
	h.Add(turnCtx("ctx"))
	h.Add(assistant("", call("c1", "light_turn_on"), call("c2", "climate_set_temperature")))
	h.Add(toolResult("c1", "ok"))

	if !h.HasOpenToolCallGroup() {
		t.Fatal("expected an open group before closing")
	}

	if n := h.CloseOpenToolCallGroup(); n != 1 {
		t.Fatalf("CloseOpenToolCallGroup = %d, want 1", n)
	}

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, path)
	msgs := r.Messages()
	if len(msgs) != 5 || msgs[3].ToolCallID != "c1" || msgs[3].Content != "ok" ||
		msgs[4].ToolCallID != "c2" || msgs[4].Content != InterruptedToolResult {
		t.Fatalf("unexpected transcript: %+v", msgs)
	}

	if r.HasOpenToolCallGroup() {
		t.Error("closed group must not be open after restore")
	}
}

// 3.5a — crash residue (a trailing open group) is closed at restore by
// APPENDING interrupted results to the file, before any new entry.
func TestRestore_ClosesTrailingOpenGroupByAppend(t *testing.T) {
	h, path := newTestHistory(t)

	h.Add(user("u"))
	h.Add(turnCtx("ctx"))
	h.Add(assistant("", call("a", "A"), call("b", "B")))
	h.Add(toolResult("a", "done"))

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, path)
	if r.HasOpenToolCallGroup() {
		t.Fatal("restore must close the trailing open group")
	}

	lines := fileLines(t, path)
	if len(lines) != 5 || !strings.Contains(lines[4], `"tool_call_id":"b"`) || !strings.Contains(lines[4], "Interrupted") {
		t.Fatalf("expected the interrupted result appended to the file, got %v", lines)
	}

	// A second restore finds the group complete and appends nothing.
	_ = reopen(t, path)
	if n := len(fileLines(t, path)); n != 5 {
		t.Errorf("second restore appended again: %d lines", n)
	}
}

// 3.5a — a result line corrupt in the middle of the file is repaired in the
// projection only: interrupted result inserted after the group, no file
// write, WARN naming the call id.
func TestProjection_RepairsMidFileOpenGroup(t *testing.T) {
	h, path := newTestHistory(t)

	h.Add(user("u1"))
	h.Add(turnCtx("ctx1"))
	h.Add(assistant("", call("x", "X")))
	h.Add(toolResult("x", "res"))
	h.Add(assistant("done1"))

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	addTurn(t, h, "u2")

	lines := fileLines(t, path)
	lines[3] = `{"role":"tool","tool_call_id":"x","content":"re` // corrupt the result
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	before, _ := os.ReadFile(path)

	var logBuf bytes.Buffer
	r := NewConversationHistory("s1", NewSessionFile(path)).WithLogger(zerolog.New(&logBuf))
	if err := r.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}

	msgs := r.Messages()
	if msgs[3].Role != RoleTool || msgs[3].ToolCallID != "x" || msgs[3].Content != InterruptedToolResult {
		t.Fatalf("expected an interrupted result after the group, got %+v", msgs[3])
	}

	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("the projection repair must not modify the file")
	}

	if !strings.Contains(logBuf.String(), `"tool_call_id":"x"`) {
		t.Errorf("expected a WARN naming the call id, got %s", logBuf.String())
	}
}

// Retry gate: the trailing open group of a turn in flight is NOT repaired by
// the projection — it stays open until the executor gives up.
func TestProjection_LeavesTrailingOpenGroup(t *testing.T) {
	h, _ := newTestHistory(t)

	h.Add(user("u"))
	h.Add(turnCtx("ctx"))
	h.Add(assistant("", call("a", "A")))

	if n := len(h.Messages()); n != 3 {
		t.Fatalf("trailing open group must not gain results in the projection, got %d entries", n)
	}

	if !h.HasOpenToolCallGroup() {
		t.Error("the restart gate must still see the group open")
	}
}

// Usage totals rebuild from turn_usage + compaction usage; last activity
// ignores compaction entries.
func TestRestore_UsageAndLastActivity(t *testing.T) {
	h, path := newTestHistory(t)
	ctx := context.Background()

	addTurn(t, h, "a")
	turnEnd := time.Now().Add(time.Hour).Round(0)

	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindTurnUsage, Usage: &UsageRecord{ReadTokens: 100, WrittenTokens: 10, LLMCalls: 2, Turns: 1}, Timestamp: turnEnd}); err != nil {
		t.Fatal(err)
	}

	// A later compaction must not move the last-Turn time.
	if err := h.AppendRecord(ctx, HistoryMessage{Kind: KindCompaction, Summary: "s", FirstKept: 1, Usage: &UsageRecord{ReadTokens: 50, WrittenTokens: 5, AuxCalls: 1}, Timestamp: turnEnd.Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, path)

	want := UsageRecord{ReadTokens: 150, WrittenTokens: 15, LLMCalls: 2, AuxCalls: 1, Turns: 1}
	if r.RestoredUsage() != want {
		t.Errorf("RestoredUsage = %+v, want %+v", r.RestoredUsage(), want)
	}

	if !r.LastActivity().Equal(turnEnd) {
		t.Errorf("LastActivity = %v, want %v (compaction ignored)", r.LastActivity(), turnEnd)
	}

	if !r.HasTurn() {
		t.Error("HasTurn = false, want true")
	}
}

func TestOmitOldestTurns(t *testing.T) {
	msgs := []HistoryMessage{
		assistant("tail-of-split-turn"),
		user("a"), turnCtx("ca"), assistant("", call("1", "t")), toolResult("1", "r"), assistant("ra"),
		user("b"), turnCtx("cb"), assistant("rb"),
		user("c"), turnCtx("cc"),
	}

	got := OmitOldestTurns(msgs, 1)
	if got[0].Content != "b" {
		t.Errorf("omit 1: first entry %q, want b", got[0].Content)
	}

	got = OmitOldestTurns(msgs, 10)
	if got[0].Content != "c" {
		t.Errorf("omit many: the in-flight turn must stay, first entry %q", got[0].Content)
	}

	if len(OmitOldestTurns(msgs, 0)) != len(msgs) {
		t.Error("omit 0 must return the input")
	}
}

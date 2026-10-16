//nolint:goconst // table-driven addresses and wall-clock times read best as literals
package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

// --- test doubles ---

type mapHistories struct {
	mu sync.Mutex
	m  map[string]*persistence.ConversationHistory
}

func newMapHistories() *mapHistories {
	return &mapHistories{m: map[string]*persistence.ConversationHistory{}}
}

func (p *mapHistories) For(ctx context.Context, s *Session) *persistence.ConversationHistory {
	p.mu.Lock()
	defer p.mu.Unlock()

	if h, ok := p.m[s.ID]; ok {
		return h
	}

	h := persistence.NewConversationHistory(s.ID, persistence.NewSessionFile(s.Path))
	_ = h.Restore(ctx)
	p.m[s.ID] = h

	return h
}

func (p *mapHistories) Drop(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.m, id)
}

func newTestManager(t *testing.T, root string, logger zerolog.Logger) *Manager {
	t.Helper()

	store, err := NewStore(root, logger)
	if err != nil {
		t.Fatal(err)
	}

	hist := newMapHistories()
	usage := func(string) persistence.UsageRecord { return persistence.UsageRecord{ReadTokens: 42, Turns: 3} }

	return NewManager(store, hist, usage, logger)
}

// recordTurn appends one complete Turn at t to the address's current Session.
func recordTurn(t *testing.T, m *Manager, addr Address, at time.Time) *Session {
	t.Helper()

	cur, h := m.History(context.Background(), addr)
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "hi", Timestamp: at})
	h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "hello", Timestamp: at})

	if err := h.Save(context.Background()); err != nil {
		t.Fatal(err)
	}

	return cur
}

func lastLine(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	return lines[len(lines)-1]
}

// --- 3.1 addresses ---

func TestAddressOf(t *testing.T) {
	cases := []struct {
		msg  channel.Message
		want Address
	}{
		{channel.Message{ChannelName: "cli"}, "cli"},
		{channel.Message{ChannelName: "cli", Metadata: map[string]string{"connection_id": "c1"}}, "cli"},
		{channel.Message{ChannelName: "telegram", Metadata: map[string]string{"chat_id": "111"}}, "telegram-111"},
		{channel.Message{ChannelName: "telegram", Metadata: map[string]string{"chat_id": "222"}}, "telegram-222"},
		{channel.Message{ChannelName: "serial"}, "serial"},
	}

	for _, tc := range cases {
		if got := AddressOf(tc.msg); got != tc.want {
			t.Errorf("AddressOf(%+v) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestRoute(t *testing.T) {
	ch, meta := Route("telegram-111")
	if ch != "telegram" || meta["chat_id"] != "111" {
		t.Errorf("Route(telegram-111) = %q %v", ch, meta)
	}

	ch, meta = Route("cli")
	if ch != "cli" || meta != nil {
		t.Errorf("Route(cli) = %q %v", ch, meta)
	}
}

func TestAddressDirNameSanitised(t *testing.T) {
	if got := Address("telegram-../x y").DirName(); strings.Contains(got, "..") || strings.ContainsAny(got, "/ ") {
		t.Errorf("DirName not sanitised: %q", got)
	}
}

// --- 5.4 reset rules ---

func TestDue(t *testing.T) {
	rome, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Skip("tzdata unavailable")
	}

	at := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, rome) }
	daily := Rule{Mode: config.ResetModeDaily, DailyAt: "04:00"}
	idle := Rule{Mode: config.ResetModeIdle, IdleAfter: 6 * time.Hour}
	both := Rule{Mode: config.ResetModeDailyIdle, DailyAt: "04:00", IdleAfter: 6 * time.Hour}

	cases := []struct {
		name       string
		rule       Rule
		last, now  time.Time
		wantDue    bool
		wantReason string
	}{
		{"daily: 22:00 → 08:00 next day", daily, at(2026, 9, 29, 22, 0), at(2026, 9, 30, 8, 0), true, persistence.ReasonResetDaily},
		{"daily: same side of the boundary", daily, at(2026, 9, 30, 4, 10), at(2026, 9, 30, 23, 0), false, ""},
		{"daily: before boundary, now before boundary", daily, at(2026, 9, 30, 1, 0), at(2026, 9, 30, 3, 59), false, ""},
		{"daily: 03:58 → 04:02", daily, at(2026, 9, 30, 3, 58), at(2026, 9, 30, 4, 2), true, persistence.ReasonResetDaily},
		{"never keeps the session", Rule{Mode: config.ResetModeNever}, at(2026, 9, 1, 0, 0), at(2026, 9, 30, 0, 0), false, ""},
		{"no turn is never due", daily, time.Time{}, at(2026, 9, 30, 8, 0), false, ""},
		{"idle: elapsed", idle, at(2026, 9, 30, 1, 0), at(2026, 9, 30, 7, 0), true, persistence.ReasonResetIdle},
		{"idle: not elapsed", idle, at(2026, 9, 30, 1, 0), at(2026, 9, 30, 6, 59), false, ""},
		{"daily+idle: daily wins the reason", both, at(2026, 9, 29, 20, 0), at(2026, 9, 30, 9, 0), true, persistence.ReasonResetDaily},
		{"daily+idle: idle only", both, at(2026, 9, 30, 5, 0), at(2026, 9, 30, 11, 30), true, persistence.ReasonResetIdle},
		// DST spring forward (2026-03-29 02:00 → 03:00): a 02:30 boundary
		// normalises to 03:30.
		{"DST forward: nonexistent boundary", Rule{Mode: config.ResetModeDaily, DailyAt: "02:30"}, at(2026, 3, 29, 1, 0), at(2026, 3, 29, 3, 20), false, ""},
		{"DST forward: past normalised boundary", Rule{Mode: config.ResetModeDaily, DailyAt: "02:30"}, at(2026, 3, 29, 1, 0), at(2026, 3, 29, 3, 40), true, persistence.ReasonResetDaily},
		// DST fall back (2026-10-25 03:00 → 02:00): one boundary per day.
		{"DST back: repeated hour", Rule{Mode: config.ResetModeDaily, DailyAt: "02:30"}, at(2026, 10, 25, 1, 0), at(2026, 10, 25, 9, 0), true, persistence.ReasonResetDaily},
		// Restart: the last Turn time comes from the file, the rule is pure.
		{"restart: days later", daily, at(2026, 9, 20, 12, 0), at(2026, 9, 30, 12, 0), true, persistence.ReasonResetDaily},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, due := Due(tc.rule, tc.last, tc.now)
			if due != tc.wantDue || reason != tc.wantReason {
				t.Errorf("Due = (%q, %v), want (%q, %v)", reason, due, tc.wantReason, tc.wantDue)
			}
		})
	}
}

// --- 3.3 store / resolution ---

func TestResolve_NewerOpenFileIsCurrent(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root, zerolog.Nop())

	first := recordTurn(t, m, "cli", time.Now())
	if _, ok := m.StartNew(context.Background(), "cli"); !ok {
		t.Fatal("StartNew on a session with a Turn must start a new one")
	}

	second := recordTurn(t, m, "cli", time.Now())

	// A fresh manager (restart) resolves the newer, unclosed file.
	m2 := newTestManager(t, root, zerolog.Nop())
	if got := m2.Current(context.Background(), "cli"); got.ID != second.ID || got.ID == first.ID {
		t.Errorf("current = %s, want the newer file %s", got.ID, second.ID)
	}
}

// Crash between end and start: the newest file ends with a closing record,
// so after restart the address has no current Session and a fresh one
// starts, created lazily.
func TestResolve_ClosedWithoutNextStartsFresh(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root, zerolog.Nop())

	ended := recordTurn(t, m, "telegram-7", time.Now())
	m.StartNew(context.Background(), "telegram-7") // closing record written, next has no file

	m2 := newTestManager(t, root, zerolog.Nop())

	cur := m2.Current(context.Background(), "telegram-7")
	if cur.ID == ended.ID {
		t.Fatal("a closed file must not be resolved as current")
	}

	if _, err := os.Stat(cur.Path); !os.IsNotExist(err) {
		t.Errorf("the new session file must be created lazily, stat err = %v", err)
	}

	if !strings.HasPrefix(filepath.Base(cur.Path), cur.StartedAt.Local().Format(fileTimeLayout)+"_"+cur.ID) {
		t.Errorf("unexpected file name %q", filepath.Base(cur.Path))
	}
}

// A truncated last line is crash residue, not a closing record: the file
// stays current.
func TestResolve_TruncatedLastLineStaysCurrent(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root, zerolog.Nop())
	cur := recordTurn(t, m, "cli", time.Now())

	f, _ := os.OpenFile(cur.Path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"kind":"session_clo`)
	_ = f.Close()

	m2 := newTestManager(t, root, zerolog.Nop())
	if got := m2.Current(context.Background(), "cli"); got.ID != cur.ID {
		t.Errorf("current = %s, want %s", got.ID, cur.ID)
	}
}

// A non-newest file without a closing record is treated as ended, with a
// WARN.
func TestResolve_NonNewestUnclosedIsEndedWithWarn(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root, zerolog.Nop())
	older := recordTurn(t, m, "cli", time.Now())

	// Tamper: strip the closing record written by StartNew.
	m.StartNew(context.Background(), "cli")
	newer := recordTurn(t, m, "cli", time.Now())

	data, _ := os.ReadFile(older.Path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	_ = os.WriteFile(older.Path, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o600) //nolint:gosec // test file under t.TempDir()

	var buf bytes.Buffer
	m2 := newTestManager(t, root, zerolog.New(&buf))

	if got := m2.Current(context.Background(), "cli"); got.ID != newer.ID {
		t.Errorf("current = %s, want the newest %s", got.ID, newer.ID)
	}

	if !strings.Contains(buf.String(), "treated as ended") {
		t.Errorf("expected a WARN, got %q", buf.String())
	}
}

// --- 3.6 manager ---

func TestStartNew_ClosingRecordWithUsage(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	ended := recordTurn(t, m, "cli", time.Now())

	var hooked []string
	m.OnEnd(func(_ context.Context, s *Session) { hooked = append(hooked, s.ID) })

	next, started := m.StartNew(context.Background(), "cli")
	if !started || next.ID == ended.ID {
		t.Fatalf("StartNew = (%v, %v)", next, started)
	}

	last := lastLine(t, ended.Path)
	if !strings.Contains(last, `"kind":"session_closed"`) || !strings.Contains(last, `"reason":"command"`) ||
		!strings.Contains(last, `"read_tokens":42`) || !strings.Contains(last, `"ended_at"`) {
		t.Errorf("closing record = %s", last)
	}

	if len(hooked) != 1 || hooked[0] != ended.ID {
		t.Errorf("end hooks = %v", hooked)
	}
}

func TestStartNew_EmptySessionIsNoOp(t *testing.T) {
	root := t.TempDir()
	m := newTestManager(t, root, zerolog.Nop())

	cur := m.Current(context.Background(), "cli")
	if next, started := m.StartNew(context.Background(), "cli"); started || next.ID != cur.ID {
		t.Fatalf("StartNew on an empty session = (%s, %v), want (%s, false)", next.ID, started, cur.ID)
	}

	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("an empty session must leave no files, got %v", entries)
	}
}

func TestEvaluateReset_MessageOriented(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	now := time.Now()
	ended := recordTurn(t, m, "telegram-1", now.Add(-30*time.Hour))

	rule := Rule{Mode: config.ResetModeDaily, DailyAt: "04:00"}
	if !m.EvaluateReset(context.Background(), "telegram-1", rule, now) {
		t.Fatal("expected a daily reset")
	}

	if !strings.Contains(lastLine(t, ended.Path), `"reason":"reset_daily"`) {
		t.Errorf("closing record = %s", lastLine(t, ended.Path))
	}

	if m.EvaluateReset(context.Background(), "telegram-1", rule, now) {
		t.Error("the fresh session must not be reset again")
	}
}

// 3.6a — a compaction entry is not activity: it cannot defer an idle reset.
func TestLastTurnIgnoresCompaction(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	now := time.Now()

	cur := recordTurn(t, m, "cli", now.Add(-7*time.Hour))
	_, h := m.History(context.Background(), "cli")

	if err := h.AppendRecord(context.Background(), persistence.HistoryMessage{Kind: persistence.KindCompaction, Summary: "s", FirstKept: 1, Timestamp: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	rule := Rule{Mode: config.ResetModeIdle, IdleAfter: 6 * time.Hour}
	if id, pending := m.RequestReset(context.Background(), "cli", rule, now); !pending || id == cur.ID {
		t.Errorf("RequestReset = (%s, %v): the /compact must not defer the idle reset", id, pending)
	}
}

// 5.6 — a live Connection never resets; a reset found due at a Connection
// start is pending (same id on repeated handshakes) and applied by the
// worker after the Turn in flight is recorded in the OLD Session.
func TestRequestReset_ZombieTurnRace(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	ctx := context.Background()
	day := time.Now()
	y, mo, d := day.Date()
	at := func(h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, time.Local) }
	rule := Rule{Mode: config.ResetModeDaily, DailyAt: "04:00"}

	// The zombie Turn started at 03:58: its user message is in memory.
	old, h := m.History(ctx, "cli")
	h.Add(persistence.HistoryMessage{Role: persistence.RoleUser, Content: "slow", Timestamp: at(3, 58)})

	// New handshake at 04:02 while the Turn runs.
	id1, pending := m.RequestReset(ctx, "cli", rule, at(4, 2))
	if !pending || id1 == old.ID {
		t.Fatalf("RequestReset = (%s, %v), want a pending new id", id1, pending)
	}

	// Double reconnect: same pending id.
	if id2, _ := m.RequestReset(ctx, "cli", rule, at(4, 3)); id2 != id1 {
		t.Fatalf("second handshake got %s, want the same pending id %s", id2, id1)
	}

	if eff, p := m.EffectiveID(ctx, "cli"); eff != id1 || !p {
		t.Errorf("EffectiveID = (%s, %v)", eff, p)
	}

	// The Turn completes at 04:05 in the OLD Session.
	if m.Current(ctx, "cli").ID != old.ID {
		t.Fatal("the current session must not change before the worker applies the reset")
	}

	h.Add(persistence.HistoryMessage{Role: persistence.RoleAssistant, Content: "done", Timestamp: at(4, 5)})
	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	// Worker, before its next item.
	if ended := m.ApplyPending(ctx, "cli"); ended == nil || ended.ID != old.ID {
		t.Fatalf("ApplyPending ended %v, want %s", ended, old.ID)
	}

	if m.Current(ctx, "cli").ID != id1 {
		t.Errorf("current = %s, want the pre-assigned %s", m.Current(ctx, "cli").ID, id1)
	}

	data, _ := os.ReadFile(old.Path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], `"done"`) || !strings.Contains(lines[2], `"reason":"reset_daily"`) {
		t.Errorf("old session file = %v: the Turn must precede the closing record", lines)
	}

	if strings.Count(string(data), "session_closed") != 1 {
		t.Error("exactly one closing record")
	}

	if m.ApplyPending(ctx, "cli") != nil {
		t.Error("nothing pending after application")
	}
}

func TestRequestReset_NotDueReturnsCurrent(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	now := time.Now()
	cur := recordTurn(t, m, "cli", now.Add(-time.Minute))

	rule := Rule{Mode: config.ResetModeIdle, IdleAfter: time.Hour}
	if id, pending := m.RequestReset(context.Background(), "cli", rule, now); pending || id != cur.ID {
		t.Errorf("RequestReset = (%s, %v), want (%s, false)", id, pending, cur.ID)
	}
}

// --- 6.1 retention ---

func writeSessionFile(t *testing.T, root string, addr Address, start time.Time, closed bool, size int) string {
	t.Helper()

	store, _ := NewStore(root, zerolog.Nop())

	s, err := store.newSession(addr, start)
	if err != nil {
		t.Fatal(err)
	}

	_ = os.MkdirAll(filepath.Dir(s.Path), 0o700)

	body := `{"role":"user","content":"` + strings.Repeat("x", size) + `","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	if closed {
		body += `{"kind":"session_closed","reason":"command","timestamp":"2026-01-01T00:00:00Z"}` + "\n"
	}

	if err := os.WriteFile(s.Path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Millisecond) // distinct UUIDv7 milliseconds

	return s.Path
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func TestRetain_DaysWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	old := writeSessionFile(t, root, "cli", now.AddDate(0, 0, -31), true, 10)
	recent := writeSessionFile(t, root, "cli", now.AddDate(0, 0, -2), true, 10)
	current := writeSessionFile(t, root, "cli", now.AddDate(0, 0, -40), false, 10) // newest by uuid, open
	stray := filepath.Join(root, "cli", "notes.txt")
	_ = os.WriteFile(stray, []byte("keep"), 0o600)

	store, _ := NewStore(root, zerolog.Nop())
	if _, err := store.Retain(context.Background(), RetentionPolicy{Days: 30}, now); err != nil {
		t.Fatal(err)
	}

	if exists(old) || !exists(recent) || !exists(current) || !exists(stray) {
		t.Errorf("old=%v recent=%v current=%v stray=%v, want false/true/true/true", exists(old), exists(recent), exists(current), exists(stray))
	}
}

func TestRetain_SizeCapSkipsCurrentSessions(t *testing.T) {
	root := t.TempDir()
	now := time.Now()

	a1 := writeSessionFile(t, root, "cli", now, true, 1000)
	b1 := writeSessionFile(t, root, "telegram-1", now, true, 1000)
	a2 := writeSessionFile(t, root, "cli", now, true, 1000)
	curA := writeSessionFile(t, root, "cli", now, false, 5000)
	curB := writeSessionFile(t, root, "telegram-1", now, false, 5000)

	store, _ := NewStore(root, zerolog.Nop())
	if _, err := store.Retain(context.Background(), RetentionPolicy{Days: 30, MaxTotalSize: 2500}, now); err != nil {
		t.Fatal(err)
	}

	// Ended total 3 × ~1.1KB > 2500: the oldest (a1) goes, across addresses.
	if exists(a1) || !exists(b1) || !exists(a2) || !exists(curA) || !exists(curB) {
		t.Errorf("a1=%v b1=%v a2=%v curA=%v curB=%v", exists(a1), exists(b1), exists(a2), exists(curA), exists(curB))
	}
}

func TestRetain_ZeroDaysDeletesEnded(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	ended := writeSessionFile(t, root, "cli", now, true, 10)
	lastClosed := writeSessionFile(t, root, "cli", now, true, 10) // newest but closed: ended too

	store, _ := NewStore(root, zerolog.Nop())
	if _, err := store.Retain(context.Background(), RetentionPolicy{Days: 0}, now); err != nil {
		t.Fatal(err)
	}

	if exists(ended) || exists(lastClosed) {
		t.Error("retention_days 0 deletes every ended session")
	}
}

// 3.6 — handshake-side calls (RequestReset, EffectiveID) race the worker's
// transitions (ApplyPending, StartNew) without data races or lost closing
// records.
func TestManager_ConcurrentHandshakesAndWorker(t *testing.T) {
	m := newTestManager(t, t.TempDir(), zerolog.Nop())
	ctx := context.Background()
	rule := Rule{Mode: config.ResetModeIdle, IdleAfter: time.Millisecond}

	recordTurn(t, m, "cli", time.Now().Add(-time.Hour))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for {
				select {
				case <-stop:
					return
				default:
					m.RequestReset(ctx, "cli", rule, time.Now())
					m.EffectiveID(ctx, "cli")
				}
			}
		}()
	}

	ended := 0
	for i := 0; i < 20; i++ {
		if m.ApplyPending(ctx, "cli") != nil {
			ended++
		}

		recordTurn(t, m, "cli", time.Now().Add(-time.Hour))
	}

	close(stop)
	wg.Wait()

	if ended == 0 {
		t.Fatal("expected at least one pending reset applied")
	}

	files, err := listFiles(filepath.Join(m.Store().Root(), "cli"))
	if err != nil {
		t.Fatal(err)
	}

	closed := 0
	for _, f := range files {
		if c, _ := isClosed(f.path); c {
			closed++
		}
	}

	if closed != ended {
		t.Errorf("closing records = %d, applied resets = %d", closed, ended)
	}
}

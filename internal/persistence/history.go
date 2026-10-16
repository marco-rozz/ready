package persistence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// MessageRole represents the role of a message sender.
type MessageRole string

const (
	// RoleUser represents a user message.
	RoleUser MessageRole = "user"
	// RoleAssistant represents an assistant response.
	RoleAssistant MessageRole = "assistant"
	// RoleSystem represents a system instruction.
	RoleSystem MessageRole = "system"
	// RoleTool represents a tool result answering an assistant tool call.
	RoleTool MessageRole = "tool"
)

// MessageKind distinguishes special-purpose lines: the turn-context message
// (a user-role message) and the non-message record kinds (no role).
type MessageKind string

const (
	// KindTurnContext marks the per-turn turn-context message (user role),
	// recorded immediately after the turn's user message
	// (the session-transcript-persistence §D6). It is NOT a turn
	// boundary.
	KindTurnContext MessageKind = "turn_context"
	// KindCompaction is a Compaction record (session-lifecycle D3): summary,
	// first_kept, preserved_skills, tokens_before/after, usage.
	KindCompaction MessageKind = "compaction"
	// KindTurnUsage is the usage delta of one Turn (session-lifecycle D7).
	KindTurnUsage MessageKind = "turn_usage"
	// KindSessionClosed is the closing record of a Session: ended_at, reason,
	// and the Session's final usage totals. Nothing follows it in the file.
	KindSessionClosed MessageKind = "session_closed"
)

// Session end reasons carried by the closing record.
const (
	ReasonCommand    = "command"
	ReasonResetDaily = "reset_daily"
	ReasonResetIdle  = "reset_idle"
)

// InterruptedToolResult is the fixed content of the result recorded for a
// tool call that has no recorded result. English product
// string; the [Transient] prefix mirrors the error-digest observation shape.
const InterruptedToolResult = "[Transient] Interrupted: no result was recorded for this call; it may or may not have executed. Verify the current state before retrying."

// ToolCallRecord is one tool call emitted by an assistant message. ID is
// always non-empty and protocol-portable once recorded; Name is the canonical
// executed tool name; Arguments is the raw JSON string, verbatim.
type ToolCallRecord struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UsageRecord carries token usage persisted in the Session file: a Turn's
// delta (turn_usage), a summariser call (compaction), or the Session's final
// totals (session_closed).
type UsageRecord struct {
	ReadTokens    int `json:"read_tokens"`
	WrittenTokens int `json:"written_tokens"`
	CachedTokens  int `json:"cached_tokens,omitempty"`
	LLMCalls      int `json:"llm_calls"`
	AuxCalls      int `json:"aux_calls"`
	Turns         int `json:"turns"`
}

// Add returns the field-wise sum of u and o.
func (u UsageRecord) Add(o UsageRecord) UsageRecord {
	return UsageRecord{
		ReadTokens:    u.ReadTokens + o.ReadTokens,
		WrittenTokens: u.WrittenTokens + o.WrittenTokens,
		CachedTokens:  u.CachedTokens + o.CachedTokens,
		LLMCalls:      u.LLMCalls + o.LLMCalls,
		AuxCalls:      u.AuxCalls + o.AuxCalls,
		Turns:         u.Turns + o.Turns,
	}
}

// HistoryMessage is one entry of a Session file: a transcript message
// (user/assistant/system lines, tool results, the turn-context line) or a
// record (compaction, turn_usage, session_closed — no role). Every entry
// carries a timestamp.
type HistoryMessage struct {
	Role       MessageRole      `json:"role,omitempty"`
	Kind       MessageKind      `json:"kind,omitempty"`
	Content    string           `json:"content,omitempty"`
	Timestamp  time.Time        `json:"timestamp"`
	ToolCalls  []ToolCallRecord `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`

	// Compaction record fields. FirstKept is the 0-based PHYSICAL line index
	// (blank and corrupt lines counted) of the first kept entry; a compaction
	// always summarises at least one entry, so it is never 0.
	Summary         string   `json:"summary,omitempty"`
	FirstKept       int      `json:"first_kept,omitempty"`
	PreservedSkills []string `json:"preserved_skills,omitempty"`
	TokensBefore    int      `json:"tokens_before,omitempty"`
	TokensAfter     int      `json:"tokens_after,omitempty"`

	// Usage rides turn_usage, compaction and session_closed records.
	Usage *UsageRecord `json:"usage,omitempty"`

	// Closing record fields.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	Reason  string     `json:"reason,omitempty"`
}

// IsTurnBoundary reports whether m opens a new turn unit: a user message
// that is not the turn-context line.
func (m HistoryMessage) IsTurnBoundary() bool {
	return m.Role == RoleUser && m.Kind != KindTurnContext
}

// IsRecord reports whether m is a non-message record (compaction,
// turn_usage, session_closed) — never part of the model's view.
func (m HistoryMessage) IsRecord() bool {
	switch m.Kind {
	case KindCompaction, KindTurnUsage, KindSessionClosed:
		return true
	default:
		return false
	}
}

// EstimatedChars approximates the prompt size of m in characters (content,
// tool-call names and arguments, call ids). Callers divide by 4 for tokens.
func (m HistoryMessage) EstimatedChars() int {
	n := len(m.Role) + len(m.Content) + len(m.ToolCallID)
	for _, tc := range m.ToolCalls {
		n += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
	}

	return n
}

// unsaved marks a view entry not yet written to the file.
const unsaved = -1

// ConversationHistory is the model's view of one Session: the
// latest compaction summary, the entries from its first kept entry onward,
// and new entries — each annotated with its physical line in the file. It is
// the only writer of the Session file: messages, records, and interrupted
// results are all appended through it, so the append watermark and the
// physical line count stay consistent. The file is append-only: nothing here
// rewrites, truncates, or renames it.
type ConversationHistory struct {
	mu        sync.RWMutex
	sessionID string
	file      *SessionFile
	logger    zerolog.Logger

	summary     *HistoryMessage // latest compaction entry, nil when none
	summaryLine int             // physical line of the latest compaction entry

	messages []HistoryMessage // the view (records excluded)
	lines    []int            // physical line per view entry; unsaved = -1

	lastSavedIndex int // first view entry not yet written
	lineCount      int // physical lines in the file

	restoredSkipped int
	restoredUsage   UsageRecord
	lastActivity    time.Time // latest non-compaction entry timestamp
	hasTurn         bool      // at least one turn boundary recorded
	closed          bool      // closing record present
	compactions     int
	lastCompaction  time.Time

	warnedRepairs map[string]bool // projection-repair WARNs already logged
}

// NewConversationHistory creates the history of Session sessionID backed by
// file (nil = in-memory only, used by tests).
func NewConversationHistory(sessionID string, file *SessionFile) *ConversationHistory {
	return &ConversationHistory{
		sessionID:     sessionID,
		file:          file,
		logger:        zerolog.Nop(),
		warnedRepairs: map[string]bool{},
	}
}

// WithLogger attaches a component-scoped logger (called by the daemon wiring;
// the default is a Nop logger so tests and direct uses stay silent).
func (h *ConversationHistory) WithLogger(logger zerolog.Logger) *ConversationHistory {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.logger = logger

	return h
}

// SessionID returns the Session this history belongs to.
func (h *ConversationHistory) SessionID() string { return h.sessionID }

// Path returns the Session file path ("" for an in-memory history).
func (h *ConversationHistory) Path() string {
	if h.file == nil {
		return ""
	}

	return h.file.Path()
}

// Add appends a message to the view; the next Save writes it.
func (h *ConversationHistory) Add(msg HistoryMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages = append(h.messages, msg)
	h.lines = append(h.lines, unsaved)
	h.observeLocked(msg)
}

// observeLocked updates the activity bookkeeping for a new entry.
func (h *ConversationHistory) observeLocked(msg HistoryMessage) {
	if msg.Kind != KindCompaction && msg.Timestamp.After(h.lastActivity) {
		h.lastActivity = msg.Timestamp
	}

	if msg.IsTurnBoundary() {
		h.hasTurn = true
	}
}

// Save appends every unsaved view entry to the file in one write. The
// watermark advances per fully written line, so a write failing midway never
// causes a later Save to re-append what already reached the file.
func (h *ConversationHistory) Save(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.saveLocked(ctx)
}

func (h *ConversationHistory) saveLocked(ctx context.Context) error {
	if h.file == nil || h.lastSavedIndex >= len(h.messages) {
		return nil
	}

	pending := make([][]byte, 0, len(h.messages)-h.lastSavedIndex)
	for i := h.lastSavedIndex; i < len(h.messages); i++ {
		data, err := json.Marshal(h.messages[i])
		if err != nil {
			return fmt.Errorf("failed to marshal history message: %w", err)
		}

		pending = append(pending, data)
	}

	n, err := h.file.Append(ctx, pending...)

	// Advance the watermark over every line that reached the file intact.
	written := 0
	for _, line := range pending {
		if n < written+len(line)+1 {
			break
		}

		written += len(line) + 1
		h.lines[h.lastSavedIndex] = h.lineCount
		h.lineCount++
		h.lastSavedIndex++
	}

	if err != nil {
		return fmt.Errorf("failed to append history: %w", err)
	}

	return nil
}

// AppendRecord flushes pending messages, then appends rec (a compaction,
// turn_usage, or session_closed record). A compaction record rebuilds the
// view: summary = rec, entries = those from rec.FirstKept onward.
func (h *ConversationHistory) AppendRecord(ctx context.Context, rec HistoryMessage) error {
	if !rec.IsRecord() {
		return fmt.Errorf("failed to append record: kind %q is not a record", rec.Kind)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return fmt.Errorf("failed to append record: session %q is closed", h.sessionID)
	}

	if err := h.saveLocked(ctx); err != nil {
		return err
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal %s record: %w", rec.Kind, err)
	}

	if h.file != nil {
		if _, err := h.file.Append(ctx, data); err != nil {
			return fmt.Errorf("failed to append %s record: %w", rec.Kind, err)
		}
	}

	line := h.lineCount
	h.lineCount++
	h.applyRecordLocked(rec, line)

	return nil
}

// applyRecordLocked folds a record into the view and the bookkeeping.
func (h *ConversationHistory) applyRecordLocked(rec HistoryMessage, line int) {
	h.observeLocked(rec)

	switch rec.Kind {
	case KindCompaction:
		cp := rec
		h.summary = &cp
		h.summaryLine = line
		h.compactions++
		h.lastCompaction = rec.Timestamp

		keptMsgs := make([]HistoryMessage, 0, len(h.messages))
		keptLines := make([]int, 0, len(h.lines))
		for i, l := range h.lines {
			if l == unsaved || l >= rec.FirstKept {
				keptMsgs = append(keptMsgs, h.messages[i])
				keptLines = append(keptLines, l)
			}
		}

		// Every saved entry precedes the unsaved ones, so the watermark is
		// the count of kept saved entries.
		saved := 0
		for _, l := range keptLines {
			if l != unsaved {
				saved++
			}
		}

		h.messages, h.lines, h.lastSavedIndex = keptMsgs, keptLines, saved
	case KindSessionClosed:
		h.closed = true
	case KindTurnUsage:
	}
}

// Messages returns the view to send to the model (the summary is exposed
// separately, see Summary): a copy of the entries, with an interrupted result
// inserted after any tool-call group missing a result that is NOT the
// trailing group (a result line lost to corruption mid-file). The file is not
// modified; the first repair of each call id is logged at WARN. The trailing
// open group is left as is — it is the turn in flight (restart gate).
func (h *ConversationHistory) Messages() []HistoryMessage {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.projectLocked()
}

func (h *ConversationHistory) projectLocked() []HistoryMessage {
	out := make([]HistoryMessage, 0, len(h.messages))
	msgs := h.messages

	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		out = append(out, m)

		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}

		// The group is the assistant message plus the contiguous tool
		// results that follow it.
		end := i + 1
		answered := map[string]bool{}
		for end < len(msgs) && msgs[end].Role == RoleTool {
			answered[msgs[end].ToolCallID] = true
			out = append(out, msgs[end])
			end++
		}

		if end < len(msgs) { // not the trailing group
			for _, tc := range m.ToolCalls {
				if answered[tc.ID] {
					continue
				}

				out = append(out, interruptedResult(tc.ID, m.Timestamp))

				if !h.warnedRepairs[tc.ID] {
					h.warnedRepairs[tc.ID] = true
					h.logger.Warn().
						Str("session", h.sessionID).
						Str("tool_call_id", tc.ID).
						Msg("tool call without a recorded result mid-session — interrupted result inserted in the prompt projection")
				}
			}
		}

		i = end - 1
	}

	return out
}

// interruptedResult builds the interrupted tool result for callID.
func interruptedResult(callID string, at time.Time) HistoryMessage {
	return HistoryMessage{
		Role:       RoleTool,
		Content:    InterruptedToolResult,
		ToolCallID: callID,
		Timestamp:  at,
	}
}

// View returns a copy of the raw view entries (no projection repair) with
// their physical line indices (unsaved = -1). Used by the compaction planner.
func (h *ConversationHistory) View() ([]HistoryMessage, []int) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	msgs := make([]HistoryMessage, len(h.messages))
	copy(msgs, h.messages)

	lines := make([]int, len(h.lines))
	copy(lines, h.lines)

	return msgs, lines
}

// Summary returns the latest compaction summary and its preserved skill
// names; ok is false when the Session was never compacted.
func (h *ConversationHistory) Summary() (summary string, preservedSkills []string, ok bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.summary == nil {
		return "", nil, false
	}

	return h.summary.Summary, append([]string(nil), h.summary.PreservedSkills...), true
}

// openGroupStart returns the index of the TRAILING assistant message whose
// tool calls do not all have a recorded result, or -1 when the transcript
// ends in a complete state. The group is trailing only when nothing but its
// tool results follows it: an open group followed by later entries (a result
// line lost mid-file) is repaired by the projection instead (the design doc rule
// 4). Results may arrive in any order.
func openGroupStart(msgs []HistoryMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == RoleTool {
			continue
		}

		if m.Role != RoleAssistant || len(m.ToolCalls) == 0 {
			return -1
		}

		answered := make(map[string]bool, len(m.ToolCalls))
		for _, later := range msgs[i+1:] {
			answered[later.ToolCallID] = true
		}

		for _, tc := range m.ToolCalls {
			if !answered[tc.ID] {
				return i
			}
		}

		return -1
	}

	return -1
}

// HasOpenToolCallGroup reports whether the transcript ends in a tool-call
// group missing at least one result — the state in which a turn cannot be
// resumed (a tool may be executing, or its result was lost).
func (h *ConversationHistory) HasOpenToolCallGroup() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return openGroupStart(h.messages) >= 0
}

// CloseOpenToolCallGroup closes a trailing open tool-call group by adding one
// interrupted result per unanswered call: the calls and
// the results that did complete stay visible to the model. The next Save
// writes them. Callers run it only once no further attempt of the turn will
// run. Returns the number of interrupted results added.
func (h *ConversationHistory) CloseOpenToolCallGroup() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.closeOpenGroupLocked(time.Now())
}

func (h *ConversationHistory) closeOpenGroupLocked(at time.Time) int {
	idx := openGroupStart(h.messages)
	if idx < 0 {
		return 0
	}

	answered := map[string]bool{}
	for _, later := range h.messages[idx+1:] {
		if later.Role == RoleTool {
			answered[later.ToolCallID] = true
		}
	}

	added := 0
	for _, tc := range h.messages[idx].ToolCalls {
		if answered[tc.ID] {
			continue
		}

		res := interruptedResult(tc.ID, at)
		h.messages = append(h.messages, res)
		h.lines = append(h.lines, unsaved)
		h.observeLocked(res)
		added++
	}

	return added
}

// Len returns the number of view entries.
func (h *ConversationHistory) Len() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return len(h.messages)
}

// Clear drops the in-memory view and its bookkeeping (the file is left as
// is).
func (h *ConversationHistory) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages, h.lines = nil, nil
	h.summary = nil
	h.lastSavedIndex = 0
	h.restoredSkipped = 0
}

// RestoredSkipped reports how many corrupt lines the last Restore skipped.
func (h *ConversationHistory) RestoredSkipped() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.restoredSkipped
}

// RestoredUsage returns the usage rebuilt by the last Restore from the
// file's turn_usage records and compaction usage.
func (h *ConversationHistory) RestoredUsage() UsageRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.restoredUsage
}

// LastActivity returns the timestamp of the latest entry other than a
// compaction entry (the Session's last Turn time — session-lifecycle D6);
// zero when the Session recorded nothing.
func (h *ConversationHistory) LastActivity() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.lastActivity
}

// HasTurn reports whether at least one Turn was recorded.
func (h *ConversationHistory) HasTurn() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.hasTurn
}

// Closed reports whether the closing record was written or restored.
func (h *ConversationHistory) Closed() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.closed
}

// CompactionCount returns the number of compaction entries in the Session
// and the timestamp of the last one.
func (h *ConversationHistory) CompactionCount() (int, time.Time) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.compactions, h.lastCompaction
}

// TurnsSinceCompaction counts the Turns recorded after the latest compaction
// entry (all Turns when never compacted).
func (h *ConversationHistory) TurnsSinceCompaction() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	n := 0
	for i, m := range h.messages {
		if !m.IsTurnBoundary() {
			continue
		}

		if h.summary == nil || h.lines[i] == unsaved || h.lines[i] > h.summaryLine {
			n++
		}
	}

	return n
}

// Restore reads the Session file and materialises the view from the latest
// compaction entry forward: its summary, then every message entry
// from its first kept entry onward; the whole file without a compaction.
// Corrupt lines are skipped with a WARN (restore never aborts). Usage totals
// are rebuilt from turn_usage records plus compaction usage. A trailing open
// tool-call group (crash residue) is closed by APPENDING one interrupted
// result per unanswered call to the file before any new entry (the design doc
// rule 3). The view is never trimmed by a turn budget.
func (h *ConversationHistory) Restore(ctx context.Context) error {
	if h.file == nil {
		return nil
	}

	data, err := h.file.Read(ctx)
	if err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages, h.lines = nil, nil
	h.summary = nil
	h.restoredSkipped = 0
	h.restoredUsage = UsageRecord{}
	h.lastActivity = time.Time{}
	h.hasTurn, h.closed = false, false
	h.compactions = 0

	var entries []HistoryMessage
	var entryLines []int

	rawLines := bytes.Split(data, []byte{'\n'})
	if n := len(rawLines); n > 0 && len(rawLines[n-1]) == 0 {
		rawLines = rawLines[:n-1] // the trailing newline opens no line
	}

	offset := 0
	for i, raw := range rawLines {
		lineOffset := offset
		offset += len(raw) + 1

		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var msg HistoryMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			// A truncated or corrupt line (crash mid-append) must not discard
			// the Session — skip it with a WARN naming the session, line
			// number, and byte offset so the anomaly is observable.
			h.restoredSkipped++
			h.logger.Warn().
				Str("session", h.sessionID).
				Int("line", i+1).
				Int("byte_offset", lineOffset).
				Err(err).
				Msg("skipped corrupt session file line")

			continue
		}

		entries = append(entries, msg)
		entryLines = append(entryLines, i)
	}

	h.lineCount = len(rawLines)

	// Latest compaction entry: the view starts at its first kept entry.
	firstKept, compactionAt := 0, -1
	for i, e := range entries {
		switch e.Kind {
		case KindCompaction:
			compactionAt = i
			firstKept = e.FirstKept
			h.compactions++
			h.lastCompaction = e.Timestamp
		case KindTurnUsage:
		case KindSessionClosed:
			h.closed = true
		case KindTurnContext:
		}

		if e.Usage != nil && (e.Kind == KindTurnUsage || e.Kind == KindCompaction) {
			h.restoredUsage = h.restoredUsage.Add(*e.Usage)
		}

		h.observeLocked(e)
	}

	if compactionAt >= 0 {
		cp := entries[compactionAt]
		h.summary = &cp
		h.summaryLine = entryLines[compactionAt]
	}

	for i, e := range entries {
		if e.IsRecord() || entryLines[i] < firstKept {
			continue
		}

		h.messages = append(h.messages, e)
		h.lines = append(h.lines, entryLines[i])
	}

	h.lastSavedIndex = len(h.messages)

	if !h.closed {
		if n := h.closeOpenGroupLocked(time.Now()); n > 0 {
			h.logger.Warn().
				Str("session", h.sessionID).
				Int("interrupted_results", n).
				Msg("restored session ended in an open tool-call group — interrupted results appended")

			if err := h.saveLocked(ctx); err != nil {
				return fmt.Errorf("failed to close restored tool-call group: %w", err)
			}
		}
	}

	return nil
}

// OmitOldestTurns returns msgs without its oldest n whole Turns: the cut
// lands immediately before a turn-boundary user line, so a tool call is never
// separated from its result, and the last Turn (the one in flight) is never
// omitted. Leading entries before the first boundary (a kept tail starting
// mid-Turn) go with the first omitted Turn. The input is not modified.
func OmitOldestTurns(msgs []HistoryMessage, n int) []HistoryMessage {
	if n <= 0 {
		return msgs
	}

	var bounds []int
	for i, m := range msgs {
		if m.IsTurnBoundary() {
			bounds = append(bounds, i)
		}
	}

	if len(bounds) <= 1 {
		return msgs
	}

	n = min(n, len(bounds)-1)

	return msgs[bounds[n]:]
}

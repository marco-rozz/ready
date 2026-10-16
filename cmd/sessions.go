package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	agentskillsSpec "github.com/flexigpt/agentskills-go/runtime/spec"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/bootstrap"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/formatter"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/skills"
	"github.com/marco-rozz/ready/internal/usage"
)

// Session command and lifecycle replies (session-lifecycle): fixed English
// product strings, delivered bypassing the post-step formatter like the
// terminal failure messages (mechanical channel adaptation retained).
const (
	msgSessionStarted    = "New session started."
	msgSessionAlreadyNew = "Session already new."
	msgSessionReset      = "New session started; the previous one was archived."
	msgCompacted         = "Context compacted."
	msgCompactionFailed  = "Compaction failed; the session is unchanged."
	msgNothingToCompact  = "Nothing to compact."
)

// Session commands recognised before any agent processing.
const (
	cmdNew     = "new"
	cmdCompact = "compact"
)

// parseSessionCommand recognises `/new` (exact) and `/compact [instructions]`
// on the trimmed content. Telegram's `/cmd@<bot>` suffix is stripped by the
// Telegram channel, which knows its bot username.
func parseSessionCommand(content string) (cmd, arg string, ok bool) {
	text := strings.TrimSpace(content)

	if text == "/"+cmdNew {
		return cmdNew, "", true
	}

	if rest, found := strings.CutPrefix(text, "/"+cmdCompact); found && (rest == "" || rest[0] == ' ' || rest[0] == '\n' || rest[0] == '\t') {
		return cmdCompact, strings.TrimSpace(rest), true
	}

	return "", "", false
}

// ── Session-keyed history store ─────────────────────────────────────────

// defaultMaxSessions caps the in-memory Session histories (LRU).
const defaultMaxSessions = 64

// historyStore lazily instantiates one ConversationHistory per Session ID,
// restoring it from the Session file on first access and seeding the
// Session's usage totals from the file's turn_usage records. A
// least-recently-used history beyond maxSessions is evicted from memory (its
// file stays on disk and is re-read on next access).
type historyStore struct {
	mu          sync.Mutex
	histories   map[string]*persistence.ConversationHistory
	accessOrder []string
	maxSessions int
	usage       *usage.Store
	logger      zerolog.Logger
}

// Guarantees historyStore satisfies session.HistoryProvider.
var _ session.HistoryProvider = (*historyStore)(nil)

func newHistoryStore(usageStore *usage.Store, rootLogger zerolog.Logger) *historyStore {
	return &historyStore{
		histories:   make(map[string]*persistence.ConversationHistory),
		maxSessions: defaultMaxSessions,
		usage:       usageStore,
		logger:      rootLogger.With().Str("component", "history-store").Logger(),
	}
}

// For returns the history of s, restoring it on first access. A corrupt or
// unreadable file yields the recoverable view with a WARN — never a silently
// empty history.
func (h *historyStore) For(ctx context.Context, s *session.Session) *persistence.ConversationHistory {
	h.mu.Lock()

	if existing, ok := h.histories[s.ID]; ok {
		h.touchLocked(s.ID)
		h.mu.Unlock()

		return existing
	}

	var evicted *persistence.ConversationHistory
	if len(h.histories) >= h.maxSessions && len(h.accessOrder) > 0 {
		oldest := h.accessOrder[0]
		h.accessOrder = h.accessOrder[1:]
		evicted = h.histories[oldest]
		delete(h.histories, oldest)
	}

	hist := persistence.NewConversationHistory(s.ID, persistence.NewSessionFile(s.Path)).
		WithLogger(h.logger.With().Str("component", "history").Str("session_id", s.ID).Str("address", string(s.Address)).Logger())

	h.histories[s.ID] = hist
	h.accessOrder = append(h.accessOrder, s.ID)
	h.mu.Unlock()

	// Disk I/O runs outside h.mu (criticalities-remediation M3).
	if evicted != nil {
		if err := evicted.Save(ctx); err != nil {
			h.logger.Warn().Err(err).Str("session_id", evicted.SessionID()).Msg("failed to save evicted session")
		}
	}

	if err := hist.Restore(ctx); err != nil {
		h.logger.Warn().Err(err).Str("session_id", s.ID).Msg("failed to restore session history — continuing with what was recovered")
	} else if skipped := hist.RestoredSkipped(); skipped > 0 {
		h.logger.Warn().Str("session_id", s.ID).Int("skipped_lines", skipped).Msg("session history restored with skipped corrupt lines")
	} else if hist.Len() > 0 {
		h.logger.Info().Str("session_id", s.ID).Str("address", string(s.Address)).Int("messages", hist.Len()).Msg("restored session")
	}

	if h.usage != nil {
		if rec := hist.RestoredUsage(); rec != (persistence.UsageRecord{}) {
			h.usage.Seed(s.ID, totalsFromRecord(rec))
		}
	}

	return hist
}

// Drop releases the in-memory history of an ended Session.
func (h *historyStore) Drop(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.histories, id)

	for i, k := range h.accessOrder {
		if k == id {
			h.accessOrder = append(h.accessOrder[:i], h.accessOrder[i+1:]...)

			break
		}
	}
}

// touchLocked moves key to the most-recently-used end. Caller holds h.mu.
func (h *historyStore) touchLocked(key string) {
	for i, k := range h.accessOrder {
		if k == key {
			h.accessOrder = append(h.accessOrder[:i], h.accessOrder[i+1:]...)

			break
		}
	}

	h.accessOrder = append(h.accessOrder, key)
}

// ── usage conversions ────────────────────────────────────────────────────

// totalsFromRecord converts a persisted usage record to live totals.
func totalsFromRecord(r persistence.UsageRecord) usage.Totals {
	return usage.Totals{
		ReadTokens:    r.ReadTokens,
		WrittenTokens: r.WrittenTokens,
		CachedTokens:  r.CachedTokens,
		TurnCount:     r.Turns,
		LLMCalls:      r.LLMCalls,
		AuxCalls:      r.AuxCalls,
	}
}

// recordFromTotals converts live totals (nil = none) to a usage record.
func recordFromTotals(t *usage.Totals) persistence.UsageRecord {
	if t == nil {
		return persistence.UsageRecord{}
	}

	return persistence.UsageRecord{
		ReadTokens:    t.ReadTokens,
		WrittenTokens: t.WrittenTokens,
		CachedTokens:  t.CachedTokens,
		LLMCalls:      t.LLMCalls,
		AuxCalls:      t.AuxCalls,
		Turns:         t.TurnCount,
	}
}

// usageDelta returns after − before.
func usageDelta(before, after *usage.Totals) persistence.UsageRecord {
	b, a := recordFromTotals(before), recordFromTotals(after)

	return persistence.UsageRecord{
		ReadTokens:    a.ReadTokens - b.ReadTokens,
		WrittenTokens: a.WrittenTokens - b.WrittenTokens,
		CachedTokens:  a.CachedTokens - b.CachedTokens,
		LLMCalls:      a.LLMCalls - b.LLMCalls,
		AuxCalls:      a.AuxCalls - b.AuxCalls,
		Turns:         a.Turns - b.Turns,
	}
}

// ── the per-address worker runtime ──────────────────────────────────────

// sessionRuntime bundles every dependency of the per-address worker: the
// Session lifecycle (resets, commands), the Turn itself, and the post-reply
// compaction. One item (message or Session command) is processed at a time
// per address, so every Session transition is executed by the address's
// worker only and FIFO transcript ordering holds.
type sessionRuntime struct {
	mgr          *channel.Manager
	agentReg     *agentRegistry
	fmtr         *formatter.Formatter
	reg          formatterRegistry
	bootstrapper *bootstrap.Bootstrapper
	bootstrapCfg map[string][]string
	skillsMgr    *skills.Manager
	skillCache   formatter.ContentLoader
	agentTools   []tool.BaseTool
	hist         *historyStore
	sessions     *session.Manager
	usageStore   *usage.Store
	sanitizer    *privacy.Sanitizer
	logger       zerolog.Logger

	// resets maps a channel name to its effective reset rule (zero rule =
	// never reset).
	resets map[string]session.Rule

	compactCfg config.CompactionConfig
	trigger    compaction.Trigger
	summariser *compaction.Summariser

	stdinResetOnce sync.Once

	toolCharsOnce sync.Once
	toolChars     int

	stateMu sync.Mutex
	states  map[string]*sessionState
}

// sessionState is the in-memory, Session-scoped compaction state.
type sessionState struct {
	backoff compaction.Backoff
	// projected marks that the Turn-start guard projected this Turn's view:
	// the Turn's own measurement is then not taken on the current view
	// (view epoch bumped).
	projected atomic.Bool
	// pinnedApplied marks that the latest compaction's preserved skills
	// were re-marked Active in this process. Atomic: the
	// /context handler runs on the ZMQ goroutine.
	pinnedApplied atomic.Bool
}

// state returns the Session-scoped state, creating it.
func (rt *sessionRuntime) state(sessionID string) *sessionState {
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()

	if rt.states == nil {
		rt.states = map[string]*sessionState{}
	}

	st, ok := rt.states[sessionID]
	if !ok {
		st = &sessionState{}
		rt.states[sessionID] = st
	}

	return st
}

// dropState releases an ended Session's state.
func (rt *sessionRuntime) dropState(sessionID string) {
	rt.stateMu.Lock()
	defer rt.stateMu.Unlock()

	delete(rt.states, sessionID)
}

// onSessionEnd releases the Session-scoped state of an ended Session: its
// agentskills session, usage totals (already snapshotted into the closing
// record), and compaction state.
func (rt *sessionRuntime) onSessionEnd(ctx context.Context, ended *session.Session) {
	if err := rt.bootstrapper.Close(ctx, ended.ID); err != nil {
		rt.logger.Warn().Err(err).Str("session_id", ended.ID).Msg("failed to close the agentskills session of the ended session")
	}

	rt.usageStore.Drop(ended.ID)
	rt.dropState(ended.ID)
}

// resetRule returns a channel's reset rule.
func (rt *sessionRuntime) resetRule(channelName string) session.Rule {
	return rt.resets[channelName]
}

// handleItem is the worker's per-item entry point: apply a pending reset,
// evaluate the reset rule for the channel kind, then run a Session command
// or a Turn followed by the post-reply compaction evaluation.
func (rt *sessionRuntime) handleItem(ctx context.Context, msg channel.Message) {
	addr := session.AddressOf(msg)

	rt.sessions.ApplyPending(ctx, addr)
	rt.evaluateReset(ctx, addr, msg)

	if cmd, arg, ok := parseSessionCommand(msg.Content); ok {
		rt.handleCommand(ctx, addr, msg, cmd, arg)

		return
	}

	info := rt.processMessage(ctx, addr, msg)
	rt.afterReply(ctx, addr, msg, info)
}

// evaluateReset evaluates the reset rule by channel kind: connection-
// oriented channels at their Connection start (not here); the stdin CLI
// once, at its first message after start (a single Connection from channel
// start), silently; message-oriented channels before every Turn, with the
// fixed notice when enabled.
func (rt *sessionRuntime) evaluateReset(ctx context.Context, addr session.Address, msg channel.Message) {
	if _, ok := rt.mgr.Channel(msg.ChannelName).(channel.ConnectionOriented); ok {
		return
	}

	rule := rt.resetRule(msg.ChannelName)

	if msg.ChannelName == cli.ChannelName {
		rt.stdinResetOnce.Do(func() {
			if _, pending := rt.sessions.RequestReset(ctx, addr, rule, time.Now()); pending {
				rt.sessions.ApplyPending(ctx, addr)
			}
		})

		return
	}

	if rt.sessions.EvaluateReset(ctx, addr, rule, time.Now()) && rule.Notify {
		rt.sendFixed(ctx, msg, msgSessionReset)
	}
}

// sendFixed delivers a fixed product string through the channel's Send path
// on a context that outlives the dispatcher's cancellation.
func (rt *sessionRuntime) sendFixed(ctx context.Context, msg channel.Message, text string) {
	deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryGrace)
	defer cancel()

	sendFinalReply(deliveryCtx, rt.mgr, msg, text, false, rt.logger)
}

// handleCommand runs a Session command: never a Turn, never recorded, never
// reaching the model.
func (rt *sessionRuntime) handleCommand(ctx context.Context, addr session.Address, msg channel.Message, cmd, arg string) {
	rt.logger.Info().Str("address", string(addr)).Str("command", cmd).Msg("session command")

	switch cmd {
	case cmdNew:
		next, started := rt.sessions.StartNew(ctx, addr)
		if !started {
			rt.sendFixed(ctx, msg, msgSessionAlreadyNew)

			return
		}

		// The confirmation carries no usage (the new Session has none); the
		// session_changed event follows it so the TUI's cleared footer is not
		// refilled by a late reply.
		rt.sendFixed(ctx, msg, msgSessionStarted)
		rt.sendSessionEvent(msg, protocol.ControlSessionChanged, next.ID, nil)
	case cmdCompact:
		cur, history := rt.sessions.History(ctx, addr)

		res := rt.compact(ctx, cur, history, msg.ChannelName, arg, true)
		switch res.outcome {
		case compactDone:
			rt.sendFixed(ctx, msg, msgCompacted)
			rt.sendSessionEvent(msg, protocol.ControlCompacted, cur.ID, &protocol.CompactionInfo{TokensBefore: res.before, TokensAfter: res.after})
		case compactNothing:
			rt.sendFixed(ctx, msg, msgNothingToCompact)
		case compactFailed:
			rt.sendFixed(ctx, msg, msgCompactionFailed)
		}
	}
}

// sendSessionEvent pushes a Session lifecycle control event on the ZMQ
// channel (a no-op elsewhere).
func (rt *sessionRuntime) sendSessionEvent(msg channel.Message, kind, sessionID string, info *protocol.CompactionInfo) {
	zmqCh := getZMQChannel(rt.mgr, msg.ChannelName)
	if zmqCh == nil {
		return
	}

	if err := zmqCh.SendSessionEvent(kind, sessionID, info); err != nil {
		rt.logger.Warn().Err(err).Str("event", kind).Msg("failed to send session event")
	}
}

// ── skills scoped to the Session ─────────────────────────────────────────

// agentSkillsSession returns the Session's agentskills session, creating it
// with the bootstrap skills, then marks Active the skills that must never be
// offered by the catalog: the inline formatter skill, the Compaction skill,
// and (once per process) the skills pinned by the latest compaction.
func (rt *sessionRuntime) agentSkillsSession(ctx context.Context, cur *session.Session, history *persistence.ConversationHistory, channelName string) agentskillsSpec.SessionID {
	sid, err := rt.bootstrapper.GetOrCreateSession(ctx, cur.ID, channelName)
	if err != nil {
		rt.logger.Warn().Err(err).Str("session_id", cur.ID).Msg("failed to create bootstrap session")
	}

	if sid == "" {
		return sid
	}

	// Formatter inline-reinforcement mode: the formatter skill is marked
	// Active so GetMetadataPrompt excludes it; its body is delivered via the
	// position-1 <formatter_skill> wrap.
	if fmtCfg := rt.reg[channelName]; fmtCfg.skillName != "" && !fmtCfg.postStep {
		if _, loadErr := rt.skillsMgr.LoadSkill(ctx, sid, fmtCfg.skillName); loadErr != nil {
			rt.logger.Warn().Err(loadErr).Str("session_id", cur.ID).Str("formatter_skill", fmtCfg.skillName).
				Msg("failed to mark formatter skill Active for progressive-disclosure suppression")
		}
	}

	// The Compaction skill is for the summariser only.
	if name := rt.compactCfg.Skill; name != "" {
		if _, pathErr := rt.skillsMgr.SkillFilePath(name); pathErr == nil {
			if _, loadErr := rt.skillsMgr.LoadSkill(ctx, sid, name); loadErr != nil {
				rt.logger.Debug().Err(loadErr).Str("skill", name).Msg("failed to hide the compaction skill from the catalog")
			}
		}
	}

	// Restore: the skills pinned by the latest compaction are Active again
	// — once per process, never against a later
	// skills-unload.
	if st := rt.state(cur.ID); st.pinnedApplied.CompareAndSwap(false, true) {
		if _, preserved, ok := history.Summary(); ok {
			for _, name := range preserved {
				if _, loadErr := rt.skillsMgr.LoadSkill(ctx, sid, name); loadErr != nil {
					rt.logger.Warn().Err(loadErr).Str("skill", name).Msg("failed to re-mark a pinned skill Active")
				}
			}
		}
	}

	return sid
}

// excludedFromPinning returns the skills never pinned by a compaction: the
// channel's bootstrap skills (already in the static shell), its formatter
// skill, and the Compaction skill.
func (rt *sessionRuntime) excludedFromPinning(channelName string) map[string]bool {
	out := map[string]bool{rt.compactCfg.Skill: true}

	for _, s := range rt.bootstrapCfg[channelName] {
		out[s] = true
	}

	if fmtCfg := rt.reg[channelName]; fmtCfg.skillName != "" {
		out[fmtCfg.skillName] = true
	}

	return out
}

// skillBody reads a skill's SKILL.md through the skill cache (hot edits
// apply at the next assembly).
func (rt *sessionRuntime) skillBody(ctx context.Context, name string) (string, bool) {
	path, err := rt.skillsMgr.SkillFilePath(name)
	if err != nil {
		return "", false
	}

	body, err := rt.skillCache.Load(ctx, path)
	if err != nil {
		return "", false
	}

	return body, true
}

// compactionSkillPrompt loads the Compaction skill's SKILL.md through the
// skill cache — the summariser's system prompt (hot edits apply).
func (rt *sessionRuntime) compactionSkillPrompt(ctx context.Context) (string, error) {
	path, err := rt.skillsMgr.SkillFilePath(rt.compactCfg.Skill)
	if err != nil {
		return "", fmt.Errorf("compaction skill %q: %w", rt.compactCfg.Skill, err)
	}

	body, err := rt.skillCache.Load(ctx, path)
	if err != nil {
		return "", fmt.Errorf("compaction skill %q: %w", rt.compactCfg.Skill, err)
	}

	return body, nil
}

// skillBlocks renders pinned skills (unknown ones skipped).
func (rt *sessionRuntime) skillBlocks(ctx context.Context, names []string) []compaction.SkillBlock {
	out := make([]compaction.SkillBlock, 0, len(names))

	for _, n := range names {
		if body, ok := rt.skillBody(ctx, n); ok {
			out = append(out, compaction.SkillBlock{Name: n, Body: body})
		}
	}

	return out
}

// leadMessage renders the user message placed right after the stable
// prefix: the latest compaction summary plus the pinned skills (and extra
// skills whose load results a failure projection omitted). "" when there is
// neither.
func (rt *sessionRuntime) leadMessage(ctx context.Context, history *persistence.ConversationHistory, extra []string) string {
	summary, preserved, _ := history.Summary()

	names := append([]string(nil), preserved...)
	for _, e := range extra {
		if !contains(names, e) {
			names = append(names, e)
		}
	}

	return compaction.SummaryMessage(summary, rt.skillBlocks(ctx, names))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// withLead appends the lead user message after the stable prefix.
func withLead(prefix []*schema.Message, lead string) []*schema.Message {
	if lead == "" {
		return prefix
	}

	out := make([]*schema.Message, 0, len(prefix)+1)
	out = append(out, prefix...)

	return append(out, schema.UserMessage(lead))
}

// ── prompt estimation (shared by /context and the compaction trigger) ────

// promptParts are the stable-prefix sections of an assembled prompt.
type promptParts struct {
	formatterBody string // inline formatter SKILL.md (position 1), "" otherwise
	staticShell   string
	dynamic       string
	skillsMeta    string // redacted
	toolChars     int
}

// collectPromptParts gathers the stable-prefix sections of a Session's
// prompt exactly as processMessage assembles them.
func (rt *sessionRuntime) collectPromptParts(ctx context.Context, sessionID string, agentSkillsSID agentskillsSpec.SessionID, channelName string) promptParts {
	var p promptParts

	if fmtCfg := rt.reg[channelName]; fmtCfg.skillName != "" && !fmtCfg.postStep && rt.fmtr != nil {
		if body, err := rt.fmtr.LoadSkillPrompt(ctx, fmtCfg.skillName); err == nil {
			p.formatterBody = body
		}
	}

	if shell, err := rt.bootstrapper.StaticShell(ctx, sessionID, channelName); err == nil {
		p.staticShell = shell
	} else {
		rt.logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to build static shell")
	}

	p.dynamic = rt.bootstrapper.DynamicContent(ctx, sessionID)

	if agentSkillsSID != "" {
		if meta, err := rt.skillsMgr.GetMetadataPrompt(ctx, agentSkillsSID); err == nil {
			// Level 1 (source containment): the skills runtime renders each
			// skill's absolute Location; redact the home prefix.
			p.skillsMeta = rt.sanitizer.Redact(meta)
		}
	}

	p.toolChars = rt.toolCatalogChars(ctx)

	return p
}

// toolCatalogChars sums each tool's marshalled ToolInfo JSON (the closest
// analogue of what the wire format carries); computed once.
func (rt *sessionRuntime) toolCatalogChars(ctx context.Context) int {
	rt.toolCharsOnce.Do(func() {
		for _, t := range rt.agentTools {
			info, err := t.Info(ctx)
			if err != nil || info == nil {
				continue
			}

			if b, err := json.Marshal(info); err == nil {
				rt.toolChars += len(b)
			}
		}
	})

	return rt.toolChars
}

// prefixMessages builds the stable prefix: 1. formatter inline wrap
// (position 1), 2. static shell, 3. dynamic assets, 4. skills metadata —
// byte-identical across Turns and independent of the prompt layout.
func (rt *sessionRuntime) prefixMessages(ctx context.Context, p promptParts, channelName string) []*schema.Message {
	var prefix []*schema.Message

	for _, section := range []string{p.staticShell, p.dynamic, p.skillsMeta} {
		if section != "" {
			prefix = append(prefix, schema.SystemMessage(section))
		}
	}

	return applyFormatterInlineReinforcement(ctx, prefix, rt.reg[channelName], rt.fmtr, channelName, rt.logger)
}

// promptSections is the estimated per-section breakdown (characters) of a
// prompt: the stable prefix, the tool catalog, the compaction summary lead
// message, and the history view.
func promptSections(p promptParts, lead string, history []persistence.HistoryMessage) []contextSection {
	sections := make([]contextSection, 0, 7)

	add := func(name string, chars int) {
		if chars > 0 {
			sections = append(sections, contextSection{name, chars})
		}
	}

	add("formatter skill (inline)", len(p.formatterBody))
	add("static shell", len(p.staticShell))
	add("dynamic assets", len(p.dynamic))
	add("skills metadata", len(p.skillsMeta))
	add("tool catalog", p.toolChars)
	add("compaction summary", len(lead))

	chars := 0
	for _, m := range history {
		chars += m.EstimatedChars()
	}

	if len(history) > 0 {
		sections = append(sections, contextSection{"history (" + formatTokens(len(history)) + " messages)", chars})
	}

	return sections
}

// estimatePromptTokens estimates the whole prompt in tokens (chars ÷ 4) —
// the one estimator shared by /context and the compaction trigger, so what
// the user sees and what triggers agree.
func estimatePromptTokens(p promptParts, lead string, history []persistence.HistoryMessage) int {
	total := 0
	for _, s := range promptSections(p, lead, history) {
		total += compaction.Tokens(s.chars)
	}

	return total
}

// ── Turn-start guard and post-reply compaction ───────────────────────────

// turnStartGuard compares the whole-prompt estimate of the current view with
// the compaction threshold before a Turn: when exceeded, the
// Turn runs on a projection omitting the oldest whole Turns (never from the
// file, never splitting a tool call from its result), with the Active
// skills whose load results it omits pinned in the lead block. It never
// runs a compaction. Returns the Turns to omit and the extra pinned skills.
func (rt *sessionRuntime) turnStartGuard(ctx context.Context, cur *session.Session, history *persistence.ConversationHistory, parts promptParts, agentSkillsSID agentskillsSpec.SessionID, channelName string) (int, []string) {
	st := rt.state(cur.ID)
	st.projected.Store(false)

	threshold := rt.trigger.Threshold()
	if threshold == 0 {
		return 0, nil
	}

	view := history.Messages()

	before := estimatePromptTokens(parts, rt.leadMessage(ctx, history, nil), view)
	if before <= threshold {
		return 0, nil
	}

	turns := 0
	for _, m := range view {
		if m.IsTurnBoundary() {
			turns++
		}
	}

	omit, estimate := 0, before

	var extra []string

	for n := 1; n < turns; n++ {
		kept := persistence.OmitOldestTurns(view, n)
		extra = rt.omittedPinnedSkills(ctx, agentSkillsSID, channelName, view[:len(view)-len(kept)])
		omit = n

		estimate = estimatePromptTokens(parts, rt.leadMessage(ctx, history, extra), kept)
		if estimate <= threshold {
			break
		}
	}

	if omit == 0 {
		return 0, nil
	}

	st.projected.Store(true)

	rt.logger.Warn().
		Str("session_id", cur.ID).
		Str("address", string(cur.Address)).
		Int("estimated_tokens", before).
		Int("projected_tokens", estimate).
		Int("threshold", threshold).
		Int("omitted_turns", omit).
		Msg("prompt over the compaction threshold — this Turn runs on a projection omitting the oldest Turns (the transcript is unchanged)")

	return omit, extra
}

// omittedPinnedSkills returns the Active, pinnable skills whose skills-load
// result lies in the omitted entries.
func (rt *sessionRuntime) omittedPinnedSkills(ctx context.Context, agentSkillsSID agentskillsSpec.SessionID, channelName string, omitted []persistence.HistoryMessage) []string {
	loaded := compaction.LoadOrder(omitted)
	if len(loaded) == 0 || agentSkillsSID == "" {
		return nil
	}

	active, err := rt.skillsMgr.ActiveSkills(ctx, agentSkillsSID)
	if err != nil {
		return nil
	}

	return compaction.PreservedSkills(active, rt.excludedFromPinning(channelName), nil, loaded)
}

// turnInfo is what the post-reply compaction evaluation needs from a Turn.
type turnInfo struct {
	sess    *session.Session
	history *persistence.ConversationHistory
	channel string
	sid     agentskillsSpec.SessionID
	// measuredValid reports that the Turn's last main-agent call measured
	// the current view (a main call happened and no projection was used).
	measuredValid bool
	// appendedTokens estimates the entries appended after that call.
	appendedTokens int
}

// afterReply evaluates the automatic compaction trigger in the address's
// worker, after the Turn's reply was delivered and before the next item
// : a message arriving meanwhile waits in the FIFO inbox.
func (rt *sessionRuntime) afterReply(ctx context.Context, addr session.Address, msg channel.Message, info turnInfo) {
	if info.history == nil || rt.summariser == nil || !rt.compactCfg.AutoEnabled() {
		return
	}

	st := rt.state(info.sess.ID)
	if st.backoff.Skip() {
		rt.logger.Debug().Str("session_id", info.sess.ID).Msg("automatic compaction skipped (failure backoff)")

		return
	}

	measured := 0
	if totals := rt.usageStore.Snapshot(info.sess.ID); totals != nil {
		measured = totals.LastMainPromptTokens
	}

	parts := rt.collectPromptParts(ctx, info.sess.ID, info.sid, info.channel)
	estimate := estimatePromptTokens(parts, rt.leadMessage(ctx, info.history, nil), info.history.Messages())
	compared := compaction.Compared(measured, info.measuredValid, info.appendedTokens, estimate)

	if !rt.trigger.Due(compared, info.history.TurnsSinceCompaction()) {
		return
	}

	rt.logger.Info().
		Str("session_id", info.sess.ID).
		Str("address", string(addr)).
		Int("compared_tokens", compared).
		Int("threshold", rt.trigger.Threshold()).
		Int("turns_since_compaction", info.history.TurnsSinceCompaction()).
		Msg("automatic compaction due")

	res := rt.compact(ctx, info.sess, info.history, info.channel, "", false)
	if res.outcome == compactDone {
		rt.sendSessionEvent(msg, protocol.ControlCompacted, info.sess.ID, &protocol.CompactionInfo{TokensBefore: res.before, TokensAfter: res.after})
	}
}

// compactOutcome is the result of one compaction.
type compactOutcome int

const (
	compactNothing compactOutcome = iota
	compactDone
	compactFailed
)

type compactResult struct {
	outcome       compactOutcome
	before, after int
}

// compact runs one Compaction of the Session: plan the cut, summarise on the
// auxiliary chain (usage counted as aux), pin the Active skills, and append
// the compaction entry (the view is rebuilt by it). A failure appends
// nothing, logs a WARN naming the consumer, and — for an automatic run —
// extends the backoff; any success resets it.
func (rt *sessionRuntime) compact(ctx context.Context, cur *session.Session, history *persistence.ConversationHistory, channelName, instructions string, manual bool) compactResult {
	st := rt.state(cur.ID)

	if rt.summariser == nil {
		return compactResult{outcome: compactNothing}
	}

	if err := history.Save(ctx); err != nil {
		rt.logger.Warn().Err(err).Str("consumer", "compaction").Msg("compaction failed: the transcript could not be flushed")

		return compactResult{outcome: compactFailed}
	}

	view, lines := history.View()
	prevSummary, prevPreserved, _ := history.Summary()
	sysPrompt := rt.summariser.SystemPrompt(ctx)

	in := compaction.PlanInput{
		Entries:          view,
		Lines:            lines,
		TurnMode:         rt.trigger.TurnMode(),
		KeepRecentTokens: rt.compactCfg.KeepRecentTokens,
		KeepRecentTurns:  rt.compactCfg.KeepRecentTurns,
		OverheadTokens:   compaction.Tokens(len(sysPrompt) + len(prevSummary) + len(instructions)),
	}
	if !rt.trigger.TurnMode() {
		in.MaxInputTokens = rt.trigger.Threshold()
	}

	plan, ok := compaction.PlanCut(in)
	if !ok {
		return compactResult{outcome: compactNothing}
	}

	sid := rt.agentSkillsSession(ctx, cur, history, channelName)
	parts := rt.collectPromptParts(ctx, cur.ID, sid, channelName)
	tokensBefore := estimatePromptTokens(parts, rt.leadMessage(ctx, history, nil), history.Messages())

	acc := rt.usageStore.Accumulator(cur.ID)
	usageBefore := acc.Snapshot()

	summary, err := rt.summariser.Summarise(usage.WithAccumulator(ctx, acc), compaction.Request{
		PreviousSummary: prevSummary,
		Conversation:    compaction.Serialise(plan.ToSummarise),
		Instructions:    instructions,
	})
	if err != nil {
		if !manual {
			st.backoff.Failed()
		}

		rt.logger.Warn().Err(err).Str("consumer", "compaction").Str("session_id", cur.ID).Bool("manual", manual).
			Msg("compaction failed; the session is unchanged")

		return compactResult{outcome: compactFailed}
	}

	preserved := rt.pinnedSkills(ctx, sid, channelName, prevPreserved, view)
	delta := usageDelta(usageBefore, acc.Snapshot())
	tokensAfter := estimatePromptTokens(parts, compaction.SummaryMessage(summary, rt.skillBlocks(ctx, preserved)), view[plan.Cut:])

	rec := persistence.HistoryMessage{
		Kind:            persistence.KindCompaction,
		Summary:         summary,
		FirstKept:       plan.FirstKept,
		PreservedSkills: preserved,
		TokensBefore:    tokensBefore,
		TokensAfter:     tokensAfter,
		Usage:           &delta,
		Timestamp:       time.Now(),
	}

	if err := history.AppendRecord(ctx, rec); err != nil {
		if !manual {
			st.backoff.Failed()
		}

		rt.logger.Warn().Err(err).Str("consumer", "compaction").Str("session_id", cur.ID).Msg("compaction failed: the entry could not be appended; the session is unchanged")

		return compactResult{outcome: compactFailed}
	}

	st.backoff.Succeeded()

	rt.logger.Info().
		Str("session_id", cur.ID).
		Str("address", string(cur.Address)).
		Int("summarised_entries", plan.Cut).
		Int("first_kept", plan.FirstKept).
		Strs("preserved_skills", preserved).
		Int("tokens_before", tokensBefore).
		Int("tokens_after", tokensAfter).
		Bool("manual", manual).
		Msg("session compacted")

	return compactResult{outcome: compactDone, before: tokensBefore, after: tokensAfter}
}

// pinnedSkills returns the skills to preserve verbatim: Active minus bootstrap, formatter and Compaction skill; when their
// estimated size exceeds half of keep_recent_tokens, the oldest-loaded are
// unloaded (available again in the catalog) with a WARN until the rest fits.
func (rt *sessionRuntime) pinnedSkills(ctx context.Context, sid agentskillsSpec.SessionID, channelName string, previous []string, view []persistence.HistoryMessage) []string {
	if sid == "" {
		return nil
	}

	active, err := rt.skillsMgr.ActiveSkills(ctx, sid)
	if err != nil {
		rt.logger.Warn().Err(err).Msg("failed to list active skills for pinning")

		return nil
	}

	preserved := compaction.PreservedSkills(active, rt.excludedFromPinning(channelName), previous, compaction.LoadOrder(view))

	budget := rt.compactCfg.KeepRecentTokens / 2
	size := func(names []string) int {
		chars := 0
		for _, b := range rt.skillBlocks(ctx, names) {
			chars += len(b.Body)
		}

		return compaction.Tokens(chars)
	}

	for len(preserved) > 0 && size(preserved) > budget {
		oldest := preserved[0]
		preserved = preserved[1:]

		if err := rt.skillsMgr.UnloadSkill(ctx, sid, oldest); err != nil {
			rt.logger.Warn().Err(err).Str("skill", oldest).Msg("failed to unload an oversized pinned skill")
		}

		rt.logger.Warn().Str("skill", oldest).Int("budget_tokens", budget).
			Msg("pinned skills exceed half of keep_recent_tokens — oldest-loaded skill unloaded (available again in the catalog)")
	}

	return preserved
}

// processTurnAppended estimates the entries recorded after the Turn's last
// main-agent call: everything after the last tool result of the Turn, or
// after its turn-context line when no tool ran (the call's own output).
func processTurnAppended(msgs []persistence.HistoryMessage) int {
	start := len(msgs)

	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == persistence.RoleTool || msgs[i].Kind == persistence.KindTurnContext {
			start = i + 1

			break
		}

		if msgs[i].IsTurnBoundary() {
			start = i

			break
		}
	}

	chars := 0
	for _, m := range msgs[start:] {
		chars += m.EstimatedChars()
	}

	return compaction.Tokens(chars)
}

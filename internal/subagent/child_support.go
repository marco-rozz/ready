// child_support.go — the child transcript bookkeeping the driver reuses
// (openspec change subagent-runtime, tasks 4.1/4.3/4.4): the turn_usage
// delta per dispatch (anchored to a per-child baseline so each child file's
// usage records sum the dispatches exactly once), the child Compaction
// evaluation, and the HistoryMessage → schema.Message projection.
package subagent

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// recordChildUsage appends the dispatch's usage delta to the child file as
// one turn_usage record. The baseline is the child's cumulative totals at
// session creation (or the previous dispatch's end), so the child file's
// records sum every dispatch exactly once (D9 restore walk).
func (r *Runtime) recordChildUsage(st *Agent, acc *usage.Accumulator) {
	child := r.childHistory(st.ID)
	if child == nil {
		return
	}

	r.mu.Lock()
	if r.childBaseline == nil {
		// Defensive: New() initialises the map, but a nil map here would
		// panic on the baseline write below while r.mu is held — a panic
		// inside a critical section wedges the launch defer (reproduced
		// 2026-11-16). Lazily initialise instead.
		r.childBaseline = map[string]usage.Totals{}
	}
	base := r.childBaseline[st.ID]
	r.mu.Unlock()

	now := acc.Snapshot()
	if now == nil {
		return
	}

	delta := persistence.UsageRecord{
		ReadTokens:    now.ReadTokens - base.ReadTokens,
		WrittenTokens: now.WrittenTokens - base.WrittenTokens,
		CachedTokens:  now.CachedTokens - base.CachedTokens,
		LLMCalls:      now.LLMCalls - base.LLMCalls,
		AuxCalls:      now.AuxCalls - base.AuxCalls,
	}

	if delta == (persistence.UsageRecord{}) {
		return
	}

	r.mu.Lock()
	r.childBaseline[st.ID] = *now
	r.mu.Unlock()

	if err := child.AppendRecord(context.Background(), persistence.HistoryMessage{
		Kind:      persistence.KindTurnUsage,
		Usage:     &delta,
		Timestamp: time.Now(),
	}); err != nil {
		r.log.Warn().Err(err).Str("child_session", st.ID).Msg("failed to append the child turn_usage record")
	}
}

// compactChild runs the child's Compaction evaluation after a dispatch
// reply (task 4.3): the trigger compares against the CHILD's own anchor
// (its detached accumulator's last main prompt tokens, in memory, never
// restored); the summariser call is attributed to the owner through the
// forwarding accumulator carried in ctx. A failure appends nothing and
// never delays the next dispatch.
func (r *Runtime) compactChild(ctx context.Context, st *Agent, child *persistence.ConversationHistory, childAcc *usage.Accumulator) {
	cc := r.deps.ChildCompaction
	if cc == nil || !cc.Enabled || cc.Summarise == nil {
		return
	}

	if err := child.Save(ctx); err != nil {
		r.log.Warn().Err(err).Str("consumer", "child-compaction").Msg("child compaction failed: the transcript could not be flushed")

		return
	}

	view, lines := child.View()
	prevSummary, prevPreserved, _ := child.Summary()

	// The child's prompt overhead: the static prefix was built at dispatch
	// time; the trigger uses the threshold and the appended-entries shape
	// like the owner path. The child anchor drives only this trigger.
	compared := compaction.Compared(
		childAcc.Snapshot().LastMainPromptTokens,
		childAcc.Snapshot().LastMainPromptTokens > 0,
		0,
		estimateViewTokens(view),
	)

	if !r.childTrigger().Due(compared, child.TurnsSinceCompaction()) {
		return
	}

	in := compaction.PlanInput{
		Entries:          view,
		Lines:            lines,
		TurnMode:         cc.Window <= 0,
		KeepRecentTokens: cc.KeepRecentTokens,
		KeepRecentTurns:  cc.KeepRecentTurns,
	}
	if cc.Window > 0 {
		in.MaxInputTokens = cc.Window - cc.Reserve
	}

	plan, ok := compaction.PlanCut(in)
	if !ok {
		return
	}

	// Neutralisation-only scan of the summariser OUTPUT before the entry
	// (task 7.4 happens at the summariser seam; here the child path runs
	// the same single production scan through cc.Summarise's wrapper).
	summary, err := cc.Summarise(usage.WithAccumulator(ctx, childAcc), compaction.Request{
		PreviousSummary: prevSummary,
		Conversation:    compaction.Serialise(plan.ToSummarise),
	})
	if err != nil {
		r.log.Warn().Err(err).Str("consumer", "child-compaction").Str("subagent", st.Name).
			Msg("child compaction failed; the child transcript is unchanged")

		return
	}

	rec := persistence.HistoryMessage{
		Kind:            persistence.KindCompaction,
		Summary:         summary,
		FirstKept:       plan.FirstKept,
		PreservedSkills: prevPreserved,
		TokensBefore:    estimateViewTokens(view),
		TokensAfter:     estimateViewTokens(view[plan.Cut:]),
		Timestamp:       time.Now(),
	}

	if err := child.AppendRecord(ctx, rec); err != nil {
		r.log.Warn().Err(err).Str("consumer", "child-compaction").Msg("child compaction failed: the entry could not be appended")

		return
	}

	r.log.Info().Str("subagent", st.Name).Int("first_kept", plan.FirstKept).Msg("child session compacted")
}

// childTrigger builds the child compaction trigger from the child params
// (the owner's parameters, D2).
func (r *Runtime) childTrigger() compaction.Trigger {
	cc := r.deps.ChildCompaction
	if cc == nil {
		return compaction.Trigger{}
	}

	return compaction.Trigger{
		Window:   cc.Window,
		Reserve:  cc.Reserve,
		MaxTurns: cc.MaxTurns,
	}
}

// estimateViewTokens estimates a view's size (chars ÷ 4, the shared
// estimator's shape).
func estimateViewTokens(view []persistence.HistoryMessage) int {
	chars := 0
	for _, m := range view {
		chars += m.EstimatedChars()
	}

	return compaction.Tokens(chars)
}

// toMessage projects a history entry into a schema message for the model
// view (the same projection the owner path's toSchemaMessages renders).
func toMessage(m persistence.HistoryMessage) *schema.Message {
	switch m.Role {
	case persistence.RoleUser:
		return schema.UserMessage(m.Content)
	case persistence.RoleAssistant:
		msg := &schema.Message{Role: schema.Assistant, Content: m.Content}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID: tc.ID,
				Function: schema.FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}

		return msg
	case persistence.RoleTool:
		return schema.ToolMessage(m.Content, m.ToolCallID)
	case persistence.RoleSystem:
		return schema.SystemMessage(m.Content)
	}

	return schema.UserMessage(m.Content)
}

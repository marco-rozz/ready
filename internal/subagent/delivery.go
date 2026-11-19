// delivery.go — the pending-handback delivery at the head of an owner Turn
// (openspec change subagent-runtime, task 5.3, design D6): one
// handback_delivery record per handback BEFORE the user message, flushed to
// the file before the first model call; the pending entries are removed
// only after that flush succeeded. The deliveries of one Turn fit
// min(handback_turn_budget_tokens, 25% of the smallest context window); the
// oldest pending handback is always delivered — truncated with the fixed
// note when it alone exceeds the budget — so nothing stays pending forever.
package subagent

import (
	"context"
	"fmt"
	"time"

	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/injection"
	"github.com/marco-rozz/ready/internal/persistence"
)

// deliverBudget is the History surface the delivery needs beyond the
// bookkeeping append: view entries + Save.
type deliverBudget interface {
	Add(msg persistence.HistoryMessage)
	Save(ctx context.Context) error
}

// DeliverPending delivers the owner's pending handbacks into history at the
// head of the Turn under budgetTokens (compaction-estimated). It returns
// (delivered, remaining). The flush failure path removes nothing: a Turn
// that crashes before the flush did not consume (re-injected next Turn).
func (r *Runtime) DeliverPending(ctx context.Context, owner Owner, history deliverBudget, budgetTokens int) (delivered, remaining int) {
	if history == nil {
		return 0, 0
	}

	pending := r.reg.PendingList(owner.SessionID)
	if len(pending) == 0 {
		return 0, 0
	}

	if budgetTokens <= 0 {
		budgetTokens = r.deps.HandbackTurnBudgetTokens
	}

	budget := budgetTokens

	for i, p := range pending {
		text := RenderHandbackDelivery(*p)
		cost := compaction.Tokens(len(text))

		switch {
		case i == 0 && cost > budget:
			// The oldest pending handback is always delivered, truncated
			// to the budget with the fixed note — nothing stays pending
			// forever (D6).
			text = TruncateTokens(text, budget)
		case cost > budget:
			break // the rest stays pending in FIFO order
		}

		if cost > budget && i > 0 {
			break
		}

		history.Add(persistence.HistoryMessage{
			Role:                persistence.RoleUser,
			Kind:                persistence.KindHandbackDelivery,
			Content:             text,
			DeliveredHandbackID: p.HandbackID,
			Timestamp:           time.Now(),
		})

		budget = max(0, budget-cost)
		delivered++
	}

	remaining = len(pending) - delivered

	if delivered == 0 {
		return 0, remaining
	}

	if err := history.Save(ctx); err != nil {
		// The flush failed: nothing was consumed — the handbacks stay
		// pending and are re-delivered at the next Turn (the additions stay
		// in the view; the next Save re-appends them — the watermark
		// advanced only over fully written lines).
		r.log.Error().Err(err).Str("owner_session", owner.SessionID).
			Msg("failed to flush the handback deliveries; they stay pending")

		return 0, len(pending)
	}

	// The flush succeeded: the entries are consumed (D6).
	for _, hb := range pending[:delivered] {
		r.ConsumePending(owner, hb.HandbackID)
	}

	// The pending count changed: the TUI's ◆ badge must drop without
	// waiting for the next fleet transition.
	if delivered > 0 {
		r.notifyFleet(owner.SessionID)
	}

	if remaining > 0 {
		// A fixed companion line naming the rest (never an LLM turn).
		history.Add(persistence.HistoryMessage{
			Role:      persistence.RoleUser,
			Kind:      persistence.KindHandbackDelivery,
			Content:   fmt.Sprintf("%d more results pending — use subagent_wait to fetch them.", remaining),
			Timestamp: time.Now(),
		})

		if err := history.Save(ctx); err != nil {
			r.log.Warn().Err(err).Str("owner_session", owner.SessionID).Msg("failed to flush the pending-results note")
		}
	}

	r.log.Info().
		Str("owner_session", owner.SessionID).
		Int("delivered", delivered).
		Int("remaining", remaining).
		Msg("pending handbacks delivered at the head of the turn")

	return delivered, remaining
}

// RenderHandbackDelivery builds one delivery's text: the status line, then
// the random-ID handback envelope around the SCAN-MUTATED report (D10: the
// delivered record carries the scanned text; the raw report stays only in
// the handback record).
func RenderHandbackDelivery(hb Handback) string {
	scanned := injection.Scan(hb.Report, injection.ModeFull)

	body := fmt.Sprintf("Subagent %s finished (%s).\nhandback_id: %s dispatch_id: %s\n\n%s",
		hb.Name, hb.Status, hb.HandbackID, hb.DispatchID, scanned.Text)

	return injection.BuildHandbackEnvelope(injection.NewID(), body)
}

// TruncateTokens cuts text to maxTokens (estimated chars ÷ 4) with the
// fixed delivery-budget note.
func TruncateTokens(text string, maxTokens int) string {
	limit := maxTokens * 4
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit]) + "… [truncated: handback exceeded the per-turn delivery budget; the rest is withheld]"
}

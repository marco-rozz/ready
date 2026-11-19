// scoping.go — depth-2 dispatch scoping (openspec change subagent-runtime,
// task 4.6, design D8): a depth-1 child's grandchildren are scoped to the
// parent's dispatch — when that dispatch ends, running grandchildren are
// interrupted, their unconsumed handbacks are folded (capped) into the
// parent's handback, and their folders are closed with reason
// `parent_dispatch_ended`. They are never reopened.
package subagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/injection"
	"github.com/marco-rozz/ready/internal/persistence"
)

// maxFoldedHandbacks caps the handbacks folded into a parent's handback.
const maxFoldedHandbacks = 4

// scopeGrandchildren runs at the end of a depth-1 child's dispatch: it
// interrupts the child's running children, joins them, folds their
// unconsumed handbacks (capped, oldest first) and closes their folders.
// Returns the folded text block for the parent's handback ("" when none).
func (r *Runtime) scopeGrandchildren(ctx context.Context, owner Owner, parent *Agent) string {
	if parent.Depth+1 > r.deps.Limits.MaxDepth {
		return "" // the parent cannot have children (depth cap)
	}

	// The grandchildren registered under the same owner Session with this
	// parent's name.
	var running []*dispatchHandle
	var views []AgentView

	for _, v := range r.reg.Agents(owner.SessionID) {
		if v.Parent != parent.Name {
			continue
		}

		views = append(views, v)

		r.mu.Lock()
		h := r.disp[v.SubagentID]
		r.mu.Unlock()

		if h != nil {
			running = append(running, h)
		}
	}

	if len(views) == 0 {
		return ""
	}

	// Interrupt + join.
	for _, h := range running {
		h.cancel()
	}

	for _, h := range running {
		<-h.done
	}

	// Fold the unconsumed handbacks (capped).
	folded := ""

	var foldedTexts []string

	count := 0
	for _, hb := range r.reg.PendingList(owner.SessionID) {
		parented := false
		for _, v := range views {
			if v.SubagentID == hb.SubagentID {
				parented = true

				break
			}
		}

		if !parented {
			continue
		}

		if r.reg.RemovePending(owner.SessionID, hb.HandbackID) {
			if count < maxFoldedHandbacks {
				scanned := r.scanReport(hb.Report)
				foldedTexts = append(foldedTexts, fmt.Sprintf("[%s · %s · %s] %s", hb.Name, hb.Status, hb.DispatchID, scanned))
			}

			count++
		}
	}

	if count > 0 {
		note := fmt.Sprintf("%d grandchild result(s) folded:", count)
		if count > maxFoldedHandbacks {
			note = fmt.Sprintf("%d grandchild result(s) folded (showing %d):", count, maxFoldedHandbacks)
		}

		folded = note + "\n" + joinBlocks(foldedTexts)
	}

	// Close the grandchildren's folders.
	for _, v := range views {
		if h := r.childHistory(v.SubagentID); h != nil && !h.Closed() {
			if n := h.CloseOpenToolCallGroupWith(persistence.InterruptedOwnerEndToolResult); n > 0 {
				r.log.Warn().Str("grandchild", v.Name).Int("interrupted_results", n).
					Msg("a scoped grandchild left tool calls without results — interrupted results recorded")
			}

			now := time.Now()
			if err := h.AppendRecord(ctx, persistence.HistoryMessage{
				Kind:      persistence.KindSessionClosed,
				EndedAt:   &now,
				Reason:    "parent_dispatch_ended",
				Timestamp: now,
			}); err != nil {
				r.log.Warn().Err(err).Str("grandchild", v.Name).Msg("failed to close a scoped grandchild folder")
			}

			r.dropChild(v.SubagentID)
		}

		// The registry record goes too (never reopened, D8).
		r.reg.DropChild(owner.SessionID, v.SubagentID)
	}

	return folded
}

// scanReport applies the delivery-time scan to a folded report.
func (r *Runtime) scanReport(report string) string {
	return injection.Scan(report, injection.ModeFull).Text
}

// joinBlocks joins folded blocks with blank lines.
func joinBlocks(blocks []string) string {
	return strings.Join(blocks, "\n\n")
}

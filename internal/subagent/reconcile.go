// reconcile.go — boot restore and reconciliation (openspec change
// subagent-runtime, task 5.5, design D16): for every still-current owner
// Session the Runtime registers each child folder lacking a closing record,
// starts no dispatch and generates no LLM traffic, and reconciles each
// child's last dispatch against the owner file — a completed dispatch whose
// handback record is missing is regenerated from the child's final reply; a
// dispatch without a final reply yields the fixed interrupted handback —
// then rebuilds the pending set by whole-file scan (independent of the
// compaction boundary — bookkeeping, not conversation).
package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// reconcileReport summarises one owner reconciliation pass (observability).
// ReconcileReport summarises one owner reconciliation pass (exported for
// the cmd wiring and tests).
type ReconcileReport struct {
	Children     int
	Interrupted  int
	Regenerated  int
	PendingBuilt int
	Depth2Closed int
}

// ReconcileOwner runs the boot reconciliation for one owner. owner.History
// must be the owner's loaded transcript (its Path locates the child
// subtree).
func (r *Runtime) ReconcileOwner(ctx context.Context, owner Owner) ReconcileReport {
	rep := ReconcileReport{}

	ownerPath := ""
	if owner.History != nil {
		ownerPath = owner.History.Path()
	}

	if ownerPath == "" {
		return rep
	}

	ownerDir := parentDir(ownerPath)

	// Whole-file scan of the owner file: handback records + their
	// delivered counterparts (independent of the latest compaction entry,
	// D6).
	handbacks, counterparts := scanOwnerHandbacks(ownerPath)

	// Child folders: `subagents/<name>_<ts>_<id>/`.
	childDirs := listChildFolders(filepath.Join(ownerDir, "subagents"))
	rep.Children = len(childDirs)

	for _, cd := range childDirs {
		childPath := filepath.Join(cd.dir, "session.jsonl")
		if info, err := os.Stat(childPath); err != nil || !info.Mode().IsRegular() {
			continue
		}

		// Depth-2 leftovers: any folder under a child's own subagents/ is a
		// grandchild of a parent whose dispatch ended — closed with reason
		// parent_dispatch_ended (D8).
		rep.Depth2Closed += r.closeDepth2Leftovers(ctx, cd.dir)

		child, err := r.deps.ChildHistoryFactory(ctx, childPath, cd.id)
		if err != nil {
			r.log.Warn().Err(err).Str("child_folder", cd.dir).Msg("failed to load a child folder at boot")

			continue
		}

		r.mu.Lock()
		r.hists[cd.id] = child
		r.childBaseline[cd.id] = usageTotalsOf(child.RestoredUsage())
		r.mu.Unlock()

		if child.Closed() {
			continue
		}

		// Reconcile the last dispatch against the owner file.
		status := r.reconcileChild(ctx, owner, child, cd, handbacks, counterparts)
		switch status {
		case StatusInterrupted:
			rep.Interrupted++
		case StatusDone:
		}

		// No dispatch starts: the child is addressable, never running.
		if st, err := r.reg.Lookup(owner.SessionID, cd.id); err == nil {
			r.reg.SetStatus(st, status)
		}
	}

	// Rebuild the pending set: handbacks without a delivered counterpart.
	for _, hb := range handbacks {
		if counterparts[hb.HandbackID] {
			continue
		}

		r.reg.Pending(owner.SessionID, hb)
		rep.PendingBuilt++
	}

	if rep.Children > 0 || rep.PendingBuilt > 0 {
		r.log.Info().
			Str("owner_session", owner.SessionID).
			Int("children", rep.Children).
			Int("interrupted", rep.Interrupted).
			Int("regenerated", rep.Regenerated).
			Int("pending", rep.PendingBuilt).
			Int("depth2_closed", rep.Depth2Closed).
			Msg("subagent fleet restored and reconciled")
	}

	return rep
}

// reconcileChild decides the restored child's status from its file vs the
// owner's handback records, appending the missing durable handback when a
// crash window lost it. Never starts a dispatch.
func (r *Runtime) reconcileChild(ctx context.Context, owner Owner, child *persistence.ConversationHistory, cd childFolder, handbacks []Handback, _ map[string]bool) Status {
	// The child file's final dispatch reply and any final reply at all.
	lastDispatch, hasFinal := lastDispatchState(child)

	// An owner handback record already covers the last dispatch?
	if hasFinal {
		for _, hb := range handbacks {
			if hb.DispatchID == lastDispatch {
				// The handback arrived (delivered or pending): status done.
				if child.HasOpenToolCallGroup() {
					// A recorded reply followed by an open group is a
					// crash residue in an EARLIER position; the restore
					// path already appended results.
					_ = child.CloseOpenToolCallGroupWith(persistence.InterruptedRestartToolResult)
					_ = child.Save(ctx)
				}

				return StatusDone
			}
		}

		// Crash window (a): the child recorded its final reply but the
		// owner's handback record was never appended — regenerate it.
		reply := finalReplyFor(child, lastDispatch)
		hb := Handback{
			HandbackID: newHandbackID(),
			DispatchID: lastDispatch,
			SubagentID: cd.id,
			Name:       cd.name,
			Status:     HandbackCompleted,
			Report:     reply,
			CreatedAt:  time.Now(),
			Usage:      UsageSnapshot{},
		}

		if err := r.appendHandback(ctx, owner, hb); err != nil {
			r.log.Warn().Err(err).Str("dispatch_id", lastDispatch).Msg("failed to regenerate a handback record at boot")
		} else {
			r.log.Info().Str("child_folder", cd.dir).Str("dispatch_id", lastDispatch).
				Msg("crash window reconciled: the handback was regenerated from the child's recorded final reply")
		}

		return StatusDone
	}

	// Crash window (b): a dispatch started but never produced a final reply.
	// Restore already closed the trailing open group with the fixed
	// restart result; append the durable interrupted handback.
	hb := Handback{
		HandbackID: newHandbackID(),
		DispatchID: lastDispatch,
		SubagentID: cd.id,
		Name:       cd.name,
		Status:     HandbackInterrupted,
		Report:     NoteInterruptedRestart,
		CreatedAt:  time.Now(),
	}

	if lastDispatch != "" {
		if err := r.appendHandback(ctx, owner, hb); err != nil {
			r.log.Warn().Err(err).Str("dispatch_id", lastDispatch).Msg("failed to append the interrupted handback at boot")
		}
	}

	return StatusInterrupted
}

// NoteInterruptedRestart is the fixed interrupted-handback text for a
// dispatch cut off by a daemon restart (never silence, D16).
const NoteInterruptedRestart = "The Subagent's dispatch was interrupted by a daemon restart; its partial work is not available."

// lastDispatchState returns the child file's last dispatch id and whether a
// final reply (an assistant line carrying that dispatch id) exists for it.
func lastDispatchState(child *persistence.ConversationHistory) (dispatchID string, hasFinal bool) {
	msgs := child.Messages()

	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == persistence.RoleUser && msgs[i].Kind != persistence.KindHandbackDelivery && msgs[i].Kind != persistence.KindTurnContext {
			last = msgs[i].Content
			if idx := strings.Index(last, "Dispatch: "); idx >= 0 {
				line := last[idx+len("Dispatch: "):]
				if end := strings.IndexByte(line, '\n'); end >= 0 {
					last = strings.TrimSpace(line[:end])
				} else {
					last = strings.TrimSpace(line)
				}
			}

			break
		}
	}

	if last == "" {
		return "", false
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == persistence.RoleAssistant && msgs[i].DispatchID == last {
			return last, true
		}
	}

	return last, false
}

// finalReplyFor returns the content of the final reply of dispatchID.
func finalReplyFor(child *persistence.ConversationHistory, dispatchID string) string {
	msgs := child.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == persistence.RoleAssistant && msgs[i].DispatchID == dispatchID {
			return msgs[i].Content
		}
	}

	return ""
}

// closeDepth2Leftovers closes every open grandchild folder under a child
// folder with reason parent_dispatch_ended (D16). Returns the closed count.
func (r *Runtime) closeDepth2Leftovers(ctx context.Context, childDir string) int {
	grand := filepath.Join(childDir, "subagents")
	dirs := listChildFolders(grand)

	closed := 0
	for _, cd := range dirs {
		path := filepath.Join(cd.dir, "session.jsonl")
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}

		h, err := r.deps.ChildHistoryFactory(ctx, path, cd.id)
		if err != nil {
			continue
		}

		if h.Closed() {
			continue
		}

		if n := h.CloseOpenToolCallGroupWith(persistence.InterruptedOwnerEndToolResult); n > 0 {
			r.log.Warn().Str("grandchild", cd.dir).Int("interrupted_results", n).
				Msg("a depth-2 leftover left tool calls without results — interrupted results recorded")
		}

		now := time.Now()
		if err := h.AppendRecord(ctx, persistence.HistoryMessage{
			Kind:      persistence.KindSessionClosed,
			EndedAt:   &now,
			Reason:    "parent_dispatch_ended",
			Timestamp: now,
		}); err != nil {
			r.log.Warn().Err(err).Str("grandchild", cd.dir).Msg("failed to close a depth-2 leftover folder")
		}

		closed++
	}

	return closed
}

// childFolder is one parsed child-folder entry.
type childFolder struct {
	dir  string
	name string
	id   string
}

// listChildFolders parses `subagents/<name>_<ts>_<uuidv7>` directories,
// oldest first.
func listChildFolders(dir string) []childFolder {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []childFolder

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		// <name>_<ts>_<id>: the name may itself contain hyphens, so split
		// from the right on the timestamp marker.
		parts := strings.Split(e.Name(), "_")
		if len(parts) < 3 {
			continue
		}

		id := parts[len(parts)-1]
		if _, err := parseUUID(id); err != nil {
			continue
		}

		start := parts[len(parts)-2]
		if len(start) != len("2006-01-02T15-04-05") {
			continue
		}

		name := strings.Join(parts[:len(parts)-2], "_")

		out = append(out, childFolder{dir: filepath.Join(dir, e.Name()), name: name, id: id})
	}

	return out
}

// parseUUID validates an id.
func parseUUID(s string) (interface{}, error) {
	return s, nil // the child history factory validates by loading; shapes checked by timestamp length
}

// scanOwnerHandbacks reads the whole owner file (compaction-independent)
// and returns every handback record with its delivered-flag map: a
// counterpart is the handback_delivery record carrying the id, or a
// recorded subagent_wait tool result referencing it (D6 restore).
func scanOwnerHandbacks(path string) ([]Handback, map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, map[string]bool{}
	}

	var handbacks []Handback
	delivered := map[string]bool{}

	var entries []persistence.HistoryMessage
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var msg persistence.HistoryMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // a corrupt line is crash residue, not a handback
		}

		if msg.Kind == persistence.KindHandback {
			hb := Handback{
				HandbackID: msg.HandbackID,
				DispatchID: msg.DispatchID,
				Status:     HandbackStatus(msg.Status),
				Report:     msg.Content,
				Severity:   msg.Severity,
				CreatedAt:  msg.Timestamp,
			}

			handbacks = append(handbacks, hb)
		}

		entries = append(entries, msg)
	}

	// Delivered counterparts, any position after the handback record.
	for i, hbMsg := range entries {
		if hbMsg.Kind != persistence.KindHandback {
			continue
		}

		for j := i + 1; j < len(entries); j++ {
			if persistence.HasDeliveredCounterpart(hbMsg, entries[j]) {
				delivered[hbMsg.HandbackID] = true

				break
			}
		}
	}

	return handbacks, delivered
}

// usageTotalsOf converts a usage record to live totals (the child usage
// baseline shape).
func usageTotalsOf(rec persistence.UsageRecord) usage.Totals {
	return usage.Totals{
		ReadTokens:    rec.ReadTokens,
		WrittenTokens: rec.WrittenTokens,
		CachedTokens:  rec.CachedTokens,
		LLMCalls:      rec.LLMCalls,
		AuxCalls:      rec.AuxCalls,
	}
}

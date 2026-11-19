// driver.go — the child dispatch driver (openspec change subagent-runtime,
// tasks 4.1/4.2/4.3, design D15): one dispatch per input on the generation's
// orchestrator through a leased GenEnv, with the child's own transcript,
// forwarding usage accumulator, errdigest guard, open-group closing,
// turn_usage record and handback.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/injection"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// childPreamble is the harness-authored subagent preamble — position 1 of
// the child's static prefix (D19). Static per child Session (cache-stable).
const childPreamble = "You are a Subagent of the READY home-automation agent. Your output goes to another model — the owner agent — never directly to a human: no channel formatting applies. Complete the task autonomously and reply with a dense factual report. Do not ask questions; state blockers explicitly. A bounded iteration budget applies to this dispatch."

// startDispatch launches the first dispatch goroutine of a freshly spawned
// child with its task (scanned at enqueue, D10).
func (r *Runtime) startDispatch(ctx context.Context, owner Owner, st *Agent, task string) {
	scanned := injection.Scan(task, injection.ModeFull)
	r.launch(ctx, owner, st, scanned.Text, false)
}

// startDispatchWithMail launches a reopen dispatch with owner mail (already
// scanned at enqueue).
func (r *Runtime) startDispatchWithMail(ctx context.Context, owner Owner, st *Agent, scannedMail string) {
	r.launch(ctx, owner, st, scannedMail, false)
}

// launch runs the dispatch loop of a child in its own goroutine: one
// dispatch per input, then queued mail FIFO on the same slot (D4/D7).
func (r *Runtime) launch(ctx context.Context, owner Owner, st *Agent, input string, _ bool) {
	hctx, cancel := context.WithCancel(ctx)
	h := &dispatchHandle{cancel: cancel, done: make(chan struct{})}

	r.mu.Lock()
	r.disp[st.ID] = h
	r.mu.Unlock()

	go func() {
		defer func() {
			// Panic recovery (same policy as the session workers): a panic
			// in the dispatch path must never kill the daemon nor leave the
			// owner without a result. The recovered panic is logged with its
			// stack and an interrupted handback is delivered when possible.
			// Cleanup uses TryLock because the panic may have left r.mu held
			// (a panic inside a critical section) — re-locking it would
			// deadlock this defer forever (reproduced 2026-11-16: a nil-map
			// panic in recordChildUsage wedged the whole fleet).
			var panicked any
			if p := recover(); p != nil {
				panicked = p
				r.log.Error().
					Str("subagent", st.Name).
					Str("panic", fmt.Sprintf("%v", p)).
					Str("stack", string(debug.Stack())).
					Msg("subagent dispatch panicked — delivering an interrupted handback; the panic was recovered, this stack identifies the bug")
			}

			// Best-effort interrupted handback so the owner always receives a
			// result (the dispatch's own flow died mid-panic).
			if panicked != nil {
				if child := r.childHistory(st.ID); child != nil {
					if n := child.CloseOpenToolCallGroup(); n > 0 {
						r.log.Warn().Str("subagent", st.Name).Int("interrupted_results", n).
							Msg("panicked child dispatch left tool calls without results — interrupted results recorded")
					}
				}

				st.Status = StatusFailed
				r.deliverHandback(context.Background(), owner, st, Handback{
					HandbackID: newHandbackID(),
					DispatchID: st.ID + "-panic",
					SubagentID: st.ID,
					Name:       st.Name,
					Status:     HandbackInterrupted,
					Report:     NoteInterrupted,
					CreatedAt:  time.Now(),
				})
			}

			cancel()
			close(h.done)

			if r.mu.TryLock() {
				delete(r.disp, st.ID)
				r.mu.Unlock()
			} else {
				r.log.Error().Str("subagent", st.Name).
					Msg("subagent runtime mutex held by the panicking dispatch — the fleet entry stays until the next owner-end cascade")
			}

			r.notifyFleet(owner.SessionID)
		}()

		inputs := []string{input}

		// The dispatch loop: current input first, then the queued mail
		// FIFO — each its own dispatch and its own handback, on the same
		// concurrency slot (D7: "the next dispatch starts right after the
		// current one").
		for {
			r.runOneDispatch(hctx, owner, st, inputs[0])

			if hctx.Err() != nil {
				return // cancelled: queued mail is dropped with the dispatch
			}

			mail, err := st.Mail.Pop()
			if err != nil {
				break // mailbox empty: the child settles
			}

			inputs[0] = mail.Scanned
		}

		// Idle: release the concurrency slot (a reopen re-acquires it).
		if hctx.Err() == nil {
			r.reg.SetStatus(st, st.Status)
			r.releaseSlot(owner, st)
		}
	}()
}

// releaseSlot marks the child idle and releases its slot; mail arriving
// before the release re-acquires nothing (the slot transfer happens in the
// send path).
func (r *Runtime) releaseSlot(owner Owner, st *Agent) {
	if st.Mail.Len() > 0 {
		return // mail raced in: keep the slot for the next dispatch
	}

	if st.Status == StatusRunning {
		st.Status = StatusDone
	}

	r.reg.ReleaseSlot(owner.SessionID, st)
	r.notifyFleet(owner.SessionID)
}

// runOneDispatch executes ONE dispatch end to end and delivers its
// handback. The slot is already held (spawn or reopen).
func (r *Runtime) runOneDispatch(ctx context.Context, owner Owner, st *Agent, input string) {
	startedAt := r.now()
	seq := r.reg.NextDispatch(st)
	dispatchID := dispatchSeq(st.ID, seq)

	env, releaseGen := r.deps.Generation(ctx)
	defer releaseGen()

	child, err := r.childSession(ctx, owner, st)
	if err != nil {
		r.log.Error().Err(err).Str("subagent", st.Name).Msg("failed to open the child session")
		r.deliverHandback(ctx, owner, st, buildHandback(st, dispatchID, agent.TurnOutcome{}, err, startedAt, UsageSnapshot{}, r.deps.HandbackMaxTokens, r.deps.Severity))

		return
	}

	// The child's own agentskills session (D19), closed with the child —
	// never with the dispatch.
	sid, _ := r.deps.Skills.Session(ctx, st.ID)

	// The child's forwarding accumulator (D9): every model call — ReAct
	// iterations, failover attempts, the child's compaction summariser —
	// accrues to the owner Session's totals.
	ownerAcc := r.deps.OwnerUsage(owner.SessionID)
	childAcc := usage.NewDetached(st.ID, ownerAcc)

	// The dispatch input is the first user record of this runtime, behind
	// the harness header (D15).
	child.Add(persistence.HistoryMessage{
		Role:      persistence.RoleUser,
		Content:   buildTaskHeader(dispatchID, owner, startedAt) + "\n\n" + input,
		Timestamp: time.Now(),
	})

	// Child catalog (D12): owner catalog → definition restriction →
	// structural exclusions. The set is stable for the dispatch (a
	// generation-scoped property).
	childCtx := context.Background()
	tools := childCatalog(childCtx, env.OwnerTools, st.Snapshot, st.Depth >= r.deps.Limits.MaxDepth, r.log)

	// Runner set (D17): built per (generation, definition snapshot) over
	// the child catalog.
	pin := st.Snapshot.EffectiveModel()
	if pin == ModelInherit {
		pin = ""
	}

	runners, rerr := env.RunnerSet(ctx, st.Snapshot, pin, tools)
	if rerr != nil || len(runners) == 0 {
		r.log.Warn().Err(rerr).Str("subagent", st.Name).Msg("failed to build the child runner set")
		r.deliverHandback(ctx, owner, st, buildHandback(st, dispatchID, agent.TurnOutcome{}, rerr, startedAt, UsageSnapshot{}, r.deps.HandbackMaxTokens, r.deps.Severity))

		return
	}

	prefix := r.childPrefix(ctx, sid, st.Snapshot, env)

	resumeReady := func() bool { return !child.HasOpenToolCallGroup() }

	runCtx, runCancel := context.WithTimeout(ctx, r.dispatchTimeout(st.Snapshot))
	defer runCancel()

	runCtx = agent.WithTranscriptSink(runCtx, fmt.Sprintf("subagent-%s", dispatchID), newChildRecorder(ctx, child, r.log))
	runCtx = usage.WithAccumulator(runCtx, childAcc)

	req := orchestrator.TurnRequest{
		SessionKey:  st.ID,
		Assemble:    r.childAssemble(prefix, child),
		ResumeReady: resumeReady,
		Runners:     runners,
		Only:        pin,
		Priority:    r.priorityFor(owner),
	}

	outcome, runErr := r.executeTurnWithDeadline(runCtx, env, req, st)

	// A failed dispatch closes its unanswered tool calls (join semantics:
	// the executor has returned, or was abandoned at the deadline).
	if runErr != nil {
		if n := child.CloseOpenToolCallGroup(); n > 0 {
			r.log.Warn().Str("subagent", st.Name).Int("interrupted_results", n).
				Msg("failed child dispatch left tool calls without results — interrupted results recorded")
		}
	}

	// The final reply carries the dispatch_id (D6); the iteration limit's
	// partial result is the last pre-action text.
	finalReply := outcome.Reply
	if runErr != nil && isIterationLimit(runErr) {
		finalReply = lastPreActionText(child)
	}

	if runErr == nil {
		reply := persistence.HistoryMessage{
			Role:       persistence.RoleAssistant,
			Content:    strings.TrimSpace(finalReply),
			DispatchID: dispatchID,
			Timestamp:  time.Now(),
		}
		child.Add(reply)
	}

	// The dispatch's usage delta on the child file (turn_usage).
	r.recordChildUsage(st, childAcc)

	// The child's own Compaction evaluation (task 4.3), after the reply is
	// recorded and never delaying the handback.
	r.compactChild(ctx, st, child, childAcc)

	// Failure status bookkeeping.
	status := StatusDone
	if runErr != nil {
		status = StatusFailed
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = StatusInterrupted
		}
	}
	r.reg.SetStatus(st, status)

	hb := buildHandback(st, dispatchID, agent.TurnOutcome{Reply: finalReply}, runErr, startedAt, usageSnapshot(childAcc, startedAt), r.deps.HandbackMaxTokens, r.deps.Severity)

	// Depth-2 dispatch scoping (task 4.6): when a depth-1 child's dispatch
	// ends, its running grandchildren are interrupted and their unconsumed
	// handbacks folded (capped) into the parent's handback.
	if st.Depth < r.deps.Limits.MaxDepth {
		if folded := r.scopeGrandchildren(ctx, owner, st); folded != "" {
			hb.Report = capTokens(hb.Report+"\n\n"+folded, r.deps.HandbackMaxTokens)
		}
	}

	r.deliverHandback(ctx, owner, st, hb)
}

// deliverHandback appends the handback to the owner file (arrival, D6a),
// enters the pending set, and runs the owner's delivery policy.
func (r *Runtime) deliverHandback(ctx context.Context, owner Owner, st *Agent, hb Handback) {
	if err := r.appendHandback(ctx, owner, hb); err != nil {
		r.log.Error().Err(err).Str("handback_id", hb.HandbackID).Msg("failed to append the handback record")
	}

	// The child also records the dispatch outcome status for the fleet.
	if owner.Kind == OwnerJob && owner.Delivery.Kind == DeliveryCallback && owner.Delivery.Callback != nil {
		// The job policy (D20): the callback receives the handback exactly
		// once, whatever its status. The handback was already recorded on
		// the job file (the owner's History); the job close happens in the
		// cmd-side callback wrapper.
		owner.Delivery.Callback(ctx, hb)
	}

	// The passive policy (D6c): the cmd-side wiring decides per channel.
	if owner.Kind == OwnerChannel {
		r.NotifyPending(ctx, owner)

		// The immediate-visibility signal: the connected TUI learns the
		// Subagent finished right now (the result content itself still
		// lands at the head of the owner's next turn). The dispatch's
		// wall-clock duration rides along (protocol 6): the usage snapshot
		// recorded it at handback-build time from the dispatch start; zero
		// (a pre-turn failure built with an empty snapshot) means unknown
		// and the wiring omits the wire field.
		if r.deps.HandbackDelivered != nil {
			var elapsedMS int64
			if hb.Usage.Duration > 0 {
				elapsedMS = hb.Usage.Duration.Milliseconds()
			}
			r.deps.HandbackDelivered(owner.SessionID, st.Name, string(hb.Status), elapsedMS)
		}
	}

	r.log.Info().
		Str("owner_session", owner.SessionID).
		Str("subagent", st.Name).
		Str("handback_id", hb.HandbackID).
		Str("dispatch_id", hb.DispatchID).
		Str("status", string(hb.Status)).
		Int("report_chars", len(hb.Report)).
		Msg("subagent handback delivered")
}

// executeTurnWithDeadline runs the turn executor and returns its outcome,
// enforcing the dispatch deadline even when the executor fails to react to
// context cancellation: ExecuteTurn runs in a goroutine and, when runCtx
// expires (or the parent is cancelled) before the executor returns, the
// executor is abandoned — its late outcome, if it ever completes, is logged
// and dropped — and the dispatch proceeds with the context error, so the
// owner always receives a handback and the owner-end cascade's join never
// blocks on a wedged executor.
func (r *Runtime) executeTurnWithDeadline(runCtx context.Context, env GenEnv, req orchestrator.TurnRequest, st *Agent) (agent.TurnOutcome, error) {
	type turnResult struct {
		outcome agent.TurnOutcome
		err     error
	}

	res := make(chan turnResult, 1)
	started := time.Now()
	go func() {
		// The executor runs on its own goroutine: a panic there is recovered
		// HERE (never catchable from the dispatch goroutine) and converted
		// into a turn error, so the handback machinery still runs.
		defer func() {
			if p := recover(); p != nil {
				r.log.Error().
					Str("subagent", st.Name).
					Str("panic", fmt.Sprintf("%v", p)).
					Str("stack", string(debug.Stack())).
					Msg("subagent turn executor panicked — the dispatch fails with the panic error; this stack identifies the bug")

				res <- turnResult{err: fmt.Errorf("turn executor panicked: %v", p)}
			}
		}()

		r.log.Debug().Str("subagent", st.Name).Msg("subagent turn executor started")

		outcome, err := env.ExecuteTurn(runCtx, req, nil)

		r.log.Info().Str("subagent", st.Name).
			Dur("elapsed", time.Since(started)).
			Bool("error", err != nil).
			Bool("reply", outcome.Reply != "").
			Bool("tool_call_completed", outcome.ToolCallCompleted).
			Msg("subagent turn executor returned")

		res <- turnResult{outcome: outcome, err: err}
	}()

	select {
	case tr := <-res:
		return tr.outcome, tr.err
	case <-runCtx.Done():
		// Both ready at once (the executor noticed the cancellation exactly
		// as the deadline fired): prefer its real result.
		select {
		case tr := <-res:
			return tr.outcome, tr.err
		default:
		}

		err := runCtx.Err()
		r.log.Warn().Str("subagent", st.Name).Err(err).
			Msg("subagent dispatch deadline reached while the turn executor was still running — executor abandoned, interrupted handback follows")

		go func() {
			late := <-res
			r.log.Info().Str("subagent", st.Name).Bool("late_error", late.err != nil).
				Msg("abandoned subagent executor completed after its dispatch was abandoned; its late outcome was dropped (handback already delivered)")
		}()

		return agent.TurnOutcome{}, err
	}
}

// dispatchTimeout bounds one dispatch: the definition's timeout (default:
// the main agent's run deadline — expressed by the absence of an extra
// deadline, i.e. a long budget the caller ctx bounds anyway).
func (r *Runtime) dispatchTimeout(def Definition) time.Duration {
	if def.Timeout > 0 {
		return def.Timeout
	}

	return defaultDispatchTimeout
}

// defaultDispatchTimeout is the fallback dispatch deadline (the main
// agent's run deadline shape: 300s — cmd can override via the definition).
const defaultDispatchTimeout = 5 * time.Minute

// priorityFor maps the owner kind to the provider-slot priority class (D18).
func (r *Runtime) priorityFor(owner Owner) int {
	if owner.Kind == OwnerJob {
		return orchestrator.PriorityJob
	}

	return orchestrator.PrioritySubagent
}

// usageSnapshot renders a child accumulator's delta as the informational
// handback usage.
func usageSnapshot(acc *usage.Accumulator, startedAt time.Time) UsageSnapshot {
	tot := acc.Snapshot()
	if tot == nil {
		return UsageSnapshot{Duration: time.Since(startedAt)}
	}

	return UsageSnapshot{
		ReadTokens:    tot.ReadTokens,
		WrittenTokens: tot.WrittenTokens,
		LLMCalls:      tot.LLMCalls,
		Duration:      time.Since(startedAt),
	}
}

// childSession returns (creating, restoring) the child's transcript
// history, owned by the Runtime — not the owner's LRU (D15).
func (r *Runtime) childSession(ctx context.Context, owner Owner, st *Agent) (*persistence.ConversationHistory, error) {
	r.mu.Lock()
	if h, ok := r.hists[st.ID]; ok {
		r.mu.Unlock()

		return h, nil
	}
	r.mu.Unlock()

	// The child folder hangs from the owner Session folder (D5).
	ownerPath := ""
	if owner.History != nil {
		ownerPath = owner.History.Path()
	}

	ownerDir := "."
	if ownerPath != "" {
		ownerDir = parentDir(ownerPath)
	}

	path := persistence.ChildSessionPath(ownerDir, st.Name, st.ID, r.now())

	h, err := r.deps.ChildHistoryFactory(ctx, path, st.ID)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	r.hists[st.ID] = h
	r.mu.Unlock()

	return h, nil
}

// childAssemble builds the per-attempt assembly closure: static prefix +
// child summary lead + live history projection (layout-independent — no
// turn context, no head-safe fold, D15). The closure re-reads the LIVE
// history each attempt so a retry continues from the last complete
// tool-call group.
func (r *Runtime) childAssemble(prefix []*schema.Message, child *persistence.ConversationHistory) func(llm.PromptLayout, string) []*schema.Message {
	return func(_ llm.PromptLayout, _ string) []*schema.Message {
		view := child.Messages()

		out := make([]*schema.Message, 0, len(prefix)+len(view)+1)
		out = append(out, prefix...)
		for _, m := range view {
			out = append(out, toMessage(m))
		}

		return out
	}
}

// buildTaskHeader renders the dispatch input's harness header (dispatch
// time, owner — D15).
func buildTaskHeader(dispatchID string, owner Owner, at time.Time) string {
	var b strings.Builder
	b.WriteString("<" + injection.TagTaskHeader + "_" + injection.NewID() + ">\n")
	b.WriteString("This is a task handed to you by the owner agent.\n")
	b.WriteString("Dispatch: " + dispatchID + "\n")
	b.WriteString("Owner: " + owner.Address + "\n")
	b.WriteString("Time: " + at.Format(time.RFC3339) + "\n")
	b.WriteString("</" + injection.TagTaskHeader + ">\n")

	return b.String()
}

// lastPreActionText returns the child's last assistant content that carried
// no final answer — the iteration limit's partial result (D6).
func lastPreActionText(child *persistence.ConversationHistory) string {
	msgs := child.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == persistence.RoleAssistant && strings.TrimSpace(m.Content) != "" && len(m.ToolCalls) > 0 {
			return strings.TrimSpace(m.Content)
		}
	}

	return ""
}

// newChildRecorder is the child transcript recorder (the same mechanics as
// the owner path's transcriptRecorder: assistant tool-call messages and
// tool results enter the history as they materialise; each completed
// tool-call group is delta-appended).
func newChildRecorder(ctx context.Context, child *persistence.ConversationHistory, logger zerolog.Logger) func(schema.Message) {
	return func(m schema.Message) {
		line := persistence.HistoryMessage{Content: m.Content, Timestamp: time.Now()}

		switch m.Role {
		case schema.Assistant:
			line.Role = persistence.RoleAssistant
			for _, tc := range m.ToolCalls {
				line.ToolCalls = append(line.ToolCalls, persistence.ToolCallRecord{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		case schema.Tool:
			line.Role = persistence.RoleTool
			line.ToolCallID = m.ToolCallID
		default:
			return
		}

		child.Add(line)

		if line.Role == persistence.RoleTool && !child.HasOpenToolCallGroup() {
			if err := child.Save(ctx); err != nil {
				logger.Error().Err(err).Msg("failed to save the child tool-call group checkpoint")
			}
		}
	}
}

// isIterationLimit reports the ADK's iteration-limit sentinel (task 0.2:
// errors.Is reaches it through the compose wrapping).
func isIterationLimit(err error) bool {
	return errors.Is(err, errExceedMaxIterations)
}

// parentDir returns dir's parent.
func parentDir(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return "."
	}

	return path[:i]
}

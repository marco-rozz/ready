// job.go — internal job owners (openspec change subagent-runtime, tasks
// 5b.1/5b.2/5b.3, design D20): internal daemon features run agent work
// through RunJob, never inside a user Session. Each job creates its own
// owner Session at the reserved address `system`, spawns exactly one
// Subagent at depth 1 with the structural tool exclusions, delivers its
// handback — whatever its status — exactly once to the launcher's callback,
// records that handback on the job Session file, and then closes the job
// Session with reason `job_done`. Jobs are always available (they bypass
// the `subagents.enabled` gate) and are governed by their own feature flags.
package subagent

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/marco-rozz/ready/internal/persistence"
)

// JobSpec describes one internal job.
type JobSpec struct {
	// Name is a diagnostic label (log fields).
	Name string
	// Type is the definition name (a `visibility: internal` definition is
	// job-only — allowed here, refused for model spawns).
	Type string
	// Task is the dispatch input (scanned by the Runtime).
	Task string
	// OnHandback is the launcher's callback: called exactly once, whatever
	// the handback status.
	OnHandback func(ctx context.Context, hb Handback)
}

// JobSessionFactory builds a job owner: a fresh Session at the reserved
// address `system` with its transcript history (cmd wires the layout).
type JobSessionFactory func(ctx context.Context, jobID string, now time.Time) (Owner, error)

// RunJob runs one job synchronously: at most max_concurrent_jobs at a time
// (beyond it the fixed structural error — the launcher retries later, and
// every job MUST be idempotent). Blocks until the handback is delivered.
func (r *Runtime) RunJob(ctx context.Context, spec JobSpec) error {
	if err := r.reg.BeginJob(); err != nil {
		return err
	}
	defer r.reg.EndJob()

	if r.deps.JobSessionFactory == nil {
		return errNoJobFactory
	}

	jobID, err := uuid.NewV7()
	if err != nil {
		return err
	}

	owner, err := r.deps.JobSessionFactory(ctx, jobID.String(), r.now())
	if err != nil {
		return err
	}

	started := r.now()

	// The job's own usage accounting (D20): INFO log at job end, never in a
	// user's totals. The child accumulator gets a detached owner with no
	// forwarding parent — the job owner's own usage stays its own.
	var logMu sync.Mutex

	// The delivery policy (callback): the child driver's deliverHandback
	// path already records the handback on the owner (job) file through the
	// owner History; the callback wrapper adds the INFO log and closes the
	// job Session exactly once.
	closed := false
	owner.Delivery = Delivery{
		Kind: DeliveryCallback,
		Callback: func(ctx context.Context, hb Handback) {
			logMu.Lock()
			defer logMu.Unlock()

			if spec.OnHandback != nil {
				spec.OnHandback(ctx, hb)
			}

			if closed {
				return
			}
			closed = true

			// Job usage: the child files under the job tree carry it; the
			// owner file's turn_usage... jobs record none — the INFO log
			// reads the child usage from the handback block.
			r.logJobDone(owner, spec, hb, started)

			r.closeJobSession(ctx, owner)
		},
	}

	// Spawn: one Subagent from the given definition with the task, depth 1,
	// structural exclusions (the child catalog applies them), bypassing the
	// user-facing gate.
	st, err := r.Spawn(ctx, owner, nil, SpawnRequest{Type: spec.Type, Task: spec.Task})
	if err != nil {
		return err
	}

	r.log.Info().
		Str("job", spec.Name).
		Str("job_session", owner.SessionID).
		Str("subagent", st.Name).
		Msg("internal job started")

	// Wait for the dispatch to finish (the callback fired).
	<-st.dispatchDone(r)

	return nil
}

// closeJobSession appends the job Session's closing record (reason
// job_done) and releases the state.
func (r *Runtime) closeJobSession(ctx context.Context, owner Owner) {
	if owner.History == nil || owner.History.Closed() {
		return
	}

	now := r.now()
	rec := persistence.HistoryMessage{
		Kind:      persistence.KindSessionClosed,
		EndedAt:   &now,
		Reason:    "job_done",
		Timestamp: now,
	}

	if err := owner.History.AppendRecord(ctx, rec); err != nil {
		r.log.Warn().Err(err).Str("job_session", owner.SessionID).Msg("failed to append the job closing record")
	}

	r.reg.DropOwner(owner.SessionID)
}

// logJobDone writes the INFO completion log (tokens, duration, status).
func (r *Runtime) logJobDone(owner Owner, spec JobSpec, hb Handback, started time.Time) {
	r.log.Info().
		Str("job", spec.Name).
		Str("job_session", owner.SessionID).
		Str("status", string(hb.Status)).
		Int("read_tokens", hb.Usage.ReadTokens).
		Int("written_tokens", hb.Usage.WrittenTokens).
		Int("llm_calls", hb.Usage.LLMCalls).
		Dur("duration", time.Since(started)).
		Msg("internal job done")
}

// dispatchDone blocks until the child's dispatch goroutine returned (the
// job is synchronous over one dispatch).
func (st *Agent) dispatchDone(r *Runtime) chan struct{} {
	r.mu.Lock()
	h := r.disp[st.ID]
	r.mu.Unlock()

	if h != nil {
		return h.done
	}

	closed := make(chan struct{})
	close(closed)

	return closed
}

// errNoJobFactory is the wiring error when jobs are requested without a
// factory (a configuration bug, not a user-facing condition).
var errNoJobFactory = zerologError("subagent: no job session factory wired")

// zerologError is a tiny errors.New alias helper for this file's sentinel.
func zerologError(text string) error { return errText{text} }

type errText struct{ s string }

func (e errText) Error() string { return e.s }

// notifyFleet fires the fleet-changed hook (wired to the coalesced ZMQ
// snapshot emission). Jobs emit no snapshot (D20): a job owner's fleet
// changes are suppressed.
func (r *Runtime) notifyFleet(ownerSessionID string) {
	if o := r.fleetOwner(ownerSessionID); o != nil && o.Kind == OwnerJob {
		return
	}

	if r.deps.FleetChanged != nil {
		r.deps.FleetChanged(ownerSessionID)
	}
}

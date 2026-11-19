// runtime.go — the Subagent Runtime service (openspec change
// subagent-runtime, D1/D4/D6): spawn, mail, wait, interrupt, the handback
// arrival and its two consumption doors, the passive notice, owner cascade
// end, daemon-shutdown interruption, and the boot reconciliation. The
// registry owns limits; this file owns lifecycle and durability.
package subagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/injection"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// dispatchHandle is the Runtime's execution state for one live child
// (goroutine + cancellation), separate from the registry's Agent record.
type dispatchHandle struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the goroutine returns
}

// Runtime is the daemon-first Subagent Runtime: process-lifetime, outside
// the reloadable runtime generation (D14). Every operation names an Owner.
type Runtime struct {
	deps Deps
	// enabled is the user-facing delegation gate: seeded from
	// deps.Enabled at boot and flipped by SetEnabled on the
	// `subagents.enabled` reload flip (D14) — a false→true flip must not
	// wait for a restart. Jobs bypass the gate entirely.
	enabled       atomic.Bool
	reg           *Registry
	defs          *Definitions
	log           zerolog.Logger
	now           func() time.Time
	mu            sync.RWMutex
	hists         map[string]*persistence.ConversationHistory // child Session ID → history
	childBaseline map[string]usage.Totals                     // child Session ID → cumulative usage at the last recorded dispatch
	disp          map[string]*dispatchHandle                  // child Session ID → live dispatch
	// pendingCh wakes blocked subagent_wait callers on a new arrival.
	pendingCh map[string]chan struct{} // owner Session ID → broadcast channel
}

// New builds the Runtime over the loaded definitions.
func New(deps Deps, defs *Definitions) *Runtime {
	if deps.Limits.MaxDepth <= 0 {
		deps.Limits.MaxDepth = DefaultMaxDepth
	}
	if deps.Limits.MaxConcurrent <= 0 {
		deps.Limits.MaxConcurrent = DefaultMaxConcurrent
	}
	if deps.Limits.MaxConcurrentJobs <= 0 {
		deps.Limits.MaxConcurrentJobs = DefaultMaxConcurrentJobs
	}
	if deps.Limits.MailboxBuffer <= 0 {
		deps.Limits.MailboxBuffer = DefaultMailboxBuffer
	}
	if deps.HandbackMaxTokens <= 0 {
		deps.HandbackMaxTokens = 4096
	}
	if deps.HandbackTurnBudgetTokens <= 0 {
		deps.HandbackTurnBudgetTokens = 16384
	}
	if deps.WaitTimeout <= 0 {
		deps.WaitTimeout = 60 * time.Second
	}
	if deps.Redact == nil {
		deps.Redact = func(text string) string { return text }
	}
	if deps.OwnerUsage == nil {
		deps.OwnerUsage = func(string) *usage.Accumulator { return nil }
	}

	rt := &Runtime{
		deps:          deps,
		reg:           NewRegistry(deps.Limits, deps.Logger),
		defs:          defs,
		log:           deps.Logger.With().Str("component", "subagent-runtime").Logger(),
		now:           time.Now,
		hists:         map[string]*persistence.ConversationHistory{},
		disp:          map[string]*dispatchHandle{},
		pendingCh:     map[string]chan struct{}{},
		childBaseline: map[string]usage.Totals{},
	}
	rt.enabled.Store(deps.Enabled)

	return rt
}

// SetDefinitions (re)installs the loaded definition set — boot loading and
// the reload revalidation hand their results here.
func (r *Runtime) SetDefinitions(defs *Definitions) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.defs = defs
}

// Registry exposes the registry (snapshot building, limits).
func (r *Runtime) Registry() *Registry { return r.reg }

// Definitions exposes the loaded definitions.
func (r *Runtime) Definitions() *Definitions { return r.defs }

// DefaultTypeName returns the default spawn type name (general-purpose).
func (r *Runtime) DefaultTypeName() string {
	if def, ok := r.defs.DefaultType(); ok {
		return def.Name
	}

	return ""
}

// gate reports whether user-facing delegation is enabled for an owner.
func (r *Runtime) gate(owner Owner) error {
	if owner.Kind == OwnerJob {
		return nil // jobs are infrastructure, always available (D13)
	}

	if !r.enabled.Load() {
		return ErrFeatureDisabled
	}

	return nil
}

// SetEnabled flips the user-facing delegation gate (the
// `subagents.enabled` reload flip, D14): true opens Spawn/Send/Interrupt
// for channel owners, false closes them (fleets are cascade-closed
// separately by DisableChannelFleets BEFORE the reload takes the write
// lock). Jobs bypass the gate either way.
func (r *Runtime) SetEnabled(on bool) {
	r.enabled.Store(on)
}

// SpawnRequest carries a spawn request.
type SpawnRequest struct {
	// Type is the definition name; empty = the default type.
	Type string
	// Name is optional; auto <type>-<n> when empty.
	Name string
	// Task is the dispatch input (scanned by the Runtime).
	Task string
	// ModelPin overrides the definition's model ("" = the definition's).
	ModelPin string
}

// Spawn registers a Subagent under owner and starts its first dispatch with
// the task. It returns immediately (never blocks on the child's work).
// parent is nil for an owner-level spawn (depth 1) and the spawning child
// at depth 2.
func (r *Runtime) Spawn(ctx context.Context, owner Owner, parent *Agent, opts SpawnRequest) (*Agent, error) {
	if err := r.gate(owner); err != nil {
		return nil, err
	}

	typeName := opts.Type
	if typeName == "" {
		typeName = r.DefaultTypeName()
		if typeName == "" {
			return nil, fmt.Errorf("%w: no public definitions are loaded", ErrDefinitionUnknown)
		}
	}

	def, ok := r.defs.Get(typeName)
	if !ok || (def.Internal() && owner.Kind != OwnerJob) {
		// Internal definitions are job-only: a model spawn sees them as
		// unknown (D20).
		return nil, fmt.Errorf("%w: %q", ErrDefinitionUnknown, typeName)
	}

	def, err := r.resolvePin(ctx, def, opts.ModelPin)
	if err != nil {
		return nil, err
	}

	depth, parentName := 1, ""
	if parent != nil {
		depth, parentName = parent.Depth+1, parent.Name
	}

	st, err := r.reg.Spawn(owner, SpawnOpts{Type: typeName, Name: opts.Name, Depth: depth, ParentName: parentName})
	if err != nil {
		return nil, err
	}

	st.Snapshot = def

	r.startDispatch(ctx, owner, st, opts.Task)

	// The spawn itself is a fleet change: the TUI's panel must show the
	// Subagent as running immediately, not only at the dispatch's end.
	r.notifyFleet(owner.SessionID)

	return st, nil
}

// resolvePin validates the effective model pin against the current
// generation's chain (a pin outside the chain is a fixed structural error).
func (r *Runtime) resolvePin(_ context.Context, def Definition, override string) (Definition, error) {
	pin := override
	if pin == "" {
		pin = def.EffectiveModel()
	}

	if pin == ModelInherit {
		def.Model = ModelInherit

		return def, nil
	}

	if !r.chainHas(pin) {
		return Definition{}, fmt.Errorf("%w: %q", ErrModelPinOutsideChain, pin)
	}

	def.Model = pin

	return def, nil
}

// chainHas reports whether the current generation's chain carries ref.
func (r *Runtime) chainHas(ref string) bool {
	env, release := r.deps.Generation(context.Background())
	defer release()

	for _, c := range env.ChainRefs {
		if c == ref {
			return true
		}
	}

	return false
}

// Send delivers owner mail to a child: a running child queues it in its
// bounded mailbox (full ⇒ the fixed backpressure error); an idle child is
// reopened — a new dispatch on the same Session file with the prior
// transcript (D4). sender is the owner (nil) or the parent child at depth 2.
func (r *Runtime) Send(ctx context.Context, owner Owner, sender *Agent, idOrName, text string) error {
	if err := r.gate(owner); err != nil {
		return err
	}

	st, err := r.reg.Lookup(owner.SessionID, idOrName)
	if err != nil {
		return err
	}

	// No peer mail: only the owner (or the parent child at depth 2) sends.
	if sender != nil && sender.Name != st.Parent {
		return fmt.Errorf("%w: only the owner (or the parent child) may send to %q", ErrUnknownAgent, st.Name)
	}

	// Scan the mail at enqueue time (D10); the child receives and records
	// the scan-mutated form with its header.
	scanned := injection.Scan(text, injection.ModeFull)
	delivered := scanned.Text

	// A running child (or one with queued mail holding its slot) queues.
	if st.Status == StatusRunning || st.Mail.Len() > 0 {
		mail := Mail{Text: text, Scanned: delivered, OwnerName: owner.Address}
		if err := r.reg.QueueMail(st, mail); err != nil {
			return err
		}

		r.log.Debug().Str("subagent", st.Name).Int("queued", st.Mail.Len()).Msg("owner mail queued")

		return nil
	}

	// Reopen: acquire the slot (fail-fast on the cap), then dispatch.
	if err := r.reg.Reopen(owner, st); err != nil {
		return err
	}

	r.reg.SetStatus(st, StatusRunning)
	r.startDispatchWithMail(ctx, owner, st, delivered)

	// The reopen is a fleet change too (the Subagent went running again).
	r.notifyFleet(owner.SessionID)

	return nil
}

// List returns the owner's fleet snapshot and the pending-handback count.
func (r *Runtime) List(owner Owner) ([]AgentView, int) {
	return r.reg.Agents(owner.SessionID), r.reg.PendingCount(owner.SessionID)
}

// WaitResult is the outcome of subagent_wait: the handbacks delivered
// through the wait door (consumed — never injected again) and whether the
// call timed out.
type WaitResult struct {
	Handbacks []Handback
	TimedOut  bool
}

// Wait returns every pending handback of the addressed children immediately;
// when none is pending it blocks until at least one arrives or the timeout
// expires (default wait_timeout, clamped to the owner Turn's remaining
// deadline by the caller's ctx). On timeout the children keep running and
// their eventual handbacks become pending.
func (r *Runtime) Wait(ctx context.Context, owner Owner, targets []string, timeout time.Duration) (WaitResult, error) {
	if timeout <= 0 {
		timeout = r.deps.WaitTimeout
	}

	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}

	if timeout <= 0 {
		return WaitResult{}, ctx.Err()
	}

	ids, err := r.resolveTargets(owner, targets)
	if err != nil {
		return WaitResult{}, err
	}

	// Fast path: pending handbacks of the targets.
	if hbs := r.takePending(owner, ids); len(hbs) > 0 {
		return WaitResult{Handbacks: hbs}, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	wake := r.waitChannel(owner.SessionID)

	for {
		select {
		case <-timer.C:
			return WaitResult{TimedOut: true}, nil
		case <-ctx.Done():
			return WaitResult{}, ctx.Err()
		case <-wake:
			if hbs := r.takePending(owner, ids); len(hbs) > 0 {
				return WaitResult{Handbacks: hbs}, nil
			}
		}
	}
}

// resolveTargets maps id-or-name targets to child Session IDs.
func (r *Runtime) resolveTargets(owner Owner, targets []string) ([]string, error) {
	ids := make([]string, 0, len(targets))

	for _, t := range targets {
		st, err := r.reg.Lookup(owner.SessionID, t)
		if err != nil {
			return nil, err
		}

		ids = append(ids, st.ID)
	}

	return ids, nil
}

// takePending removes and returns the pending handbacks of ids (consumed by
// the wait door — the recorded tool result references their ids, D6).
func (r *Runtime) takePending(owner Owner, ids []string) []Handback {
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	var out []Handback

	for _, hb := range r.reg.PendingList(owner.SessionID) {
		if idSet[hb.SubagentID] && r.reg.RemovePending(owner.SessionID, hb.HandbackID) {
			out = append(out, *hb)
		}
	}

	// The pending count changed: the TUI's ◆ badge must drop without
	// waiting for the next fleet transition.
	if len(out) > 0 {
		r.notifyFleet(owner.SessionID)
	}

	return out
}

// waitChannel returns (creating) the owner's arrival broadcast channel.
func (r *Runtime) waitChannel(ownerSessionID string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()

	ch, ok := r.pendingCh[ownerSessionID]
	if !ok {
		ch = make(chan struct{}, 1)
		r.pendingCh[ownerSessionID] = ch
	}

	return ch
}

// wakeWaiters signals the owner's blocked waiters (non-blocking).
func (r *Runtime) wakeWaiters(ownerSessionID string) {
	r.mu.Lock()
	ch := r.pendingCh[ownerSessionID]
	r.mu.Unlock()

	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// PendingCount reports the owner's pending handbacks (the ZMQ badge).
func (r *Runtime) PendingCount(owner Owner) int {
	return r.reg.PendingCount(owner.SessionID)
}

// Interrupt cancels a running child's current dispatch (an `interrupted`
// handback is produced); any queued mail then starts the next dispatch.
func (r *Runtime) Interrupt(_ context.Context, owner Owner, idOrName string) error {
	if err := r.gate(owner); err != nil {
		return err
	}

	st, err := r.reg.Lookup(owner.SessionID, idOrName)
	if err != nil {
		return err
	}

	r.mu.Lock()
	h := r.disp[st.ID]
	r.mu.Unlock()

	if h == nil {
		return fmt.Errorf("%w: %q", ErrNotRunning, st.Name)
	}

	h.cancel()

	return nil
}

// PendingHandbacks returns a snapshot of the owner's pending set (the
// pre-turn delivery hook's input).
func (r *Runtime) PendingHandbacks(owner Owner) []Handback {
	list := r.reg.PendingList(owner.SessionID)
	out := make([]Handback, len(list))
	for i, hb := range list {
		out[i] = *hb
	}

	return out
}

// ConsumePending removes the named handback from the pending set — called
// by the delivery hook AFTER the handback_delivery flush succeeded (D6:
// a Turn that crashes before the flush did not consume).
func (r *Runtime) ConsumePending(owner Owner, handbackID string) {
	r.reg.RemovePending(owner.SessionID, handbackID)
}

// appendHandback is the arrival path (D6/D6a): the handback record is
// appended to the owner file through the detached bookkeeping append — the
// only durable queue — and enters the pending set. A handback arriving
// after the owner Session's closing record is discarded silently.
func (r *Runtime) appendHandback(ctx context.Context, owner Owner, hb Handback) error {
	if owner.History != nil {
		rec := persistence.HistoryMessage{
			Kind:       persistence.KindHandback,
			Content:    hb.Report,
			HandbackID: hb.HandbackID,
			DispatchID: hb.DispatchID,
			Status:     string(hb.Status),
			Severity:   hb.Severity,
			Timestamp:  hb.CreatedAt,
		}

		if err := owner.History.AppendBookkeeping(ctx, rec); err != nil {
			if owner.History.Closed() {
				return nil // the hierarchy is closing: silent discard (D6a)
			}

			return fmt.Errorf("failed to append the handback record: %w", err)
		}
	}

	r.reg.Pending(owner.SessionID, hb)
	r.wakeWaiters(owner.SessionID)
	r.notifyFleet(owner.SessionID)

	return nil
}

// NotifyPending runs the passive-policy notice (D6c): the cmd-side wiring
// decides per channel (Telegram fixed message once per idle period, ZMQ
// badge, nothing on stdin CLI).
func (r *Runtime) NotifyPending(ctx context.Context, owner Owner) {
	if r.deps.Notify != nil {
		r.deps.Notify(ctx, owner, r.reg.PendingCount(owner.SessionID))
	}
}

// EndOwner cascade-closes the owner's fleet (D5): cancel every in-flight
// dispatch, join their goroutines, then append the fixed owner-end result
// where a tool-call group is open and a closing record with the owner's
// reason to every child file. The pending set ends with the owner Session.
func (r *Runtime) EndOwner(ctx context.Context, owner Owner, reason string) {
	// Evaluated BEFORE the cascade: a Session with no Subagent history —
	// no live fleet, no pending handbacks, no subagents/ subtree — never
	// receives the fleet snapshot (wire-protocol spec), so a fleetless
	// Session end (every /new, daily reset) emits nothing while a real
	// cascade close still emits its one empty snapshot (pending dropped
	// with the owner, agents array empty).
	hadHistory := r.ownerHadSubagentHistory(owner)

	r.cancelAndJoin(owner)

	for _, view := range r.reg.Agents(owner.SessionID) {
		r.closeChildOnOwnerEnd(ctx, owner, view.SubagentID, reason)
	}

	r.reg.DropOwner(owner.SessionID)
	if hadHistory {
		r.notifyFleet(owner.SessionID)
	}

	r.log.Info().
		Str("owner_session", owner.SessionID).
		Str("reason", reason).
		Msg("owner session ended — subagent fleet cascade-closed")
}

// ownerHadSubagentHistory reports whether the owner carries Subagent
// history: the registry currently knows it (a live fleet or pending
// handbacks) or its Session folder has a subagents/ subtree — the durable
// trace of a fleet that already ended (restart, reconcile). The owner dir
// is resolved the same way the usage walk does: the parent of the owner
// transcript file.
func (r *Runtime) ownerHadSubagentHistory(owner Owner) bool {
	if len(r.reg.Agents(owner.SessionID)) > 0 || r.reg.PendingCount(owner.SessionID) > 0 {
		return true
	}

	if owner.History == nil {
		return false
	}

	p := owner.History.Path()
	if p == "" {
		return false
	}

	info, err := os.Stat(filepath.Join(filepath.Dir(p), "subagents"))

	return err == nil && info.IsDir()
}

// closeChildOnOwnerEnd appends the closing record (and the fixed
// interrupted results for an open group) to one child file.
func (r *Runtime) closeChildOnOwnerEnd(ctx context.Context, _ Owner, childID, reason string) {
	h := r.childHistory(childID)
	if h == nil || h.Closed() {
		return
	}

	// Join already happened: append the fixed owner-end result where a
	// tool-call group is open.
	if n := h.CloseOpenToolCallGroupWith(persistence.InterruptedOwnerEndToolResult); n > 0 {
		r.log.Warn().Str("child_session", childID).Int("interrupted_results", n).
			Msg("owner session ended with an open child tool-call group")
	}

	now := r.now()
	rec := persistence.HistoryMessage{
		Kind:      persistence.KindSessionClosed,
		EndedAt:   &now,
		Reason:    reason,
		Timestamp: now,
	}

	if err := h.AppendRecord(ctx, rec); err != nil {
		r.log.Warn().Err(err).Str("child_session", childID).Msg("failed to append the child closing record")
	}

	r.dropChild(childID)
}

// Shutdown interrupts every running dispatch daemon-wide and closes open
// groups with the fixed daemon-restart result. It is NOT a Session end: no
// closing records are appended and pending handbacks stay durable (D5/2.4).
// shutdownJoinGrace bounds the daemon-shutdown join: a dispatch goroutine
// wedged anywhere that ignores context cancellation must never hold the
// process hostage — after the grace the stragglers are abandoned (boot
// reconcile closes their open tool-call groups) and shutdown proceeds.
const shutdownJoinGrace = 5 * time.Second

// Shutdown interrupts every running dispatch and joins the dispatch
// goroutines, bounded by shutdownJoinGrace. Unlike the owner-end cascade
// (cancelAndJoin, join-then-append) no record is appended here, so an
// abandoned straggler is safe: the boot reconcile closes its open
// tool-call group and appends the fixed interrupted handback.
func (r *Runtime) Shutdown(_ context.Context) {
	var handles []*dispatchHandle

	for _, ownerID := range r.reg.OwnerIDs() {
		for _, view := range r.reg.Agents(ownerID) {
			r.mu.Lock()
			h := r.disp[view.SubagentID]
			r.mu.Unlock()

			if h != nil {
				handles = append(handles, h)
			}
		}
	}

	for _, h := range handles {
		h.cancel()
	}

	deadline := time.After(shutdownJoinGrace)
	for i, h := range handles {
		select {
		case <-h.done:
		case <-deadline:
			r.log.Warn().
				Int("stragglers", len(handles)-i).
				Dur("grace", shutdownJoinGrace).
				Msg("subagent shutdown join grace exceeded — abandoning the stragglers; boot reconcile closes their open tool-call groups")

			return
		}
	}

	r.mu.Lock()
	ids := make([]string, 0, len(r.hists))
	for id := range r.hists {
		ids = append(ids, id)
	}
	r.mu.Unlock()

	for _, id := range ids {
		if h := r.childHistory(id); h != nil && !h.Closed() {
			if n := h.CloseOpenToolCallGroupWith(persistence.InterruptedRestartToolResult); n > 0 {
				r.log.Warn().Str("child_session", id).Int("interrupted_results", n).
					Msg("daemon shutdown closed an open child tool-call group")
			}
		}
	}
}

// cancelAndJoin cancels every running dispatch of an owner's fleet and
// waits for their goroutines (join-then-append: the cascade appends to a
// child file only after its goroutine returned, D6a).
func (r *Runtime) cancelAndJoin(owner Owner) {
	var handles []*dispatchHandle

	for _, view := range r.reg.Agents(owner.SessionID) {
		r.mu.Lock()
		h := r.disp[view.SubagentID]
		r.mu.Unlock()

		if h != nil {
			handles = append(handles, h)
		}
	}

	for _, h := range handles {
		h.cancel()
	}

	for _, h := range handles {
		<-h.done
	}
}

// dropChild releases a child's history from memory.
func (r *Runtime) dropChild(childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.hists, childID)
}

// childHistory returns the in-memory history of a child (nil when none).
func (r *Runtime) childHistory(childID string) *persistence.ConversationHistory {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.hists[childID]
}

// newHandbackID mints a handback identifier.
func newHandbackID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Sprintf("handback-%d", time.Now().UnixNano())
	}

	return "handback-" + id.String()
}

// Deps exposes the runtime's resolved configuration (the delivery budget
// computation on the owner path).
func (r *Runtime) Deps() Deps { return r.deps }

// DisableChannelFleets cascade-closes every channel-owner hierarchy (the
// `subagents.enabled` true→false flip, D14): running jobs are untouched.
// Called by the reload path BEFORE it requests the write lock.
func (r *Runtime) DisableChannelFleets(ctx context.Context) {
	for _, ownerID := range r.reg.OwnerIDs() {
		if f := r.fleetOwner(ownerID); f != nil && f.Kind == OwnerChannel {
			r.EndOwner(ctx, *f, "feature_disabled")
		}
	}
}

// fleetOwner returns a copy of the fleet's Owner (registry-locked).
func (r *Runtime) fleetOwner(ownerSessionID string) *Owner {
	return r.reg.FleetOwner(ownerSessionID)
}

// SetChildCompaction (re)installs the child compaction parameters — wired
// per generation.
func (r *Runtime) SetChildCompaction(cc *ChildCompaction) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.deps.ChildCompaction = cc
}

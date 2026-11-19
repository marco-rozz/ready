// registry.go — the per-owner-Session fleet and the structural limits
// (openspec change subagent-runtime, tasks 1.2). The registry owns names,
// depth and concurrency accounting; execution (dispatches, handbacks)
// belongs to the Runtime. All limits are enforced fail-fast here: a spawn or
// reopen beyond a limit returns the fixed structural error — no queue, no
// retry. A dispatch started from queued mail continues on its existing slot.
package subagent

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Limits are the registry's structural caps (from config.subagents).
type Limits struct {
	// MaxDepth is the nesting cap, 1 (default) to 2.
	MaxDepth int
	// MaxConcurrent is the cap on running dispatches per owner Session,
	// grandchildren included.
	MaxConcurrent int
	// MaxConcurrentJobs caps RunJob concurrency daemon-wide.
	MaxConcurrentJobs int
	// MailboxBuffer is each child's bounded FIFO capacity.
	MailboxBuffer int
}

// Defaults of the Limits fields when zero (mirrors config applySubagentsDefaults).
const (
	DefaultMaxDepth          = 1
	DefaultMaxConcurrent     = 4
	DefaultMaxConcurrentJobs = 1
	DefaultMailboxBuffer     = 32
)

// AgentView is one row of a fleet snapshot (the wire `subagents` event,
// design D11).
type AgentView struct {
	SubagentID string    `json:"subagent_id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	Activity   string    `json:"activity,omitempty"` // internal/activity label
	QueuedMail int       `json:"queued_mail"`
	Parent     string    `json:"parent,omitempty"` // depth-2 only
}

// Agent is the registry record of one Subagent. It carries no
// execution state: the Runtime adds goroutine handles and cancel funcs on
// top.
type Agent struct {
	ID        string // the Subagent's ID = its child Session ID (minted at spawn)
	Name      string
	Type      string
	Depth     int    // 1 for an owner's child, 2 for a grandchild
	Parent    string // the parent child's Name at depth 2 ("" at depth 1)
	Status    Status
	CreatedAt time.Time
	Dispatch  int // dispatch sequence within the child Session (1-based)
	HoldsSlot bool
	Mail      *mailbox
	// Snapshot is the spawn-time resolved definition: a live child never
	// re-reads the definition file (design D3).
	Snapshot Definition
}

// fleet is one owner Session's Subagent set.
type fleet struct {
	owner    Owner
	byName   map[string]*Agent
	byID     map[string]*Agent
	nextAuto map[string]int // per-type auto-name counter
	running  int            // children currently holding a concurrency slot
	pending  []*Handback    // the owner's pending handbacks, FIFO arrival order
}

// Registry owns every owner Session's fleet and the structural limits.
type Registry struct {
	limits Limits
	logger zerolog.Logger

	mu     sync.Mutex
	fleets map[string]*fleet // keyed by owner Session ID
	jobs   int               // running jobs (daemon-wide)
}

// NewRegistry builds the registry.
func NewRegistry(limits Limits, logger zerolog.Logger) *Registry {
	l := limits
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	if l.MaxConcurrent <= 0 {
		l.MaxConcurrent = DefaultMaxConcurrent
	}
	if l.MaxConcurrentJobs <= 0 {
		l.MaxConcurrentJobs = DefaultMaxConcurrentJobs
	}
	if l.MailboxBuffer <= 0 {
		l.MailboxBuffer = DefaultMailboxBuffer
	}

	return &Registry{
		limits: l,
		logger: logger.With().Str("component", "subagent-registry").Logger(),
		fleets: map[string]*fleet{},
	}
}

// Limits returns the effective limits.
func (r *Registry) Limits() Limits { return r.limits }

// fleetFor returns (creating) the owner's fleet.
func (r *Registry) fleetFor(owner Owner) *fleet {
	f, ok := r.fleets[owner.SessionID]
	if !ok {
		f = &fleet{
			owner:    owner,
			byName:   map[string]*Agent{},
			byID:     map[string]*Agent{},
			nextAuto: map[string]int{},
		}
		r.fleets[owner.SessionID] = f
	}

	return f
}

// SpawnOpts carries a spawn's registry inputs.
type SpawnOpts struct {
	Type       string // definition name
	Name       string // optional; auto <type>-<n> when empty
	Depth      int    // the new child's depth (1, or 2 under a depth-1 child)
	ParentName string // the parent child's name at depth 2
}

// Spawn registers a new Subagent under owner. Fail-fast structural checks
// (depth cap, concurrency cap, name rules) run here; the caller executes the
// dispatch afterwards. The new child holds a concurrency slot from birth.
func (r *Registry) Spawn(owner Owner, opts SpawnOpts) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fleetFor(owner)

	if opts.Depth > r.limits.MaxDepth {
		return nil, depthLimitError(r.limits.MaxDepth)
	}

	if f.running >= r.limits.MaxConcurrent {
		return nil, concurrencyLimitError(r.limits.MaxConcurrent)
	}

	name := opts.Name
	if name == "" {
		name = r.autoNameLocked(f, opts.Type)
	}

	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name %q must match [a-z0-9-]", ErrNameConflict, name)
	}

	if _, exists := f.byName[name]; exists {
		return nil, nameConflictError(name)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("failed to mint subagent id: %w", err)
	}

	st := &Agent{
		ID:        id.String(),
		Name:      name,
		Type:      opts.Type,
		Depth:     opts.Depth,
		Parent:    opts.ParentName,
		Status:    StatusRunning,
		CreatedAt: time.Now(),
		Dispatch:  0,
		HoldsSlot: true,
		Mail:      newMailbox(r.limits.MailboxBuffer),
	}

	f.byName[name] = st
	f.byID[st.ID] = st
	f.running++

	r.logger.Info().
		Str("owner_session", owner.SessionID).
		Str("subagent_id", st.ID).
		Str("name", name).
		Str("type", opts.Type).
		Int("depth", st.Depth).
		Msg("subagent spawned")

	return st, nil
}

// autoNameLocked returns `<type>-<n>` with the smallest unused n for the
// type in this fleet.
func (r *Registry) autoNameLocked(f *fleet, typ string) string {
	for n := f.nextAuto[typ] + 1; ; n++ {
		name := fmt.Sprintf("%s-%d", typ, n)
		if _, taken := f.byName[name]; !taken {
			f.nextAuto[typ] = n

			return name
		}
	}
}

// Lookup resolves idOrName (exact ID first, then name) in the owner's
// fleet.
func (r *Registry) Lookup(ownerSessionID, idOrName string) (*Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		return nil, ErrUnknownAgent
	}

	if st, ok := f.byID[idOrName]; ok {
		return st, nil
	}

	if st, ok := f.byName[idOrName]; ok {
		return st, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnknownAgent, idOrName)
}

// Reopen acquires a concurrency slot for a new dispatch on an idle child
// (a send to a non-running child, design D4). A child holding a slot
// (running or with queued mail) needs none. Fail-fast on the cap.
func (r *Registry) Reopen(owner Owner, st *Agent) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if st.HoldsSlot {
		return nil
	}

	f, ok := r.fleets[owner.SessionID]
	if !ok {
		return ErrUnknownAgent
	}

	if f.running >= r.limits.MaxConcurrent {
		return concurrencyLimitError(r.limits.MaxConcurrent)
	}

	f.running++
	st.HoldsSlot = true

	return nil
}

// ReleaseSlot releases a child's concurrency slot once it settles idle
// (no running dispatch, empty mailbox). Must be called exactly once per
// acquired slot.
func (r *Registry) ReleaseSlot(ownerSessionID string, st *Agent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !st.HoldsSlot {
		return
	}

	st.HoldsSlot = false

	if f, ok := r.fleets[ownerSessionID]; ok && f.running > 0 {
		f.running--
	}
}

// QueueMail enqueues owner mail on a running child's bounded mailbox.
// Full ⇒ the fixed backpressure error, nothing enqueued.
func (r *Registry) QueueMail(st *Agent, m Mail) error {
	if err := st.Mail.Push(m); err != nil {
		return mailboxFullError(st.Name, cap(st.Mail.items))
	}

	return nil
}

// SetStatus updates a child's status.
func (r *Registry) SetStatus(st *Agent, s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st.Status = s
}

// NextDispatch returns the child's next dispatch sequence (1-based).
func (r *Registry) NextDispatch(st *Agent) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	st.Dispatch++

	return st.Dispatch
}

// Pending appends a handback to the owner's pending set (arrival order);
// the fleet is created lazily (an owner with handbacks but no live spawn —
// the reconciliation path).
func (r *Registry) Pending(ownerSessionID string, hb Handback) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		f = &fleet{
			owner:    Owner{SessionID: ownerSessionID},
			byName:   map[string]*Agent{},
			byID:     map[string]*Agent{},
			nextAuto: map[string]int{},
		}
		r.fleets[ownerSessionID] = f
	}

	f.pending = append(f.pending, &hb)
}

// PendingList returns a snapshot of the owner's pending handbacks.
func (r *Registry) PendingList(ownerSessionID string) []*Handback {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		return nil
	}

	out := make([]*Handback, len(f.pending))
	copy(out, f.pending)

	return out
}

// RemovePending removes the handback with the given ID (consumed by wait or
// delivery). Reports whether it was found.
func (r *Registry) RemovePending(ownerSessionID, handbackID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		return false
	}

	for i, hb := range f.pending {
		if hb.HandbackID == handbackID {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)

			return true
		}
	}

	return false
}

// PendingCount returns the owner's pending-handback count.
func (r *Registry) PendingCount(ownerSessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if f, ok := r.fleets[ownerSessionID]; ok {
		return len(f.pending)
	}

	return 0
}

// DropOwner ends the fleet: every child's state is removed (the owner
// Session ended — the cascade close already joined goroutines) and the
// pending set is dropped with it.
func (r *Registry) DropOwner(ownerSessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.fleets, ownerSessionID)
}

// OwnerIDs lists every owner Session ID with a fleet (the shutdown path's
// iteration order is unspecified).
func (r *Registry) OwnerIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.fleets))
	for id := range r.fleets {
		out = append(out, id)
	}

	return out
}

// RunningCount returns the owner's running-dispatch count.
func (r *Registry) RunningCount(ownerSessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if f, ok := r.fleets[ownerSessionID]; ok {
		return f.running
	}

	return 0
}

// Agents returns a snapshot of the owner's fleet (fleet order: creation).
func (r *Registry) Agents(ownerSessionID string) []AgentView {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		return nil
	}

	ids := make([]string, 0, len(f.byID))
	for id := range f.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids) // UUIDv7 strings sort in creation order

	out := make([]AgentView, 0, len(ids))
	for _, id := range ids {
		st := f.byID[id]
		out = append(out, AgentView{
			SubagentID: st.ID,
			Name:       st.Name,
			Type:       st.Type,
			Status:     st.Status,
			StartedAt:  st.CreatedAt,
			QueuedMail: st.Mail.Len(),
			Parent:     st.Parent,
		})
	}

	return out
}

// BeginJob / EndJob account a running job against the daemon-wide
// max_concurrent_jobs cap. BeginJob fails fast beyond the cap.
func (r *Registry) BeginJob() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.jobs >= r.limits.MaxConcurrentJobs {
		return jobsCapError(r.limits.MaxConcurrentJobs)
	}

	r.jobs++

	return nil
}

// EndJob releases a job's slot.
func (r *Registry) EndJob() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.jobs > 0 {
		r.jobs--
	}
}

// FleetOwner returns a copy of the owner descriptor of one fleet (nil when
// absent) — the disable-flip cascade needs the Owner kinds.
func (r *Registry) FleetOwner(ownerSessionID string) *Owner {
	r.mu.Lock()
	defer r.mu.Unlock()

	if f, ok := r.fleets[ownerSessionID]; ok {
		o := f.owner

		return &o
	}

	return nil
}

// DropChild removes one child's registry record (a scoped grandchild is
// never reopened, D8).
func (r *Registry) DropChild(ownerSessionID, childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	f, ok := r.fleets[ownerSessionID]
	if !ok {
		return
	}

	if st, ok := f.byID[childID]; ok {
		delete(f.byName, st.Name)
		delete(f.byID, childID)

		if st.HoldsSlot && f.running > 0 {
			f.running--
		}
	}
}

// slots.go — per-provider parallelism (openspec change subagent-runtime,
// tasks 3.3/3.3b, design D18): every main-agent attempt (owner or child),
// aux call and probe acquires a slot on its candidate's provider instance
// before the attempt. Queue wait does not count toward the liveness budget
// (armed after acquisition) but does count toward the outer deadline. Slots
// are granted in priority order — owner attempts (0), then channel-owner
// Subagent attempts (1), then job attempts (2) — so maintenance never
// delays a user, and a local model serialising concurrent children is never
// falsely demoted for liveness.
package orchestrator

import (
	"context"
	"sync"
)

// Priority classes of slot acquisition (lower = granted first).
const (
	// PriorityOwner is the owner Turn and its aux consumers.
	PriorityOwner = 0
	// PrioritySubagent is a channel-owner Subagent dispatch.
	PrioritySubagent = 1
	// PriorityJob is an internal job (lowest).
	PriorityJob = 2
)

// waiter is one queued acquisition.
type waiter struct {
	priority int
	seq      uint64
	granted  chan struct{}
}

// providerSlots is one provider instance's semaphore.
type providerSlots struct {
	limit int // 0 = unlimited
	inUse int
	seq   uint64 // grants sequence, FIFO tie-break within a priority
	// waiters sorted by (priority, seq); granted on release.
	waiters []*waiter
}

// SlotLimiter is the daemon-wide slot manager, keyed by provider instance
// name. A nil SlotLimiter (or a provider with limit 0) means unlimited.
type SlotLimiter struct {
	mu        sync.Mutex
	providers map[string]*providerSlots
}

// NewSlotLimiter builds the limiter; limits maps a provider instance name to
// its max_parallel (0 or missing = unlimited).
func NewSlotLimiter(limits map[string]int) *SlotLimiter {
	m := make(map[string]*providerSlots, len(limits))
	for name, limit := range limits {
		if limit > 0 {
			m[name] = &providerSlots{limit: limit}
		}
	}

	return &SlotLimiter{providers: m}
}

// releaseFunc frees the slot (a no-op for an unlimited provider or an
// immediate grant that found no queued waiter to hand the slot to — the
// handoff model: a release always either transfers the slot to the
// highest-priority waiter or returns it to the pool).
type releaseFunc func()

// acquire grants one slot on provider, blocking ( honouring ctx ) until a
// slot is available. The queue wait is NOT part of any attempt budget: the
// caller arms its liveness budget after acquire returns.
func (l *SlotLimiter) acquire(ctx context.Context, provider string, priority int) (releaseFunc, error) {
	if l == nil {
		return func() {}, nil
	}

	l.mu.Lock()
	ps, ok := l.providers[provider]
	if !ok || ps.limit <= 0 {
		l.mu.Unlock()

		return func() {}, nil
	}

	// Immediate grant when a slot is free and no queued waiter outranks the
	// caller (priority order: waiters are granted before a fresh acquirer of
	// lower priority; an equal-or-higher-priority newcomer may jump ahead of
	// queued lower-priority waiters only if it is itself the best candidate —
	// the release path grants to the head, so a fresh acquirer never passes a
	// queued waiter of strictly better priority).
	if ps.inUse < ps.limit && !ps.hasBetterWaiter(priority) {
		ps.inUse++
		l.mu.Unlock()

		return func() { l.release(provider, ps) }, nil
	}

	ps.seq++
	w := &waiter{priority: priority, seq: ps.seq, granted: make(chan struct{})}
	ps.waiters = append(ps.waiters, w)
	l.mu.Unlock()

	select {
	case <-w.granted:
		return func() { l.release(provider, ps) }, nil
	case <-ctx.Done():
		// Remove the waiter; if it was granted concurrently, pass the slot on.
		l.mu.Lock()
		for i, q := range ps.waiters {
			if q == w {
				ps.waiters = append(ps.waiters[:i], ps.waiters[i+1:]...)

				break
			}
		}

		select {
		case <-w.granted:
			l.mu.Unlock()
			l.release(provider, ps)

			return nil, ctx.Err()
		default:
		}
		l.mu.Unlock()

		return nil, ctx.Err()
	}
}

// hasBetterWaiter reports whether a queued waiter strictly outranks priority.
func (ps *providerSlots) hasBetterWaiter(priority int) bool {
	for _, w := range ps.waiters {
		if w.priority < priority {
			return true
		}
	}

	return false
}

// release returns one slot: granted to the best queued waiter (lowest
// priority, then FIFO) or returned to the pool.
func (l *SlotLimiter) release(_ string, ps *providerSlots) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(ps.waiters) > 0 {
		best := 0
		for i, w := range ps.waiters {
			if w.priority < ps.waiters[best].priority ||
				(w.priority == ps.waiters[best].priority && w.seq < ps.waiters[best].seq) {
				best = i
			}
		}

		w := ps.waiters[best]
		ps.waiters = append(ps.waiters[:best], ps.waiters[best+1:]...)
		close(w.granted) // the slot transfers: inUse unchanged

		return
	}

	if ps.inUse > 0 {
		ps.inUse--
	}
}

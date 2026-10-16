package session

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/rs/zerolog"
)

// HistoryProvider hands out the ConversationHistory of a Session (restored
// from its file on first access) — the only writer of that file.
type HistoryProvider interface {
	For(ctx context.Context, s *Session) *persistence.ConversationHistory
	// Drop releases the in-memory history of an ended Session.
	Drop(sessionID string)
}

// EndHook runs after a Session ended (closing record written): it releases
// Session-scoped state (agentskills session, usage totals, compaction state)
// and may trigger retention.
type EndHook func(ctx context.Context, ended *Session)

// Manager holds the address → current Session map and executes Session
// transitions. Single-writer invariant: End and the application
// of a pending reset run only in the address's message worker; external
// triggers (a Connection start) only record an intent via RequestReset.
type Manager struct {
	store      *Store
	hist       HistoryProvider
	finalUsage func(sessionID string) persistence.UsageRecord
	logger     zerolog.Logger

	mu    sync.Mutex
	addrs map[Address]*addrState
	hooks []EndHook
}

// addrState is the per-address lifecycle state.
type addrState struct {
	mu      sync.Mutex
	current *Session
	pending *pendingReset
}

// pendingReset is a reset decided at a Connection start, with its new
// Session pre-assigned, applied by the worker before its next item. It is
// in-memory only (lost on restart — accepted).
type pendingReset struct {
	next   *Session
	reason string
}

// NewManager wires the lifecycle. finalUsage returns a Session's cumulative
// usage totals for its closing record.
func NewManager(store *Store, hist HistoryProvider, finalUsage func(sessionID string) persistence.UsageRecord, logger zerolog.Logger) *Manager {
	return &Manager{
		store:      store,
		hist:       hist,
		finalUsage: finalUsage,
		logger:     logger,
		addrs:      make(map[Address]*addrState),
	}
}

// Store returns the underlying file store.
func (m *Manager) Store() *Store { return m.store }

// OnEnd registers a hook run after every Session end.
func (m *Manager) OnEnd(h EndHook) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.hooks = append(m.hooks, h)
}

// lockedState returns addr's state with its mutex held and the current
// Session resolved (from disk on first use; a fresh Session when the
// address has none). The caller must unlock a.mu.
func (m *Manager) lockedState(ctx context.Context, addr Address) *addrState {
	m.mu.Lock()
	a, ok := m.addrs[addr]
	if !ok {
		a = &addrState{}
		m.addrs[addr] = a
	}
	m.mu.Unlock()

	a.mu.Lock()

	if a.current == nil {
		cur, err := m.store.resolve(ctx, addr)
		if err != nil {
			m.logger.Warn().Err(err).Str("address", string(addr)).Msg("failed to resolve the current session — starting a new one")
		}

		if cur == nil {
			cur = m.mustNewSession(addr, time.Now())
		}

		a.current = cur
	}

	return a
}

// mustNewSession mints a Session; a UUIDv7 failure (entropy exhaustion) is
// not recoverable here, so it falls back to a time-derived id via uuid's
// own fallback path by retrying once.
func (m *Manager) mustNewSession(addr Address, now time.Time) *Session {
	s, err := m.store.newSession(addr, now)
	if err != nil {
		m.logger.Error().Err(err).Str("address", string(addr)).Msg("failed to mint a session id — retrying")

		s, err = m.store.newSession(addr, now)
		if err != nil {
			panic(fmt.Sprintf("session: cannot mint a UUIDv7: %v", err))
		}
	}

	return s
}

// Current returns the address's current Session (never a pending one).
func (m *Manager) Current(ctx context.Context, addr Address) *Session {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	return a.current
}

// EffectiveID returns the Session ID a client should see: the pending
// Session's when a reset is pending, the current one's otherwise.
func (m *Manager) EffectiveID(ctx context.Context, addr Address) (id string, pending bool) {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	if a.pending != nil {
		return a.pending.next.ID, true
	}

	return a.current.ID, false
}

// History returns the address's current Session and its history.
func (m *Manager) History(ctx context.Context, addr Address) (*Session, *persistence.ConversationHistory) {
	cur := m.Current(ctx, addr)

	return cur, m.hist.For(ctx, cur)
}

// RequestReset evaluates rule at a Connection start without waiting for the
// address's worker (the ZMQ receive loop also serves pings and session_end):
// when a reset is already pending it returns the pending Session ID
// (idempotent); when the rule says a reset is due it records a pending reset
// with a pre-assigned UUIDv7 and returns it; otherwise it returns the
// current Session ID. pending reports whether a reset awaits application.
func (m *Manager) RequestReset(ctx context.Context, addr Address, rule Rule, now time.Time) (effectiveID string, pending bool) {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	if a.pending != nil {
		return a.pending.next.ID, true
	}

	reason, due := Due(rule, m.hist.For(ctx, a.current).LastActivity(), now)
	if !due {
		return a.current.ID, false
	}

	a.pending = &pendingReset{next: m.mustNewSession(addr, now), reason: reason}

	m.logger.Info().
		Str("address", string(addr)).
		Str("session_id", a.current.ID).
		Str("next_session_id", a.pending.next.ID).
		Str("reason", reason).
		Msg("session reset pending — applied before the next item")

	return a.pending.next.ID, true
}

// ApplyPending applies a pending reset (worker only): the current Session is
// ended with the recorded reason and the pre-assigned Session becomes
// current. Returns the ended Session, nil when nothing was pending.
func (m *Manager) ApplyPending(ctx context.Context, addr Address) *Session {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	if a.pending == nil {
		return nil
	}

	p := a.pending
	a.pending = nil

	return m.endLocked(ctx, a, p.reason, p.next)
}

// EvaluateReset evaluates rule before a Turn on a message-oriented channel
// (worker only) and ends the Session when due. Returns whether it reset.
func (m *Manager) EvaluateReset(ctx context.Context, addr Address, rule Rule, now time.Time) bool {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	reason, due := Due(rule, m.hist.For(ctx, a.current).LastActivity(), now)
	if !due {
		return false
	}

	m.endLocked(ctx, a, reason, m.mustNewSession(addr, now))

	return true
}

// StartNew ends the current Session on the user's request (/new, worker
// only). A Session with no recorded Turn changes nothing: started is false.
func (m *Manager) StartNew(ctx context.Context, addr Address) (next *Session, started bool) {
	a := m.lockedState(ctx, addr)
	defer a.mu.Unlock()

	if !m.hist.For(ctx, a.current).HasTurn() {
		return a.current, false
	}

	m.endLocked(ctx, a, persistence.ReasonCommand, m.mustNewSession(addr, time.Now()))

	return a.current, true
}

// endLocked appends the closing record (reason + final usage totals) to the
// current Session when it recorded anything, runs the end hooks, and makes
// next current. Must hold a.mu. Returns the ended Session.
func (m *Manager) endLocked(ctx context.Context, a *addrState, reason string, next *Session) *Session {
	ended := a.current
	h := m.hist.For(ctx, ended)

	if h.HasTurn() && !h.Closed() {
		now := time.Now()
		totals := m.finalUsage(ended.ID)

		rec := persistence.HistoryMessage{
			Kind:      persistence.KindSessionClosed,
			EndedAt:   &now,
			Reason:    reason,
			Usage:     &totals,
			Timestamp: now,
		}

		if err := h.AppendRecord(ctx, rec); err != nil {
			m.logger.Error().Err(err).Str("session_id", ended.ID).Msg("failed to append the closing record")
		}
	}

	m.logger.Info().
		Str("address", string(ended.Address)).
		Str("session_id", ended.ID).
		Str("next_session_id", next.ID).
		Str("reason", reason).
		Msg("session ended")

	a.current = next

	m.mu.Lock()
	hooks := append([]EndHook(nil), m.hooks...)
	m.mu.Unlock()

	for _, hook := range hooks {
		hook(ctx, ended)
	}

	m.hist.Drop(ended.ID)

	return ended
}

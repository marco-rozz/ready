package hatwin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Evidence collector (twin-native-actuation D3, task 2.2).
//
// A collector entry is armed BEFORE the command is sent, with the pre-command
// snapshots of the targets. It buffers state_changed events of the targets
// until the call_service result reveals HA's context id, then attributes
// events: an own-context event reaching the expected state is `applied`
// (direct); a foreign-context event reaching the expected state (and not
// unavailable/unknown) is `applied` with indirect evidence (integration
// re-poll fallback); an own-context transitional event is `pending`. Events
// are offered AFTER handleStateChanged updated the mirror, so a read right
// after a result is consistent with the outcomes.

// Outcome is the per-entity result of a command.
type Outcome string

const (
	OutcomeApplied     Outcome = "applied"
	OutcomePending     Outcome = "pending"
	OutcomeAlready     Outcome = "already"
	OutcomeUnchanged   Outcome = "unchanged"
	OutcomeUnavailable Outcome = "unavailable"
	OutcomeError       Outcome = "error"
)

// Evidence retention: entries live in the store for evidenceTTL (verify mode
// window) with at most evidenceMaxEntries entries (oldest evicted).
const (
	evidenceTTL        = 10 * time.Minute
	evidenceMaxEntries = 64
)

// EntityOutcome is the LLM-facing per-entity outcome.
type EntityOutcome struct {
	EntityID string  `json:"entity_id"`
	Outcome  Outcome `json:"outcome"`
	Before   string  `json:"before,omitempty"`
	After    string  `json:"after,omitempty"`
	// Evidence is "direct" (own HA context) or "indirect" (foreign context
	// reaching the expected state — integration re-poll fallback).
	Evidence string `json:"evidence,omitempty"`
	Note     string `json:"note,omitempty"`
}

// CallRecord is one HA call_service under a logical call_id.
type CallRecord struct {
	Domain    string   `json:"domain"`
	Service   string   `json:"service"`
	EntityIDs []string `json:"entity_ids"`
	ContextID string   `json:"context_id,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// CommandReport is the outcome of one logical command.
type CommandReport struct {
	CallID   string          `json:"call_id"`
	Sent     bool            `json:"sent"`
	Calls    []CallRecord    `json:"calls,omitempty"`
	Outcomes []EntityOutcome `json:"outcomes"`
}

// targetTrack is the per-entity evidence state.
type targetTrack struct {
	before       string
	expect       stateExpectation // nil = no expected state
	outcome      Outcome
	after        string
	evidence     string
	note         string
	settled      bool
	seenOwnEvent bool
}

// evidenceEntry is one logical command's collector.
type evidenceEntry struct {
	callID   string
	armedAt  time.Time
	mu       sync.Mutex
	targets  map[string]*targetTrack
	order    []string
	calls    []CallRecord
	ctxKnown bool
	ctxIDs   map[string]bool
	buffered map[string][]map[string]any
	done     chan struct{}
	closeOne sync.Once
}

func (e *evidenceEntry) allSettled() bool {
	for _, tr := range e.targets {
		if !tr.settled {
			return false
		}
	}

	return true
}

// newCallID returns a fresh logical call id.
func newCallID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cmd_%d", time.Now().UnixNano())
	}

	return "cmd_" + hex.EncodeToString(b[:])
}

// armEvidence creates and registers a collector entry for the target set.
// Must be called before any send (pre-command snapshots).
func (t *Twin) armEvidence(targets []string, expects map[string]stateExpectation, preStates map[string]string) *evidenceEntry {
	e := &evidenceEntry{
		callID:   newCallID(),
		armedAt:  time.Now(),
		targets:  make(map[string]*targetTrack, len(targets)),
		ctxIDs:   make(map[string]bool),
		buffered: make(map[string][]map[string]any),
		done:     make(chan struct{}),
	}
	for _, id := range targets {
		e.targets[id] = &targetTrack{
			before:  preStates[id],
			expect:  expects[id],
			outcome: OutcomePending,
		}
		e.order = append(e.order, id)
	}

	t.cmdMu.Lock()
	t.activeEvidence = append(t.activeEvidence, e)
	t.evidenceStore[e.callID] = e
	t.evidenceOrder = append(t.evidenceOrder, e.callID)
	t.evictEvidenceLocked()
	t.cmdMu.Unlock()

	return e
}

// evictEvidenceLocked drops expired entries and trims the store to
// evidenceMaxEntries (oldest first). Caller holds cmdMu.
func (t *Twin) evictEvidenceLocked() {
	now := time.Now()
	kept := t.evidenceOrder[:0]
	for _, id := range t.evidenceOrder {
		e, ok := t.evidenceStore[id]
		if !ok {
			continue
		}
		if now.Sub(e.armedAt) > evidenceTTL {
			delete(t.evidenceStore, id)

			continue
		}
		kept = append(kept, id)
	}
	t.evidenceOrder = kept

	for len(t.evidenceOrder) > evidenceMaxEntries {
		oldest := t.evidenceOrder[0]
		t.evidenceOrder = t.evidenceOrder[1:]
		delete(t.evidenceStore, oldest)
	}
}

// observeResult records one HA call's outcome in the entry and, on success,
// reveals the context id and drains the buffered target events.
func (t *Twin) observeResult(e *evidenceEntry, rec CallRecord, res CommandResult) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if res.Success {
		rec.ContextID = res.ContextID
		e.ctxIDs[res.ContextID] = true
		e.ctxKnown = true
	} else {
		rec.Error = haErrorText(res)
		// A failed HA call settles its entities as `error` immediately.
		for _, id := range rec.EntityIDs {
			if tr := e.targets[id]; tr != nil && !tr.settled {
				tr.outcome = OutcomeError
				tr.settled = true
				tr.note = rec.Error
				tr.after = tr.before
			}
		}
	}
	e.calls = append(e.calls, rec)
	e.checkSettledLocked()

	if !e.ctxKnown {
		return
	}

	// Drain buffered events in arrival order, now that the context is known.
	for entityID, events := range e.buffered {
		for _, st := range events {
			e.classifyLocked(entityID, st)
		}
		delete(e.buffered, entityID)
	}
}

// markCallFailed records a transport failure for one HA call: every entity of
// that call settles as `error`.
func (t *Twin) markCallFailed(e *evidenceEntry, rec CallRecord, cause string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec.Error = cause
	e.calls = append(e.calls, rec)
	for _, id := range rec.EntityIDs {
		if tr := e.targets[id]; tr != nil && !tr.settled {
			tr.outcome = OutcomeError
			tr.settled = true
			tr.note = cause
		}
	}
	e.checkSettledLocked()
}

// haErrorText renders an HA error object as a short string.
func haErrorText(res CommandResult) string {
	if code := pickStr(res.Error, "code"); code != "" {
		return fmt.Sprintf("HA error %s: %s", code, pickStr(res.Error, "message"))
	}

	return fmt.Sprintf("HA error: %v", res.Error)
}

// offerStateChanged feeds a state_changed event to every active collector
// containing the entity. Called from handleStateChanged AFTER the mirror was
// updated (D3, D-no-optimistic-echo: the collector observes, never mutates).
func (t *Twin) offerStateChanged(entityID string, newState map[string]any) {
	t.cmdMu.Lock()
	active := append([]*evidenceEntry(nil), t.activeEvidence...)
	t.cmdMu.Unlock()

	for _, e := range active {
		e.mu.Lock()
		if _, targeted := e.targets[entityID]; !targeted {
			e.mu.Unlock()

			continue
		}
		if !e.ctxKnown {
			e.buffered[entityID] = append(e.buffered[entityID], newState)
			e.mu.Unlock()

			continue
		}
		e.classifyLocked(entityID, newState)
		e.mu.Unlock()
	}
}

// classifyLocked attributes one event to the entry. Own-context events carry
// the expected-state logic; foreign-context events only count through the
// expected-state indirect fallback. Caller holds e.mu.
func (e *evidenceEntry) classifyLocked(entityID string, newState map[string]any) {
	tr := e.targets[entityID]
	if tr == nil || tr.settled {
		return
	}

	state := pickStr(newState, "state")
	ctxID := pickStr(asMap(newState["context"]), "id")
	own := ctxID != "" && e.ctxIDs[ctxID]

	tr.after = state

	if own {
		tr.seenOwnEvent = true
		if tr.expect != nil && tr.expect(state) {
			tr.outcome = OutcomeApplied
			tr.evidence = "direct"
			tr.settled = true
			e.checkSettledLocked()

			return
		}
		if isTransitional(state) {
			tr.outcome = OutcomePending

			return
		}
		// Unexpected own-context state (including unavailable/unknown):
		// keep listening; the final outcome is decided at window end.
		tr.outcome = OutcomePending

		return
	}

	// Foreign context: only the expected-state fallback counts, and only for
	// a state that is a real value (never unavailable/unknown).
	if tr.expect != nil && tr.expect(state) && !unavailableLikeStates[strings.ToLower(state)] {
		tr.outcome = OutcomeApplied
		tr.evidence = "indirect"
		tr.settled = true
		e.checkSettledLocked()
	}
}

// checkSettledLocked closes done once every target settled. Caller holds e.mu.
func (e *evidenceEntry) checkSettledLocked() {
	if !e.allSettled() {
		return
	}
	e.closeOne.Do(func() { close(e.done) })
}

// waitForEvidence blocks until all targets settled, the window expires, or
// ctx is done. It reports whether the window completed (false on cancellation
// — the caller must then NOT finalize, leaving the entry verifiable).
func (t *Twin) waitForEvidence(ctx context.Context, e *evidenceEntry, window time.Duration) (completed bool) {
	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-e.done:
		return true
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// finalizeEvidence settles every remaining target (already/unchanged) and
// detaches the entry from the active list.
func (t *Twin) finalizeEvidence(e *evidenceEntry) {
	e.mu.Lock()
	for _, id := range e.order {
		tr := e.targets[id]
		if tr.settled {
			continue
		}
		switch {
		case tr.seenOwnEvent && isTransitional(tr.after):
			// Mid-flight at window end (e.g. a slow cover still
			// opening): honestly pending, not unchanged.
			tr.outcome = OutcomePending
			tr.note = "transitional at window end"
		case !tr.seenOwnEvent && tr.expect != nil && tr.expect(tr.before):
			tr.outcome = OutcomeAlready
		default:
			tr.outcome = OutcomeUnchanged
		}
		tr.settled = true
	}
	e.closeOne.Do(func() { close(e.done) })
	e.mu.Unlock()

	t.cmdMu.Lock()
	for i, a := range t.activeEvidence {
		if a == e {
			t.activeEvidence = append(t.activeEvidence[:i], t.activeEvidence[i+1:]...)

			break
		}
	}
	t.cmdMu.Unlock()
}

// report renders the entry's current outcomes. Settled entities carry their
// final outcome; unsettled ones report `pending` (verification not complete).
func (e *evidenceEntry) report() CommandReport {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := CommandReport{
		CallID: e.callID,
		Sent:   len(e.calls) > 0,
		Calls:  append([]CallRecord(nil), e.calls...),
	}
	for _, id := range e.order {
		tr := e.targets[id]
		entity := EntityOutcome{
			EntityID: id,
			Before:   tr.before,
			After:    tr.after,
			Evidence: tr.evidence,
		}
		switch {
		case tr.settled:
			entity.Outcome = tr.outcome
		default:
			entity.Outcome = OutcomePending
			entity.Note = "verification not complete"
		}
		out.Outcomes = append(out.Outcomes, entity)
	}

	return out
}

// VerifyCommand returns the outcomes collected so far for a logical call_id
// (mode=verify). Unknown or expired ids fail with ErrUnknownCallID.
func (t *Twin) VerifyCommand(callID string) (CommandReport, error) {
	t.cmdMu.Lock()
	e, ok := t.evidenceStore[callID]
	if ok {
		if time.Since(e.armedAt) > evidenceTTL {
			delete(t.evidenceStore, callID)
			ok = false
		}
	}
	t.cmdMu.Unlock()

	if !ok {
		return CommandReport{}, fmt.Errorf("%w: %q; if the command was recent, check the current state with ha_state_query instead", ErrUnknownCallID, callID)
	}

	return e.report(), nil
}

//nolint:goconst
package hatwin

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// CommandSpec is one intent-style command request (twin-native-actuation
// D3–D6). The tool adapter (internal/twin) builds it; pkg/hatwin resolves,
// arms, sends, verifies.
type CommandSpec struct {
	Action string
	// Domain is mandatory for selectors; with explicit EntityIDs it is
	// derived per entity.
	Domain string
	// Area, Pattern, Name are selector fields (see TargetSpec).
	Area, Pattern, Name string
	// EntityIDs, when non-empty, selects explicit ids.
	EntityIDs []string
	// Params are the parametric-set parameters.
	Params map[string]any
	// MaxTargets caps the resolved target count (≤0 = no cap; the adapter
	// passes the configured default).
	MaxTargets int
	// VerifyWindow bounds the evidence wait; 0 = send mode (return after
	// the HA result, verify later via VerifyCommand).
	VerifyWindow time.Duration
}

// resolvedCall is one per-domain call_service ready to send.
type resolvedCall struct {
	domain  string
	service string
	data    map[string]any
	ids     []string
}

// ExecuteCommand serves one intent command end to end:
//
//  1. write barrier (D2) — live session, bootstrapped, not hard-stale;
//  2. target resolution (D6) — validation, de-dup, cap, availability;
//  3. intent resolution (D5) — per-domain service + validated params;
//  4. evidence arm (D3) — pre-command snapshots, BEFORE any send;
//  5. one call_service per domain group under one logical call_id;
//  6. evidence window (sync mode) — bounded wait, then finalize.
//
// After the first frame is written the report is always returned (call_id
// included), never a bare error — cancellation or deadline mid-verify yields
// the outcomes known so far, verifiable later via VerifyCommand.
func (t *Twin) ExecuteCommand(ctx context.Context, spec CommandSpec) (CommandReport, error) {
	// Write barrier (D2). CallService re-checks on the wire; checking here
	// first keeps resolution and the barrier one step, before arming.
	if err := t.writeBarrier(); err != nil {
		return CommandReport{}, err
	}

	// Early vocabulary check for an explicit selector domain: an inactive or
	// unknown domain is refused before any target lookup, so the guided text
	// never mentions a spurious no-match.
	if spec.Domain != "" && !t.commandDomainActive(spec.Domain) {
		return CommandReport{}, fmt.Errorf("%w: domain %q is not in the active command domains; see the tool description — use an equivalent tool from the catalog if one exists, otherwise report the action as unavailable", ErrOutOfVocabulary, spec.Domain)
	}

	// Target resolution (D6) and intent resolution (D5) happen before any
	// send: every refusal below sends nothing.
	targets, unavailable, rerr := t.resolveTargets(TargetSpec{
		Domain:    spec.Domain,
		Area:      spec.Area,
		Pattern:   spec.Pattern,
		Name:      spec.Name,
		EntityIDs: spec.EntityIDs,
	}, spec.MaxTargets)
	if rerr != nil {
		return CommandReport{}, rerr
	}

	if terr := t.checkToggle(spec, targets); terr != nil {
		return CommandReport{}, terr
	}

	calls, cerr := t.buildDomainCalls(spec, targets)
	if cerr != nil {
		return CommandReport{}, cerr
	}

	if len(calls) == 0 {
		// Every target was unavailable: report them, nothing sent.
		entry := t.armUnavailableEvidence(unavailable)

		return entry.report(), nil
	}

	entry, sendErr := t.armAndSend(ctx, spec, targets, unavailable, calls)
	if sendErr != nil {
		return CommandReport{}, sendErr
	}

	// Evidence window. Cancellation mid-verify is NOT an error: the report
	// carries call_id and the outcomes known so far (D4).
	if spec.VerifyWindow > 0 {
		if completed := t.waitForEvidence(ctx, entry, spec.VerifyWindow); completed {
			t.finalizeEvidence(entry)
		}
	} else {
		// Send mode: settle entities that already have all evidence, keep
		// the entry verifiable via VerifyCommand.
		entry.mu.Lock()
		entry.checkSettledLocked()
		entry.mu.Unlock()
	}

	return entry.report(), nil
}

// checkToggle enforces D6 rule 6: toggle applies to exactly one target.
func (t *Twin) checkToggle(spec CommandSpec, targets []string) error {
	if spec.Action != "toggle" || len(targets) <= 1 {
		return nil
	}

	return fmt.Errorf("%w: toggle resolved to %d targets; toggle applies to exactly one entity — use on/off for sets", ErrMultiTargetToggle, len(targets))
}

// buildDomainCalls splits the resolved targets per domain (D6 rule 8) and
// resolves each group's intent (D5). Refusals here send nothing. An empty
// result means every target was unavailable.
func (t *Twin) buildDomainCalls(spec CommandSpec, targets []string) ([]resolvedCall, error) {
	if len(targets) == 0 {
		return nil, nil
	}

	byDomain := make(map[string][]string)
	for _, id := range targets {
		d := spec.Domain
		if len(spec.EntityIDs) > 0 {
			d = domainOf(id)
		}
		byDomain[d] = append(byDomain[d], id)
	}

	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	calls := make([]resolvedCall, 0, len(domains))
	for _, d := range domains {
		if !t.commandDomainActive(d) {
			return nil, fmt.Errorf("%w: domain %q is not in the active command domains; see the tool description — use an equivalent tool from the catalog if one exists, otherwise report the action as unavailable", ErrOutOfVocabulary, d)
		}
		service, data, err := resolveIntent(d, spec.Action, spec.Params)
		if err != nil {
			return nil, err
		}
		calls = append(calls, resolvedCall{domain: d, service: service, data: data, ids: byDomain[d]})
	}

	return calls, nil
}

// armUnavailableEvidence registers an evidence entry that reports the given
// targets as `unavailable` (nothing sent).
func (t *Twin) armUnavailableEvidence(unavailable []string) *evidenceEntry {
	entry := t.armEvidence(unavailable, nil, t.preStatesFor(unavailable))
	for _, id := range unavailable {
		if tr := entry.targets[id]; tr != nil {
			tr.outcome = OutcomeUnavailable
			tr.settled = true
			tr.note = "unavailable at resolution time — not sent"
		}
	}

	return entry
}

// armAndSend arms the evidence collector (pre-command snapshots, D3) and
// sends one call_service per domain group, attributing each result. A nil
// error means at least one frame reached the wire; when nothing was sent the
// bare transport error is returned (the entry is finalized with error
// outcomes).
func (t *Twin) armAndSend(ctx context.Context, spec CommandSpec, targets, unavailable []string, calls []resolvedCall) (*evidenceEntry, error) {
	expects := make(map[string]stateExpectation)
	preStates := make(map[string]string)
	for _, id := range targets {
		d := domainOf(id)
		snap, ok, err := t.GetState(id)
		if err != nil || !ok {
			preStates[id] = ""
		} else {
			preStates[id] = snap.State
		}
		expect, hasExpect := expectedStateForAction(d, spec.Action, spec.Params)
		if spec.Action == "toggle" {
			expect = expectationToggle(d, preStates[id])
			hasExpect = true
		}
		if hasExpect && expect != nil {
			expects[id] = expect
		}
	}
	for _, id := range unavailable {
		preStates[id] = "unavailable"
	}

	entry := t.armEvidence(append(append([]string(nil), targets...), unavailable...), expects, preStates)
	for _, id := range unavailable {
		if tr := entry.targets[id]; tr != nil {
			tr.outcome = OutcomeUnavailable
			tr.settled = true
			tr.note = "unavailable at resolution time — not sent"
		}
	}

	var firstSendErr error
	sent := false
	for _, c := range calls {
		res, err := t.CallService(ctx, c.domain, c.service, c.ids, c.data)
		if err != nil {
			if !sent && firstSendErr == nil {
				firstSendErr = err
			}
			t.markCallFailed(entry, CallRecord{Domain: c.domain, Service: c.service, EntityIDs: c.ids}, err.Error())

			continue
		}
		sent = true
		t.observeResult(entry, CallRecord{Domain: c.domain, Service: c.service, EntityIDs: c.ids}, res)
	}

	if !sent {
		// Nothing reached the wire: finalize (error outcomes) and surface
		// the bare transport error to the adapter.
		t.finalizeEvidence(entry)

		return entry, fmt.Errorf("command not sent: %w", firstSendErr)
	}

	return entry, nil
}

// writeBarrier implements D2: live session, completed bootstrap, WS activity
// within HardStaleThreshold. Immediate refusal — never a reconnect wait.
func (t *Twin) writeBarrier() error {
	sess := t.session.Load()
	if sess == nil {
		return ErrNotConnected
	}
	if !sess.bootstrapComplete() {
		return ErrSessionNotReady
	}
	if hard := t.opts.HardStaleThreshold; hard > 0 {
		if last := t.LastWSActivity(); !last.IsZero() && time.Since(last) > hard {
			return fmt.Errorf("%w: last WS activity %ds ago", ErrStale, int(time.Since(last).Seconds()))
		}
	}

	return nil
}

// commandDomainActive reports whether a domain is in the operator-selected
// active set (D5): nil options value ⇒ every table domain except siren;
// a non-nil value is taken literally.
func (t *Twin) commandDomainActive(domain string) bool {
	for _, d := range t.opts.ActiveCommandDomains {
		if d == domain {
			return true
		}
	}

	return false
}

// ActiveCommandDomains returns the effective active domain list.
func (t *Twin) ActiveCommandDomains() []string {
	return append([]string(nil), t.opts.ActiveCommandDomains...)
}

// SetActiveCommandDomains overrides the active command-domain set (D5).
// nil restores the default (every table domain except siren); a non-nil
// slice is taken literally.
func (t *Twin) SetActiveCommandDomains(domains []string) {
	t.opts.ActiveCommandDomains = domains
}

// IsCommandDomainActive reports whether domain is active (used by the adapter
// to render the description).
func (t *Twin) IsCommandDomainActive(domain string) bool {
	return t.commandDomainActive(domain)
}

// preStatesFor snapshots the current state strings of the given entities.
func (t *Twin) preStatesFor(ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if snap, ok, err := t.GetState(id); err == nil && ok {
			out[id] = snap.State
		}
	}

	return out
}

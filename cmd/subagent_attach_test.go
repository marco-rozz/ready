package main

// The Connection attach snapshot (wire-protocol spec, "Subagent fleet
// snapshot control event"): a Connection attaching to a Session with
// Subagent history receives exactly one snapshot through the shared fleet
// limiter, before any change-driven event; a history-less Session receives
// nothing.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/subagent"
)

// testAttachRuntime wires a skeleton sessionRuntime with a recording fleet
// limiter (the channel side is not under test here — emitSubagentsSnapshot
// needs the live manager; the limiter's emissions stand in for them).
func testAttachRuntime(t *testing.T, dir string) (*sessionRuntime, *fleetLimiter, func() []string) {
	t.Helper()

	mgr, _, _ := testSessions(t, dir)

	var mu sync.Mutex
	var emissions []string
	lim := newFleetLimiter(func(id string) {
		mu.Lock()
		emissions = append(emissions, id)
		mu.Unlock()
	})

	rt := &sessionRuntime{
		sessions:     mgr,
		subagents:    subagent.New(subagent.Deps{Logger: zerolog.Nop()}, subagent.NewDefinitions(nil)),
		fleetLimiter: lim,
		logger:       zerolog.Nop(),
	}

	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), emissions...)
	}

	return rt, lim, snapshot
}

// TestAttachSnapshotExactlyOnceBeforeChangeEvents: the attach on a Session
// with a subagents/ subtree emits exactly one immediate snapshot; a
// change-driven notification arriving inside the window coalesces into the
// limiter's trailing flush (so the attach emission precedes it and no
// second immediate attach emission ever happens).
func TestAttachSnapshotExactlyOnceBeforeChangeEvents(t *testing.T) {
	ctx := context.Background()

	rt, lim, emissions := testAttachRuntime(t, t.TempDir())
	addr := session.Address("cli")
	cur := rt.sessions.Current(ctx, addr)
	if cur == nil {
		t.Fatal("no current session")
	}

	// Subagent history: the durable subtree.
	if err := os.MkdirAll(filepath.Join(cur.Dir(), "subagents", "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Attach: exactly one immediate snapshot.
	rt.maybeEmitAttachSnapshot(cur)

	// A change-driven fleet notification inside the window.
	lim.notify(cur.ID)

	// A second attach immediately after: inside the window, no second
	// immediate emission.
	rt.maybeEmitAttachSnapshot(cur)

	if got := emissions(); len(got) != 1 {
		t.Fatalf("immediate emissions = %d (%v), want exactly 1 (the attach)", len(got), got)
	}

	// The trailing flush carries the coalesced state.
	deadline := time.Now().Add(2 * time.Second)
	for len(emissions()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("trailing flush never arrived; emissions = %v", emissions())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := emissions(); len(got) != 2 || got[0] != cur.ID || got[1] != cur.ID {
		t.Fatalf("emissions = %v, want [%s %s] — the attach must precede the change flush", got, cur.ID, cur.ID)
	}
}

// TestAttachSnapshotSkipsHistorylessSession: no registry knowledge and no
// subagents/ subtree — the attach emits nothing, and the fleetless
// session's end emits nothing either.
func TestAttachSnapshotSkipsHistorylessSession(t *testing.T) {
	ctx := context.Background()

	rt, _, emissions := testAttachRuntime(t, t.TempDir())
	addr := session.Address("cli")
	cur := rt.sessions.Current(ctx, addr)

	// Attach to a history-less Session.
	rt.maybeEmitAttachSnapshot(cur)
	if got := emissions(); len(got) != 0 {
		t.Fatalf("attach on a history-less session emitted %v — want none", got)
	}

	// Session end for the fleetless session (the serve OnEnd path's
	// subagents.EndOwner call): the runtime gate suppresses the emission.
	owner := subagent.Owner{
		SessionID: cur.ID,
		Address:   string(addr),
		Kind:      subagent.OwnerChannel,
		Delivery:  subagent.Delivery{Kind: subagent.DeliveryPassive},
	}
	rt.subagents.EndOwner(ctx, owner, "command")
	if got := emissions(); len(got) != 0 {
		t.Fatalf("fleetless session end emitted %v — want none", got)
	}
}

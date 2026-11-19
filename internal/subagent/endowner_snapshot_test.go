package subagent

// The end-of-owner snapshot gate (wire-protocol spec, "Subagent fleet
// snapshot control event"): a Session with no Subagent history SHALL never
// receive the event, while a real cascade close still emits exactly one
// empty snapshot.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/marco-rozz/ready/internal/persistence"
)

// TestEndOwnerFleetlessEmitsNoSnapshot: a Session end for a session with no
// Subagent history — no live fleet, no pending handbacks, no subagents/
// subtree — never fires the fleet-changed hook (every /new, daily reset).
func TestEndOwnerFleetlessEmitsNoSnapshot(t *testing.T) {
	rt := New(testDeps(), NewDefinitions(nil))

	dir := t.TempDir()
	owner := testOwner("fleetless", "cli")
	owner.History = persistence.NewConversationHistory(
		"fleetless", persistence.NewSessionFile(filepath.Join(dir, "session.jsonl")),
	)

	var fired atomic.Int32
	rt.deps.FleetChanged = func(string) { fired.Add(1) }

	rt.EndOwner(context.Background(), owner, "command")

	if got := fired.Load(); got != 0 {
		t.Fatalf("fleet-changed fired %d times for a fleetless session end — want 0", got)
	}
}

// TestEndOwnerWithSubtreeEmitsEmptySnapshot: the durable trace counts as
// history — a Session whose folder carries a subagents/ subtree (a fleet
// that already ended, e.g. across a restart) still emits the one cascade
// snapshot on end.
func TestEndOwnerWithSubtreeEmitsEmptySnapshot(t *testing.T) {
	rt := New(testDeps(), NewDefinitions(nil))

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subagents", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	owner := testOwner("withsub", "cli")
	owner.History = persistence.NewConversationHistory(
		"withsub", persistence.NewSessionFile(filepath.Join(dir, "session.jsonl")),
	)

	var fired atomic.Int32
	rt.deps.FleetChanged = func(string) { fired.Add(1) }

	rt.EndOwner(context.Background(), owner, "command")

	if got := fired.Load(); got != 1 {
		t.Fatalf("fleet-changed fired %d times for a subtree-bearing session end — want 1", got)
	}
}

// TestEndOwnerLiveFleetEmitsEmptySnapshot: a real cascade close emits
// exactly one snapshot (the empty post-DropOwner state) — unchanged by the
// gate.
func TestEndOwnerLiveFleetEmitsEmptySnapshot(t *testing.T) {
	rt := New(testDeps(), NewDefinitions(nil))

	dir := t.TempDir()
	owner := testOwner("fleetful", "cli")
	owner.History = persistence.NewConversationHistory(
		"fleetful", persistence.NewSessionFile(filepath.Join(dir, "session.jsonl")),
	)

	// A live fleet: one registry-known agent (no dispatch launched).
	if _, err := rt.reg.Spawn(owner, SpawnOpts{Type: tTypeGP, Name: "worker-1"}); err != nil {
		t.Fatal(err)
	}

	var fired atomic.Int32
	rt.deps.FleetChanged = func(string) { fired.Add(1) }

	rt.EndOwner(context.Background(), owner, "command")

	if got := fired.Load(); got != 1 {
		t.Fatalf("fleet-changed fired %d times for a live-fleet session end — want 1", got)
	}

	if views, pending := rt.List(owner); len(views) != 0 || pending != 0 {
		t.Fatalf("post-end fleet = %d views, %d pending — want empty", len(views), pending)
	}
}

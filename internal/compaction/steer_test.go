package compaction

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/persistence"
)

func steerEntry(s string) persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleUser, Kind: persistence.KindSteer, Content: s}
}

func stoppedEntry() persistence.HistoryMessage {
	return persistence.HistoryMessage{Role: persistence.RoleUser, Kind: persistence.KindTurnStopped, Content: persistence.TurnStoppedContent}
}

// A steered Turn: user, ctx, assistant(call), result, steer, final — the
// budget edge falling on or near the steer moves to a Turn boundary, never
// splitting the steer from its Turn.
func TestPlanCut_NeverCutsAtOrBeforeASteer(t *testing.T) {
	call := persistence.ToolCallRecord{ID: "c1", Name: "light_on", Arguments: "{}"}

	var entries []persistence.HistoryMessage

	for i := 0; i < 6; i++ {
		entries = append(entries,
			u("q"), tc("c"),
			a("", call), res("c1", big(300)),
			steerEntry("<user_steer_x>correction</user_steer_x>"),
			a(big(300)),
		)
	}

	for keep := 100; keep <= 4000; keep += 100 {
		p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: keep})
		if !ok {
			continue
		}

		if isTurnMember(entries[p.Cut]) {
			t.Fatalf("keep=%d: cut %d lands on a steer", keep, p.Cut)
		}

		if p.Cut+1 < len(entries) && isTurnMember(entries[p.Cut+1]) {
			t.Fatalf("keep=%d: cut %d is immediately before a steer", keep, p.Cut)
		}
	}

	// A budget edge that falls inside the steered Turn settles on the Turn boundary.
	p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), KeepRecentTokens: 700})
	if !ok || !entries[p.Cut].IsTurnBoundary() {
		t.Fatalf("cut %d (%v) must settle on a Turn boundary", p.Cut, ok)
	}
}

// Turn-count keep mode counts steered Turns as one Turn each (a steer is not
// a boundary).
func TestPlanCut_TurnModeSteerIsNotATurn(t *testing.T) {
	var entries []persistence.HistoryMessage

	for i := 0; i < 8; i++ {
		entries = append(entries, u("q"), tc("c"), steerEntry("s1"), steerEntry("s2"), a("r"))
	}

	p, ok := PlanCut(PlanInput{Entries: entries, Lines: lines(len(entries), 0), TurnMode: true, KeepRecentTurns: 3})
	if !ok || p.Cut != 25 {
		t.Fatalf("cut = %d (%v), want 25 (last 3 Turns kept, steers stay with theirs)", p.Cut, ok)
	}
}

func TestSerialise_SteerAndStopLines(t *testing.T) {
	out := Serialise([]persistence.HistoryMessage{
		u("turn on the lights"), tc("ctx"),
		steerEntry("no, the upstairs ones"),
		a("done"),
		stoppedEntry(),
	})

	for _, want := range []string{
		"[User]: turn on the lights",
		"[User (mid-turn)]: no, the upstairs ones",
		"[Assistant]: done",
		"[Turn stopped by the user]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("serialised input missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, persistence.TurnStoppedContent) {
		t.Error("the stop marker must render as the fixed label, not its raw content")
	}
}

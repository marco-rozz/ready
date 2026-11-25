package persistence

import (
	"context"
	"testing"
	"time"
)

func steerRecord(s string, at time.Time) HistoryMessage {
	return HistoryMessage{Role: RoleUser, Kind: KindSteer, Content: s, Timestamp: at}
}

func TestSteerKinds_Classification(t *testing.T) {
	for _, k := range []MessageKind{KindSteer, KindTurnStopped} {
		m := HistoryMessage{Role: RoleUser, Kind: k}
		if m.IsTurnBoundary() {
			t.Errorf("%s must not be a Turn boundary", k)
		}

		if m.IsRecord() {
			t.Errorf("%s must not be bookkeeping", k)
		}
	}

	if !(HistoryMessage{Role: RoleUser}).IsTurnBoundary() {
		t.Error("a plain user message stays a Turn boundary")
	}
}

func TestRestore_KeepsSteerAtItsPositionAndBytes(t *testing.T) {
	h, path := newTestHistory(t)
	ctx := context.Background()
	body := "<user_steer_ab12cd34>\nThe user sent this message at 10:00:00 ...\n\nno, the upstairs ones\n</user_steer_ab12cd34>"

	h.Add(user("turn on the lights"))
	h.Add(turnCtx("ctx"))
	h.Add(assistant("", call("c1", "ha_command")))
	h.Add(toolResult("c1", SupersededToolResult))
	h.Add(steerRecord(body, time.Now()))
	h.Add(assistant("done"))

	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, path)
	msgs := r.Messages()

	if len(msgs) != 6 || msgs[4].Kind != KindSteer || msgs[4].Content != body || msgs[3].Content != SupersededToolResult {
		t.Fatalf("unexpected view: %+v", msgs)
	}

	// Two projections of the same restored view carry identical bytes.
	again := r.Messages()
	if again[4].Content != msgs[4].Content {
		t.Fatal("projection must be byte-stable")
	}

	if starts := TurnStarts(msgs); len(starts) != 1 || starts[0] != 0 {
		t.Fatalf("a steer must not open a Turn: starts=%v", starts)
	}
}

func TestSteer_MovesLastActivityButIsNotATurn(t *testing.T) {
	h, path := newTestHistory(t)
	ctx := context.Background()

	addTurn(t, h, "a")

	later := time.Now().Add(time.Hour).Round(0)
	h.Add(steerRecord("late steer", later))

	if err := h.Save(ctx); err != nil {
		t.Fatal(err)
	}

	r := reopen(t, path)
	if !r.LastActivity().Equal(later) {
		t.Errorf("LastActivity = %v, want the steer's time %v", r.LastActivity(), later)
	}
}

func TestCloseOpenToolCallGroup_DoesNotReplaceSupersededOrStopped(t *testing.T) {
	for name, fixed := range map[string]string{"superseded": SupersededToolResult, "stopped": StoppedToolResult} {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHistory(t)

			h.Add(user("x"))
			h.Add(turnCtx("ctx"))
			h.Add(assistant("", call("c1", "a"), call("c2", "b")))

			if n := h.CloseOpenToolCallGroupWith(fixed); n != 2 {
				t.Fatalf("closed %d results, want 2", n)
			}

			if h.HasOpenToolCallGroup() {
				t.Fatal("group must be closed")
			}

			if n := h.CloseOpenToolCallGroup(); n != 0 {
				t.Fatalf("interrupted closer found %d open calls; the group must already be closed", n)
			}

			for _, m := range h.Messages()[3:] {
				if m.Content != fixed {
					t.Errorf("result replaced: %q", m.Content)
				}
			}
		})
	}
}

package activity

import (
	"context"
	"testing"
	"time"
)

func TestParallelAndClose(t *testing.T) {
	tr := New()
	endA := tr.Start("a")
	endB := tr.Start("a")
	s := <-tr.Updates()
	if s.Sequence != 2 || s.ToolName != "a" || s.AdditionalTools != 1 {
		t.Fatalf("snapshot: %+v", s)
	}
	endA()
	s = <-tr.Updates()
	if s.Sequence != 3 || s.ToolName != "a" || s.AdditionalTools != 0 {
		t.Fatalf("snapshot: %+v", s)
	}
	endB()
	s = <-tr.Updates()
	if s.Sequence != 4 || s.ToolName != "" {
		t.Fatalf("snapshot: %+v", s)
	}
	tr.Close()
	endA()
	tr.Start("late")()
	if _, ok := <-tr.Updates(); ok {
		t.Fatal("tracker remained open")
	}
}

func TestEarliestDistinctAndCoalescing(t *testing.T) {
	tr := New()
	defer tr.Close()
	a := tr.Start("first")
	b := tr.Start("second")
	if s := <-tr.Updates(); s.Sequence != 2 || s.ToolName != "first" || s.AdditionalTools != 1 {
		t.Fatalf("overlap: %+v", s)
	}
	a()
	if s := <-tr.Updates(); s.Sequence != 3 || s.ToolName != "second" || s.AdditionalTools != 0 {
		t.Fatalf("remaining: %+v", s)
	}
	b()
	if s := <-tr.Updates(); s.Sequence != 4 || s.ToolName != "" {
		t.Fatalf("end: %+v", s)
	}
}

func TestTrackerIsTurnScoped(t *testing.T) {
	first, second := New(), New()
	defer first.Close()
	defer second.Close()
	ctx1 := WithTracker(context.Background(), first)
	ctx2 := WithTracker(context.Background(), second)
	Start(context.Background(), "probe")()
	done := Start(ctx1, "first")
	select {
	case got := <-first.Updates():
		if got.ToolName != "first" {
			t.Fatalf("first: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("missing first activity")
	}
	select {
	case got := <-second.Updates():
		t.Fatalf("leaked to second turn: %+v", got)
	default:
	}
	end := Start(ctx2, "second")
	if got := <-second.Updates(); got.ToolName != "second" {
		t.Fatalf("second: %+v", got)
	}
	done()
	end()
}

func TestSlowObserverDoesNotBlockTool(t *testing.T) {
	tracker := New()
	defer tracker.Close()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; i < 10000; i++ {
			tracker.Start("tool")()
		}
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("slow observer blocked tool execution")
	}
	snapshot := <-tracker.Updates()
	if snapshot.Sequence != 20000 || snapshot.ToolName != "" {
		t.Fatalf("latest snapshot: %+v", snapshot)
	}
}

func TestTrackerDoesNotPropagateToParentContext(t *testing.T) {
	parent := context.Background()
	tracker := New()
	defer tracker.Close()
	mainTurn := WithTracker(parent, tracker)
	if Enabled(parent) || !Enabled(mainTurn) {
		t.Fatal("tracker escaped its main-turn context")
	}
	Start(parent, "auxiliary")()
	select {
	case got := <-tracker.Updates():
		t.Fatalf("auxiliary context published: %+v", got)
	default:
	}
}

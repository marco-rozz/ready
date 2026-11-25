package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/steer"
)

const waitedOneSecond = "Waited 1 seconds successfully."

func TestWaitTool_Info(t *testing.T) {
	wt := NewWaitTool()

	info, err := wt.Info(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if info.Name != "wait" {
		t.Errorf("expected name 'wait', got %q", info.Name)
	}

	if info.Desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestWaitTool_InvokableRun_Success(t *testing.T) {
	wt := NewWaitTool()
	ctx := context.Background()

	start := time.Now()
	result, err := wt.InvokableRun(ctx, `{"seconds": 1}`)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if elapsed < 900*time.Millisecond {
		t.Errorf("expected at least ~1s wait, got %v", elapsed)
	}

	if result != waitedOneSecond {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestWaitTool_InvokableRun_InvalidJSON(t *testing.T) {
	wt := NewWaitTool()

	_, err := wt.InvokableRun(context.Background(), `{bad}`)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestWaitTool_InvokableRun_ZeroSeconds(t *testing.T) {
	wt := NewWaitTool()

	_, err := wt.InvokableRun(context.Background(), `{"seconds": 0}`)
	if err == nil {
		t.Fatal("expected error for zero seconds")
	}
}

func TestWaitTool_InvokableRun_NegativeSeconds(t *testing.T) {
	wt := NewWaitTool()

	_, err := wt.InvokableRun(context.Background(), `{"seconds": -5}`)
	if err == nil {
		t.Fatal("expected error for negative seconds")
	}
}

func TestWaitTool_InvokableRun_ExceedsMax(t *testing.T) {
	wt := NewWaitTool()

	_, err := wt.InvokableRun(context.Background(), `{"seconds": 999}`)
	if err == nil {
		t.Fatal("expected error for exceeding max seconds")
	}
}

func TestWaitTool_InvokableRun_ContextCancelled(t *testing.T) {
	wt := NewWaitTool()
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel immediately.
	cancel()

	_, err := wt.InvokableRun(ctx, `{"seconds": 60}`)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestWaitTool_InvokableRun_RefusesDeadlineOvershoot(t *testing.T) {
	wt := NewWaitTool()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	start := time.Now()
	_, err := wt.InvokableRun(ctx, `{"seconds": 2}`)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrWaitExceedsTurnBudget) {
		t.Fatalf("expected ErrWaitExceedsTurnBudget, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected immediate refusal, but waited %v", elapsed)
	}
	if !strings.Contains(err.Error(), "6s left") && !strings.Contains(err.Error(), "5s left") {
		t.Errorf("refusal must name remaining seconds, got %q", err)
	}
	if !strings.Contains(err.Error(), "shorten the pause or split the sequence") {
		t.Errorf("refusal must be actionable, got %q", err)
	}
}

func TestWaitTool_InvokableRun_WithinDeadline(t *testing.T) {
	wt := NewWaitTool()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := wt.InvokableRun(ctx, `{"seconds": 1}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != waitedOneSecond {
		t.Errorf("result = %q", result)
	}
}

func steerTurn(t *testing.T) (*steer.Inbox, *steer.Turn) {
	t.Helper()

	in := steer.New(nil, steer.WithStopCommand())
	turn := in.OpenTurn(steer.TurnSpec{ID: "t1", Steerable: true, MaxSteers: 8, MaxRestarts: 4})

	return in, turn
}

func TestWaitTool_EndsOnSteeringCandidate(t *testing.T) {
	in, turn := steerTurn(t)
	ctx := steer.WithTurn(context.Background(), turn)

	go func() {
		time.Sleep(50 * time.Millisecond)
		in.Admit(channel.Message{Content: "no, the other one", Origin: channel.OriginHuman})
	}()

	start := time.Now()

	got, err := NewWaitTool().InvokableRun(ctx, `{"seconds":120}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	if got != steer.WaitInterruptedSteer {
		t.Fatalf("result = %q", got)
	}

	if time.Since(start) > 3*time.Second {
		t.Fatal("the 120s wait was not interrupted promptly")
	}

	if ctx.Err() != nil {
		t.Fatal("interrupting a wait must not cancel the Turn context")
	}
}

func TestWaitTool_EndsOnStop(t *testing.T) {
	in, turn := steerTurn(t)
	ctx := steer.WithTurn(context.Background(), turn)

	go func() {
		time.Sleep(50 * time.Millisecond)
		in.Admit(channel.Message{Content: "/stop", Origin: channel.OriginHuman})
	}()

	got, err := NewWaitTool().InvokableRun(ctx, `{"seconds":120}`)
	if err != nil || got != steer.WaitInterruptedStop {
		t.Fatalf("result = %q err=%v", got, err)
	}
}

func TestWaitTool_CandidateAlreadyPendingReturnsAtOnce(t *testing.T) {
	in, turn := steerTurn(t)
	in.Admit(channel.Message{Content: "early", Origin: channel.OriginHuman})

	ctx := steer.WithTurn(context.Background(), turn)

	start := time.Now()

	if got, _ := NewWaitTool().InvokableRun(ctx, `{"seconds":60}`); got != steer.WaitInterruptedSteer {
		t.Fatalf("result = %q", got)
	}

	if time.Since(start) > time.Second {
		t.Fatal("a pending candidate must end the wait immediately")
	}
}

// The signal is not the Turn context: a concurrent tool sharing the context
// is never cancelled by an interruption.
func TestWaitTool_InterruptionDoesNotCancelConcurrentTool(t *testing.T) {
	in, turn := steerTurn(t)
	ctx := steer.WithTurn(context.Background(), turn)

	concurrentDone := make(chan error, 1)

	go func() {
		select {
		case <-ctx.Done():
			concurrentDone <- ctx.Err()
		case <-time.After(300 * time.Millisecond):
			concurrentDone <- nil
		}
	}()

	go func() {
		time.Sleep(20 * time.Millisecond)
		in.Admit(channel.Message{Content: "correction", Origin: channel.OriginHuman})
	}()

	if _, err := NewWaitTool().InvokableRun(ctx, `{"seconds":30}`); err != nil {
		t.Fatal(err)
	}

	if err := <-concurrentDone; err != nil {
		t.Fatalf("a concurrent tool on the Turn context was cancelled: %v", err)
	}
}

func TestWaitTool_NoTurnHandleKeepsTimerSemantics(t *testing.T) {
	got, err := NewWaitTool().InvokableRun(context.Background(), `{"seconds":1}`)
	if err != nil || got != waitedOneSecond {
		t.Fatalf("result=%q err=%v", got, err)
	}
}

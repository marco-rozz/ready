package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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

	if result != "Waited 1 seconds successfully." {
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
	if result != "Waited 1 seconds successfully." {
		t.Errorf("result = %q", result)
	}
}

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/protocol"
)

func TestNonInteractiveActivityDoesNotExtendDeadline(t *testing.T) {
	r := newRecvLoop(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case r.control <- protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, Activity: &protocol.Activity{State: "thinking", Sequence: 1}}:
				default:
				}
			}
		}
	}()
	started := time.Now()
	_, err := waitReplyIgnoringModelStatus(ctx, r, 40*time.Millisecond, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout: %v", err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("activity extended timeout")
	}
}

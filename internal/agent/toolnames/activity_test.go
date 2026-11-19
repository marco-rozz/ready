package toolnames

import (
	"context"
	"testing"

	"github.com/marco-rozz/ready/internal/activity"
)

func TestPolicyBlockPublishesOnlyItsIdentity(t *testing.T) {
	tracker := activity.New()
	defer tracker.Close()
	ctx := activity.WithTracker(context.Background(), tracker)
	if _, err := NewPolicyBlockTool().InvokableRun(ctx, `{"tool":"private_target"}`); err != nil {
		t.Fatal(err)
	}
	snapshot := <-tracker.Updates()
	if snapshot.Sequence != 2 || snapshot.ToolName != "" {
		t.Fatalf("unexpected completed activity: %+v", snapshot)
	}
}

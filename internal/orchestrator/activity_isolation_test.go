package orchestrator

import (
	"context"
	"testing"

	"github.com/marco-rozz/ready/internal/activity"
)

func TestSyntheticProbeToolCannotPublishActivity(t *testing.T) {
	tracker := activity.New()
	defer tracker.Close()
	ctx := activity.WithTracker(context.Background(), tracker)
	if _, err := (&probeNoopTool{}).InvokableRun(ctx, `{"value":1}`); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-tracker.Updates():
		t.Fatalf("probe published activity: %+v", got)
	default:
	}
}

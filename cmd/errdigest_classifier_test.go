package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/errdigest"
	"github.com/marco-rozz/ready/internal/subagent"
	"github.com/rs/zerolog"
)

// The daemon classifier maps the Subagent runtime's permanent refusals to
// PERMANENT: the model must see [PERMANENT] and report the capability as
// unavailable, never [UNKNOWN] (which reads as "the tool does not exist")
// and never a retryable transient.
func TestErrDigestClassifierSubagentRefusals(t *testing.T) {
	c := errDigestClassifier()

	if got := c.Classify(subagent.ErrFeatureDisabled); got != errdigest.Permanent {
		t.Fatalf("ErrFeatureDisabled classified %q, want %q", got, errdigest.Permanent)
	}

	if got := c.Classify(subagent.ErrModelPinOutsideChain); got != errdigest.Permanent {
		t.Fatalf("ErrModelPinOutsideChain classified %q, want %q", got, errdigest.Permanent)
	}
}

// disabledSpawnTool fails every run with the disabled-feature sentinel, the
// way subagent_spawn does while subagents.enabled is false.
type disabledSpawnTool struct{}

var _ tool.InvokableTool = (*disabledSpawnTool)(nil)

func (disabledSpawnTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "subagent_spawn"}, nil
}

func (disabledSpawnTool) InvokableRun(_ context.Context, _ string, _ ...tool.Option) (string, error) {
	return "", subagent.ErrFeatureDisabled
}

// The observation the model receives carries the [PERMANENT] tag and the
// fixed sentinel text — a refused call never reaches it as [UNKNOWN] or as
// a bare error dump.
func TestErrDigestDisabledFeatureObservation(t *testing.T) {
	tracked := errdigest.NewTrackedTool(disabledSpawnTool{}, errDigestClassifier(),
		func(context.Context) string { return testCLIChannelName }, zerolog.Nop())

	out, err := tracked.InvokableRun(t.Context(), "{}")
	if err != nil {
		t.Fatalf("tracked tool must not fail the call: %v", err)
	}

	if !strings.Contains(out, "[PERMANENT]") || !strings.Contains(out, "subagents feature is disabled") {
		t.Fatalf("observation = %q", out)
	}
}

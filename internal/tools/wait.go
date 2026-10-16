// Package tools provides built-in utility tools for the READY. agent.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// MaxWaitSeconds is the maximum allowed wait duration to prevent abuse.
const MaxWaitSeconds = 300

const waitResponseBuffer = 5 * time.Second

// ErrWaitExceedsTurnBudget is returned when the requested pause would leave
// no time for the next model iteration. TrackedTool converts it into a normal
// transient observation, so ReAct continues without completing the plan step.
var ErrWaitExceedsTurnBudget = errors.New("wait exceeds remaining turn budget")

// Compile-time assertion that WaitTool implements InvokableTool.
var _ tool.InvokableTool = (*WaitTool)(nil)

// waitArgs is the input structure for the wait tool.
type waitArgs struct {
	Seconds int `json:"seconds"`
}

// WaitTool implements tool.InvokableTool for pausing execution between actions.
// This enables the agent to execute timed sequences (e.g., change light color,
// wait 10 seconds, change again).
type WaitTool struct{}

// NewWaitTool creates a new WaitTool.
func NewWaitTool() *WaitTool {
	return &WaitTool{}
}

// Info returns the tool metadata for the LLM.
func (t *WaitTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "wait",
		Desc: "Wait for a specified number of seconds before proceeding to the next action. " +
			"Use this when the user requests timed sequences (e.g., 'change color after 10 seconds'). " +
			fmt.Sprintf("Maximum wait time is %d seconds.", MaxWaitSeconds),
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"seconds": {
				Type:     schema.Number,
				Desc:     "Number of seconds to wait (1-300)",
				Required: true,
			},
		}),
	}, nil
}

// InvokableRun pauses execution for the requested duration, respecting context cancellation.
func (t *WaitTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args waitArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("failed to parse wait arguments: %w", err)
	}

	if args.Seconds <= 0 {
		return "", fmt.Errorf("failed to wait: seconds must be positive, got %d", args.Seconds)
	}

	if args.Seconds > MaxWaitSeconds {
		return "", fmt.Errorf("failed to wait: seconds exceeds maximum of %d, got %d", MaxWaitSeconds, args.Seconds)
	}

	duration := time.Duration(args.Seconds) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if duration > remaining-waitResponseBuffer {
			remainingSeconds := max(int64(0), int64(remaining/time.Second))

			return "", fmt.Errorf("%w: wait(%d)s exceeds the remaining turn budget (%ds left); shorten the pause or split the sequence into multiple turns", ErrWaitExceedsTurnBudget, args.Seconds, remainingSeconds)
		}
	}

	select {
	case <-time.After(duration):
		return fmt.Sprintf("Waited %d seconds successfully.", args.Seconds), nil
	case <-ctx.Done():
		return "", fmt.Errorf("wait interrupted after partial duration: %w", ctx.Err())
	}
}

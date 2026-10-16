package errdigest

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// Compile-time assertion.
var _ tool.InvokableTool = (*TrackedTool)(nil)

// TrackedTool wraps an InvokableTool with error digestion: a failing call is
// classified, logged with tool, session and severity, and handed back to the
// model as a `[<SEVERITY>] <error>` observation instead of a Go error, so the
// ReAct loop continues. It keeps no call-sequence state and emits nothing
// else (session-lifecycle D10).
type TrackedTool struct {
	inner      tool.InvokableTool
	classifier *Classifier
	sessionKey func(ctx context.Context) string
	logger     zerolog.Logger
}

// NewTrackedTool wraps inner with error digestion. sessionKeyFn extracts the
// session identifier logged with every failure.
func NewTrackedTool(
	inner tool.InvokableTool,
	classifier *Classifier,
	sessionKeyFn func(ctx context.Context) string,
	logger zerolog.Logger,
) *TrackedTool {
	return &TrackedTool{
		inner:      inner,
		classifier: classifier,
		sessionKey: sessionKeyFn,
		logger:     logger,
	}
}

// Info delegates to the inner tool.
func (t *TrackedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// InvokableRun runs the inner tool and turns a failure into a severity-tagged
// observation.
func (t *TrackedTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	result, err := t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	if err == nil {
		return result, nil
	}

	toolName := "unknown"
	if info, infoErr := t.inner.Info(ctx); infoErr == nil && info != nil {
		toolName = info.Name
	}

	severity := t.classifier.Classify(err)

	t.logger.Error().
		Str("tool", toolName).
		Str("session", t.sessionKey(ctx)).
		Str("severity", string(severity)).
		Str("error", err.Error()).
		Msg("tool call failed")

	// Return the enriched error as the tool observation (not as a Go error):
	// Eino's ToolNode treats a non-nil error as a fatal turn failure, which
	// would abort the ReAct loop. By surfacing the failure as the tool's
	// textual result, the LLM sees it as an observation and can decide how
	// to proceed (retry, explain to the user, call another tool, etc.).
	return fmt.Sprintf("[%s] %s", severity, err.Error()), nil
}

// WrapTools wraps every InvokableTool in agentTools with a TrackedTool.
// Non-invokable tools (BaseTool only) are passed through unchanged.
func WrapTools(
	agentTools []tool.BaseTool,
	classifier *Classifier,
	sessionKeyFn func(ctx context.Context) string,
	logger zerolog.Logger,
) []tool.BaseTool {
	wrapped := make([]tool.BaseTool, len(agentTools))

	for i, t := range agentTools {
		if invokable, ok := t.(tool.InvokableTool); ok {
			wrapped[i] = NewTrackedTool(invokable, classifier, sessionKeyFn, logger)
		} else {
			wrapped[i] = t
		}
	}

	return wrapped
}

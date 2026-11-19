package errdigest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/activity"
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
	guard := guardFromContext(ctx)
	var key [32]byte
	if guard != nil {
		info, err := t.inner.Info(ctx)
		if err != nil {
			return "", err
		}
		if info == nil || info.Name == "" {
			return "", fmt.Errorf("tool identity unavailable")
		}
		key = fingerprint(info.Name, argumentsInJSON)
		if _, err := guard.begin(ctx, key); err != nil {
			return "", err
		}
	}
	invoke := func() (string, error) {
		if activity.Enabled(ctx) {
			if info, infoErr := t.inner.Info(ctx); infoErr == nil && info != nil && info.Name != "" {
				defer activity.Start(ctx, info.Name)()
			}
		}

		return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
	}
	result, err := invoke()
	if guard != nil {
		guard.finish(key, err != nil && t.classifier.Classify(err) == Permanent)
	}
	if errors.Is(err, ErrGuardEditRetries) {
		return "", err
	}
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

// trackedDualTool preserves both tool interfaces and applies the same guard
// before either invocation path begins.
type trackedDualTool struct {
	*TrackedTool
	stream *activity.StreamTool
}

var (
	_ tool.InvokableTool  = (*trackedDualTool)(nil)
	_ tool.StreamableTool = (*trackedDualTool)(nil)
)

func (t *trackedDualTool) StreamableRun(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	return guardedStream(ctx, args, t.TrackedTool, func() (*schema.StreamReader[string], error) { return t.stream.StreamableRun(ctx, args, opts...) })
}

// guardedStream shares the invocation guard across standard and enhanced streams.
func guardedStream[T any](ctx context.Context, args string, tracked *TrackedTool, invoke func() (*schema.StreamReader[T], error)) (*schema.StreamReader[T], error) {
	guard := guardFromContext(ctx)
	if guard == nil {
		return invoke()
	}
	info, err := tracked.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info == nil || info.Name == "" {
		return nil, fmt.Errorf("tool identity unavailable")
	}
	key := fingerprint(info.Name, args)
	if _, err = guard.begin(ctx, key); err != nil {
		return nil, err
	}
	var once sync.Once
	finish := func(permanent bool) { once.Do(func() { guard.finish(key, permanent) }) }
	handedOff := false
	defer func() {
		if !handedOff {
			finish(false)
		}
	}()
	reader, err := invoke()
	if err != nil {
		finish(tracked.classifier.Classify(err) == Permanent)

		return reader, err
	}
	if reader == nil {
		return nil, nil
	}
	reader = schema.StreamReaderWithConvert(reader, func(value T) (T, error) { return value, nil },
		schema.WithErrWrapper(func(streamErr error) error {
			finish(tracked.classifier.Classify(streamErr) == Permanent)

			return streamErr
		}),
		schema.WithOnEOF(func() (any, error) {
			finish(false)

			return nil, io.EOF
		}),
	)
	stop := context.AfterFunc(ctx, func() { finish(false) })
	reader.OnClose(func() { stop(); finish(false) })
	handedOff = true

	return reader, nil
}

// Each combination preserves the exact tool interfaces of its underlying tool.
type standardEnhancedTool struct {
	*TrackedTool
	stream *activity.EnhancedStreamTool
}

func (t *standardEnhancedTool) StreamableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	text := ""
	if args != nil {
		text = args.Text
	}

	return guardedStream(ctx, text, t.TrackedTool, func() (*schema.StreamReader[*schema.ToolResult], error) {
		return t.stream.StreamableRun(ctx, args, opts...)
	})
}

type enhancedStandardTool struct {
	*activity.EnhancedInvokeTool
	stream *activity.StreamTool
}

func (t *enhancedStandardTool) StreamableRun(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	return t.stream.StreamableRun(ctx, args, opts...)
}

type enhancedDualTool struct {
	*activity.EnhancedInvokeTool
	stream *activity.EnhancedStreamTool
}

func (t *enhancedDualTool) StreamableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	return t.stream.StreamableRun(ctx, args, opts...)
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
		invokable, canInvoke := t.(tool.InvokableTool)
		streamable, canStream := t.(tool.StreamableTool)
		enhancedInvoke, canEnhancedInvoke := t.(tool.EnhancedInvokableTool)
		enhancedStream, canEnhancedStream := t.(tool.EnhancedStreamableTool)
		switch {
		case canInvoke && canEnhancedStream:
			wrapped[i] = &standardEnhancedTool{TrackedTool: NewTrackedTool(invokable, classifier, sessionKeyFn, logger), stream: &activity.EnhancedStreamTool{Inner: enhancedStream}}
		case canEnhancedInvoke && canStream:
			wrapped[i] = &enhancedStandardTool{EnhancedInvokeTool: &activity.EnhancedInvokeTool{Inner: enhancedInvoke}, stream: &activity.StreamTool{Inner: streamable}}
		case canEnhancedInvoke && canEnhancedStream:
			wrapped[i] = &enhancedDualTool{EnhancedInvokeTool: &activity.EnhancedInvokeTool{Inner: enhancedInvoke}, stream: &activity.EnhancedStreamTool{Inner: enhancedStream}}
		case canEnhancedInvoke:
			wrapped[i] = &activity.EnhancedInvokeTool{Inner: enhancedInvoke}
		case canEnhancedStream:
			wrapped[i] = &activity.EnhancedStreamTool{Inner: enhancedStream}
		case canInvoke && canStream:
			wrapped[i] = &trackedDualTool{TrackedTool: NewTrackedTool(invokable, classifier, sessionKeyFn, logger), stream: &activity.StreamTool{Inner: streamable}}
		case canInvoke:
			wrapped[i] = NewTrackedTool(invokable, classifier, sessionKeyFn, logger)
		case canStream:
			wrapped[i] = &activity.StreamTool{Inner: streamable}
		default:
			wrapped[i] = t
		}
	}

	return wrapped
}

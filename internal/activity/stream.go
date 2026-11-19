package activity

import (
	"context"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// StreamTool observes a streamable tool without changing its schema.
type StreamTool struct{ Inner tool.StreamableTool }

var _ tool.StreamableTool = (*StreamTool)(nil)

func (t *StreamTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return t.Inner.Info(ctx) }

func (t *StreamTool) StreamableRun(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	return observeStream(ctx, t.Inner, func() (*schema.StreamReader[string], error) {
		return t.Inner.StreamableRun(ctx, args, opts...)
	})
}

func observeStream[T any](ctx context.Context, inner tool.BaseTool, invoke func() (*schema.StreamReader[T], error)) (*schema.StreamReader[T], error) {
	var done func()
	if Enabled(ctx) {
		if info, err := inner.Info(ctx); err == nil && info != nil && info.Name != "" {
			done = Start(ctx, info.Name)
		}
	}
	if done == nil {
		return invoke()
	}
	handedOff := false
	defer func() {
		if !handedOff {
			done()
		}
	}()
	reader, err := invoke()
	if err != nil || reader == nil {
		return reader, err
	}
	output, writer := schema.Pipe[T](0)
	output.OnClose(done)
	stopCancellation := context.AfterFunc(ctx, done)
	handedOff = true
	go func() {
		defer stopCancellation()
		defer done()
		defer reader.Close()
		defer writer.Close()
		for {
			value, recvErr := reader.Recv()
			if errors.Is(recvErr, io.EOF) {
				return
			}
			if recvErr != nil {
				var zero T
				writer.Send(zero, recvErr)

				return
			}
			if writer.Send(value, nil) {
				return
			}
		}
	}()

	return output, nil
}

package activity

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// EnhancedInvokeTool preserves the enhanced invocation interface and result.
type EnhancedInvokeTool struct{ Inner tool.EnhancedInvokableTool }

func (t *EnhancedInvokeTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.Inner.Info(ctx)
}

func (t *EnhancedInvokeTool) InvokableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
	if Enabled(ctx) {
		if info, err := t.Inner.Info(ctx); err == nil && info != nil && info.Name != "" {
			defer Start(ctx, info.Name)()
		}
	}

	return t.Inner.InvokableRun(ctx, args, opts...)
}

// EnhancedStreamTool observes the lifetime of an enhanced streaming result.
type EnhancedStreamTool struct{ Inner tool.EnhancedStreamableTool }

func (t *EnhancedStreamTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.Inner.Info(ctx)
}

func (t *EnhancedStreamTool) StreamableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	return observeStream(ctx, t.Inner, func() (*schema.StreamReader[*schema.ToolResult], error) {
		return t.Inner.StreamableRun(ctx, args, opts...)
	})
}

var (
	_ tool.EnhancedInvokableTool  = (*EnhancedInvokeTool)(nil)
	_ tool.EnhancedStreamableTool = (*EnhancedStreamTool)(nil)
)

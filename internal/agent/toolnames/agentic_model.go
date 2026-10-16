package toolnames

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"
)

// NormalizingAgenticModel is the AgenticModel counterpart of
// [NormalizingChatModel] for the `responses` construction path
// (adk.NewTypedChatModelAgent[*schema.AgenticMessage]). AgenticModel exposes
// no WithTools — tools ride the per-request model.Option — so the wrapper is
// Generate/Stream only, mirroring usage.RecordedAgenticModel.
//
// It normalises hallucinated tool-call names carried in FunctionToolCall
// content blocks and, when a Disclosure is attached, rewrites calls that
// resolve unambiguously against the blocked (ghost) index into the
// policy_block sentinel — so a blocked call reaches the sentinel identically
// on both construction paths.
type NormalizingAgenticModel struct {
	base     model.AgenticModel
	resolver *Resolver
	logger   zerolog.Logger
	disclose *Disclosure
}

// Compile-time assertion (code style: interface implementation validation).
var _ model.AgenticModel = (*NormalizingAgenticModel)(nil)

// NewNormalizingAgenticModel wraps base with tool-call-name normalisation;
// disclosure disabled.
func NewNormalizingAgenticModel(base model.AgenticModel, resolver *Resolver, logger zerolog.Logger) *NormalizingAgenticModel {
	return &NormalizingAgenticModel{
		base:     base,
		resolver: resolver,
		logger:   logger.With().Str("component", "toolnames").Logger(),
	}
}

// NewDisclosingAgenticModel wraps base like NewNormalizingAgenticModel and
// enables blocked-call disclosure under the named active profile.
func NewDisclosingAgenticModel(base model.AgenticModel, resolver *Resolver, logger zerolog.Logger, profile string) *NormalizingAgenticModel {
	return &NormalizingAgenticModel{
		base:     base,
		resolver: resolver,
		logger:   logger.With().Str("component", "toolnames").Logger(),
		disclose: &Disclosure{Profile: profile},
	}
}

// Generate delegates to the wrapped model and rewrites tool-call names on
// the returned message.
func (n *NormalizingAgenticModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	msg, err := n.base.Generate(ctx, input, opts...)
	if err != nil || msg == nil {
		return msg, err
	}

	n.normalizeAgentic(msg)

	return msg, nil
}

// Stream delegates to the wrapped model and rewrites tool-call names on each
// streamed chunk. Rewrites are idempotent, so re-application on incremental
// chunks is safe.
func (n *NormalizingAgenticModel) Stream(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	reader, err := n.base.Stream(ctx, input, opts...)
	if err != nil || reader == nil {
		return reader, err
	}

	sr, sw := schema.Pipe[*schema.AgenticMessage](0)

	go func() {
		defer reader.Close()
		defer sw.Close()

		for {
			chunk, recvErr := reader.Recv()
			if errors.Is(recvErr, io.EOF) {
				return
			}
			if recvErr != nil {
				sw.Send(nil, recvErr)

				return
			}

			if chunk != nil {
				n.normalizeAgentic(chunk)
			}

			if closed := sw.Send(chunk, nil); closed {
				return
			}
		}
	}()

	return sr, nil
}

// normalizeAgentic walks the message's FunctionToolCall content blocks and
// applies the same classification as the chat path: registered rewrite
// first, then blocked rewrite into the sentinel, else pass-through.
func (n *NormalizingAgenticModel) normalizeAgentic(msg *schema.AgenticMessage) {
	if n == nil || n.resolver == nil || msg == nil {
		return
	}

	for _, block := range msg.ContentBlocks {
		if block == nil || block.Type != schema.ContentBlockTypeFunctionToolCall || block.FunctionToolCall == nil {
			continue
		}

		call := block.FunctionToolCall
		called := call.Name
		if called == "" || n.resolver.Known(called) {
			continue
		}

		if canonical, ok := n.resolver.Resolve(called); ok {
			n.logger.Warn().
				Str("called", called).
				Str("canonical", canonical).
				Str("tool_call_id", call.CallID).
				Msg("rewriting hallucinated tool name")

			call.Name = canonical

			continue
		}

		if !n.rewriteBlockedAgentic(call, called) {
			n.logger.Warn().
				Str("called", called).
				Str("tool_call_id", call.CallID).
				Msg("tool name not resolvable — passing through (Eino will fail downstream)")
		}
	}
}

// rewriteBlockedAgentic, when disclosure is enabled and the called name
// resolves unambiguously against the blocked index, rewrites the call into
// the policy_block sentinel with {tool, owner, profile} arguments.
func (n *NormalizingAgenticModel) rewriteBlockedAgentic(call *schema.FunctionToolCall, called string) bool {
	if n.disclose == nil {
		return false
	}

	blocked, ok := n.resolver.ResolveBlocked(called)
	if !ok {
		return false
	}

	args, err := json.Marshal(policyBlockArgs{
		Tool:    blocked.Name,
		Owner:   blocked.Owner,
		Profile: n.disclose.Profile,
	})
	if err != nil {
		return false
	}

	n.logger.Warn().
		Str("called", called).
		Str("blocked_tool", blocked.Name).
		Str("owner", blocked.Owner).
		Str("profile", n.disclose.Profile).
		Str("tool_call_id", call.CallID).
		Msg("rewriting blocked tool call into policy_block disclosure")

	call.Name = PolicyBlockToolName
	call.Arguments = string(args)

	return true
}

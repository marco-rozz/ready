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

// NormalizingChatModel wraps a ToolCallingChatModel and rewrites hallucinated
// tool-call names on the model's output using a Resolver.
//
// It is transparent to Eino and to the underlying provider: the model still
// sees the true tool schemas via WithTools; only the *response* names get
// normalised. Failing rewrites are logged at Warn and left untouched (Eino's
// downstream error remains informative).
//
// When a Disclosure is attached (disclose_denied_tools knob on), a call that
// matches no registered name but resolves unambiguously against the
// resolver's blocked (ghost) index is rewritten into the policy_block
// sentinel tool with {tool, owner, profile} arguments — the sentinel returns
// the fixed disclosure message as the call's tool result, and the ReAct loop
// continues instead of failing.
type NormalizingChatModel struct {
	base     model.ToolCallingChatModel
	resolver *Resolver
	logger   zerolog.Logger
	disclose *Disclosure
}

// Disclosure carries the profile-scoped configuration of the blocked-call
// disclosure. nil on the wrapper disables the behavior entirely.
type Disclosure struct {
	// Profile is the active profile name, interpolated into the sentinel's
	// arguments and disclosure message.
	Profile string
}

// Compile-time assertion that NormalizingChatModel implements the interface.
var _ model.ToolCallingChatModel = (*NormalizingChatModel)(nil)

// NewNormalizingChatModel wraps base with tool-name normalisation using the
// given resolver. resolver may be nil, in which case the wrapper degrades to
// a pass-through. Disclosure is disabled.
func NewNormalizingChatModel(base model.ToolCallingChatModel, resolver *Resolver, logger zerolog.Logger) *NormalizingChatModel {
	return &NormalizingChatModel{
		base:     base,
		resolver: resolver,
		logger:   logger.With().Str("component", "toolnames").Logger(),
	}
}

// NewDisclosingChatModel wraps base like NewNormalizingChatModel and
// additionally enables blocked-call disclosure under the named active
// profile (disclose_denied_tools knob on).
func NewDisclosingChatModel(base model.ToolCallingChatModel, resolver *Resolver, logger zerolog.Logger, profile string) *NormalizingChatModel {
	return &NormalizingChatModel{
		base:     base,
		resolver: resolver,
		logger:   logger.With().Str("component", "toolnames").Logger(),
		disclose: &Disclosure{Profile: profile},
	}
}

// Generate delegates to the wrapped model and rewrites tool-call names on the
// returned message.
func (n *NormalizingChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := n.base.Generate(ctx, input, opts...)
	if err != nil || msg == nil {
		return msg, err
	}

	n.normalize(msg)

	return msg, nil
}

// Stream delegates to the wrapped model and rewrites tool-call names on each
// streamed chunk. Rewrites are idempotent, so re-application on incremental
// chunks is safe.
func (n *NormalizingChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	reader, err := n.base.Stream(ctx, input, opts...)
	if err != nil || reader == nil {
		return reader, err
	}

	sr, sw := schema.Pipe[*schema.Message](0)

	go n.forwardStream(reader, sw)

	return sr, nil
}

// WithTools forwards to the wrapped model, re-wrapping the derived model with
// the same resolver so the normalisation layer is preserved across per-request
// tool bindings issued by Eino's toolsNode.
func (n *NormalizingChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	derived, err := n.base.WithTools(tools)
	if err != nil {
		return nil, err
	}

	return &NormalizingChatModel{
		base:     derived,
		resolver: n.resolver,
		logger:   n.logger,
		disclose: n.disclose,
	}, nil
}

// normalize walks msg.ToolCalls and rewrites any name that the resolver can
// map unambiguously to a registered display name. When disclosure is enabled,
// a name that matches no registered tool but resolves against the blocked
// (ghost) index is rewritten into the policy_block sentinel call.
func (n *NormalizingChatModel) normalize(msg *schema.Message) {
	if n == nil || n.resolver == nil || msg == nil {
		return
	}

	for i := range msg.ToolCalls {
		called := msg.ToolCalls[i].Function.Name
		if called == "" || n.resolver.Known(called) {
			continue
		}

		canonical, ok := n.resolver.Resolve(called)
		if !ok {
			if !n.rewriteBlocked(&msg.ToolCalls[i], called) {
				n.logger.Warn().
					Str("called", called).
					Str("tool_call_id", msg.ToolCalls[i].ID).
					Msg("tool name not resolvable — passing through (Eino will fail downstream)")
			}

			continue
		}

		n.logger.Warn().
			Str("called", called).
			Str("canonical", canonical).
			Str("tool_call_id", msg.ToolCalls[i].ID).
			Msg("rewriting hallucinated tool name")

		msg.ToolCalls[i].Function.Name = canonical
	}
}

// rewriteBlocked, when disclosure is enabled and the called name resolves
// unambiguously against the blocked index, rewrites the call into the
// policy_block sentinel with {tool, owner, profile} arguments. Returns true
// when the rewrite was applied.
func (n *NormalizingChatModel) rewriteBlocked(call *schema.ToolCall, called string) bool {
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
		// Marshal of three strings cannot fail; kept for safety — degrade to
		// pass-through rather than emit a malformed call.
		return false
	}

	n.logger.Warn().
		Str("called", called).
		Str("blocked_tool", blocked.Name).
		Str("owner", blocked.Owner).
		Str("profile", n.disclose.Profile).
		Str("tool_call_id", call.ID).
		Msg("rewriting blocked tool call into policy_block disclosure")

	call.Function.Name = PolicyBlockToolName
	call.Function.Arguments = string(args)

	return true
}

// forwardStream pumps normalised chunks from the base reader into sw.
func (n *NormalizingChatModel) forwardStream(reader *schema.StreamReader[*schema.Message], sw *schema.StreamWriter[*schema.Message]) {
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
			n.normalize(chunk)
		}

		if closed := sw.Send(chunk, nil); closed {
			return
		}
	}
}

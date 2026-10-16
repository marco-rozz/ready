package toolnames

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// PolicyBlockToolName is the registered, advertised name of the sentinel
// tool that delivers the profile-block disclosure into the ReAct loop. The
// tool is bound into ToolsConfig (and therefore advertised to the model)
// only when the active profile's disclose_denied_tools knob is on; the
// NormalizingChatModel / NormalizingAgenticModel rewrite a call classified
// as blocked into this name before Eino's toolsNode indexes it by call id.
const PolicyBlockToolName = "policy_block"

// BlockMessage renders the fixed disclosure message delivered as the tool
// result of a policy_block call: constant English text plus the
// three interpolations — tool LLM-facing name, owner, active profile name.
// It carries no error status: the ReAct loop is expected to stop retrying
// the blocked path and answer the user.
func BlockMessage(toolName, owner, profile string) string {
	return fmt.Sprintf(
		"[POLICY BLOCK] The tool %q exists but is not available: the active profile %q disallows it (owner: %s). "+
			"Do not seek alternative routes to reach it — they may be equally blocked by policy. "+
			"If no allowed tool can serve this request, inform the user that the data or action is currently unreachable.",
		toolName, profile, owner,
	)
}

// directCallMessage is the fixed informational response returned when the
// model calls the sentinel directly with no usable {tool} argument.
const directCallMessage = "[POLICY BLOCK] This tool is invoked automatically when a requested tool is " +
	"disallowed by the active profile. It carries no capability of its own; call the tools listed in your tool catalog instead."

// policyBlockArgs is the argument shape the normalising wrappers write when
// rewriting a blocked call into the sentinel call.
type policyBlockArgs struct {
	Tool    string `json:"tool"`
	Owner   string `json:"owner,omitempty"`
	Profile string `json:"profile,omitempty"`
}

// PolicyBlockTool is the sentinel Eino tool executed by the ordinary
// tool-execution flow: a blocked call rewritten to this name produces the
// fixed disclosure message as its tool result, which the transcript records
// as an ordinary completed tool-call group.
type PolicyBlockTool struct{}

// Compile-time assertion that PolicyBlockTool satisfies the invokable-tool
// surface Eino's toolsNode dispatches on.
var _ tool.InvokableTool = (*PolicyBlockTool)(nil)

// NewPolicyBlockTool constructs the sentinel tool.
func NewPolicyBlockTool() *PolicyBlockTool {
	return &PolicyBlockTool{}
}

// Info describes the sentinel. The description states it is invoked
// automatically and discourages direct calls (accepted cost: the sentinel is
// advertised to the model — the design doc).
func (p *PolicyBlockTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: PolicyBlockToolName,
		Desc: "Invoked automatically when a requested tool is disallowed by the active profile. " +
			"Returns the policy-block disclosure for that tool. Do not call this tool directly.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"tool": {
				Desc:     "the disallowed tool name the model called",
				Required: true,
				Type:     schema.String,
			},
			"owner": {
				Desc:     "the policy owner that blocked the tool",
				Required: false,
				Type:     schema.String,
			},
			"profile": {
				Desc:     "the active profile name",
				Required: false,
				Type:     schema.String,
			},
		}),
	}, nil
}

// InvokableRun delivers the disclosure. When invoked with a usable {tool}
// argument (the rewritten blocked call) it renders the fixed block message;
// a direct call without one returns the fixed informational text. Either way
// the result is a normal, non-error tool result and the turn continues.
func (p *PolicyBlockTool) InvokableRun(_ context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	args := parseBlockArgs(argumentsInJSON)
	if args.Tool == "" {
		return directCallMessage, nil
	}

	return BlockMessage(args.Tool, args.Owner, args.Profile), nil
}

// parseBlockArgs extracts the {tool, owner, profile} arguments the
// normalising wrappers write. An unparseable body or a missing {tool}
// argument degrades to the zero value (empty tool), which the caller maps
// to the direct-call informational text — the sentinel never fails the turn.
func parseBlockArgs(argumentsInJSON string) policyBlockArgs {
	args := policyBlockArgs{}
	if strings.TrimSpace(argumentsInJSON) == "" {
		return args
	}

	_ = json.Unmarshal([]byte(argumentsInJSON), &args)

	return args
}

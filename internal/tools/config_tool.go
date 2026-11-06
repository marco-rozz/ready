package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/marco-rozz/ready/internal/config"
)

// jsTypeString is the JSON-schema primitive type used by the arg schema.
const jsTypeString = "string"

// Compile-time assertion that ConfigManagerTool implements InvokableTool.
var _ tool.InvokableTool = (*ConfigManagerTool)(nil)

// ConfigManagerToolName is the tool's wire name under the builtin_tools owner;
// it is owned by the config package so the synthesized profile policies and
// the tool always agree.
const ConfigManagerToolName = config.ConfigManagerToolKey

// ConfigManagerTool exposes exactly four actions — read, update, save and
// discard — over the elected configuration's single staging buffer.
type ConfigManagerTool struct {
	mgr *ConfigManager
}

// NewConfigManagerTool wraps an initialized manager as an Eino tool.
func NewConfigManagerTool(mgr *ConfigManager) *ConfigManagerTool {
	return &ConfigManagerTool{mgr: mgr}
}

// configManagerArgs is the tool's single argument object.
type configManagerArgs struct {
	Action           string     `json:"action"`
	StageID          string     `json:"stage_id"`
	ExpectedRevision *int       `json:"expected_revision"`
	Edits            []editPair `json:"edits"`
}

// Info returns the tool metadata for the LLM. The description carries the
// procedural guidance (the ready-guide assisted flow) and is written to hold
// without the skill loaded.
func (t *ConfigManagerTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	desc := "Edit the daemon's elected configuration file safely. " +
		"CRITICAL privacy rules: NEVER ask anyone for a secret value (device tokens, API keys, shared secrets) in chat, and never write a secret value into the file — the file holds only references. " +
		"A sensitive field (api_key, tokens, client_secret, every MCP header value) takes `!secret <name>` (a Secret kept in the agent's encrypted store) or `!secret_file /absolute/path`; write the reference with a descriptive lowercase name (e.g. `api_key: !secret openai_api_key`) and tell the operator the value is entered in the TUI secret editor (opens automatically at the end of the turn) or with `ready secrets set <name>`. A literal in a sensitive field is refused. " +
		"agent.id is immutable. " +
		"Recommended flow (see the ready-guide skill): 1) read to open staging (returns the text, a stage_id and a revision); 2) discuss with the operator what to change; 3) update with a list of exact old_text/new_text edits (each old_text must match the staged text exactly once); 4) show the operator the resulting diff and ask for explicit confirmation; 5) only after the operator confirms, save — this writes the new file and activates it automatically at the end of the turn (on the loopback ZMQ TUI: RED secret editor for pending secrets -> in-TUI OAuth login -> in-process runtime reload; on other channels: immediate in-process runtime reload at turn end, with any pending secrets or OAuth logins completed later in the local TUI or via `ready secrets set` / `ready login`; no manual daemon restart is needed). " +
		"Foreign sessions cannot see or change another session's staging and will be refused. " +
		"If a save reports the editing base changed externally, discard the staging, read again and reapply the operator's intent. " +
		"Never edit any other configuration file: this tool refuses paths — you edit the elected configuration only."
	params := configManagerSchema()

	return &schema.ToolInfo{
		Name:        ConfigManagerToolName,
		Desc:        desc,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(params),
	}, nil
}

// configManagerSchema mirrors the arg shape: action, stage_id,
// expected_revision, edits.
func configManagerSchema() *jsonschema.Schema {
	editProps := orderedmap.New[string, *jsonschema.Schema]()
	editProps.Set("old_text", &jsonschema.Schema{Type: jsTypeString, Description: "Exact text to replace; must match the staged text exactly once"})
	editProps.Set("new_text", &jsonschema.Schema{Type: jsTypeString, Description: "Replacement text (may be empty)"})
	editSchema := &jsonschema.Schema{
		Type:                 "object",
		Description:          "One exact-match replacement, applied in order",
		Properties:           editProps,
		Required:             []string{"old_text", "new_text"},
		AdditionalProperties: schemaFalse(),
	}

	action := &jsonschema.Schema{
		Type:        "string",
		Enum:        enumValues("read", "update", "save", "discard"),
		Description: "read opens/returns staging; update applies an edit batch; save validates and commits (or records no changes); discard releases staging without writing",
	}
	stageID := &jsonschema.Schema{Type: jsTypeString, Description: "Current staging identifier from the last read/update result"}
	expectedRevision := &jsonschema.Schema{Type: "integer", Description: "Current revision from the last read/update result; refused when stale"}

	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set("action", action)
	props.Set("stage_id", stageID)
	props.Set("expected_revision", expectedRevision)
	props.Set("edits", &jsonschema.Schema{
		Type:        "array",
		Description: "Required for update: the ordered exact-match edit batch",
		Items:       editSchema,
	})

	return &jsonschema.Schema{
		Type:        "object",
		Description: "config-manager argument object",
		Properties:  props,
		Required:    []string{"action"},
	}
}

func schemaFalse() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Enum: []any{false}, Const: false}
}

func enumValues(values ...string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}

	return out
}

// InvokableRun dispatches the four actions. Every identity/ownership/revision
// check runs inside the manager's mutex before effects; invalid arguments
// fail before any state change.
func (t *ConfigManagerTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args configManagerArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("%w: arguments do not parse as the expected object (action, stage_id, expected_revision, edits)", ErrConfigInvalidEdit)
	}

	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "read":
		if args.StageID != "" || args.ExpectedRevision != nil || len(args.Edits) > 0 {
			return "", fmt.Errorf("%w: read takes no arguments — call it with action:'read' only", ErrConfigInvalidEdit)
		}

		return t.mgr.Read(ctx)
	case "update":
		if len(args.Edits) == 0 {
			return "", fmt.Errorf("%w: update requires a non-empty edits array", ErrConfigInvalidEdit)
		}
		if args.ExpectedRevision == nil {
			return "", fmt.Errorf("%w: update requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
		}

		return t.mgr.Update(ctx, args.StageID, *args.ExpectedRevision, args.Edits)
	case "save":
		if len(args.Edits) > 0 {
			return "", fmt.Errorf("%w: save takes no edits — commit the current staged state", ErrConfigInvalidEdit)
		}
		if args.ExpectedRevision == nil {
			return "", fmt.Errorf("%w: save requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
		}

		return t.mgr.Save(ctx, args.StageID, *args.ExpectedRevision)
	case "discard":
		if args.ExpectedRevision == nil {
			return "", fmt.Errorf("%w: discard requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
		}

		return t.mgr.Discard(ctx, args.StageID, *args.ExpectedRevision)
	case "":
		return "", fmt.Errorf("%w: the action field is required (read|update|save|discard)", ErrConfigInvalidEdit)
	default:
		return "", fmt.Errorf("%w: unknown action %q — supported: read, update, save, discard", ErrConfigInvalidEdit, args.Action)
	}
}

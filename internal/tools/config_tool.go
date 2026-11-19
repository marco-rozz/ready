package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/errdigest"
)

// jsTypeString is the JSON-schema primitive type used by the arg schema.
const (
	jsTypeString    = "string"
	configActionKey = "action"
)

// Compile-time assertion that ConfigManagerTool implements InvokableTool.
var _ tool.InvokableTool = (*ConfigManagerTool)(nil)

// ConfigManagerToolName is the tool's wire name under the builtin_tools owner;
// it is owned by the config package so the synthesized profile policies and
// the tool always agree.
const ConfigManagerToolName = config.ConfigManagerToolKey

// ConfigManagerTool exposes saved-file inspection and Session-owned drafts.
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
		"Recommended flow (see the ready-guide skill): read inspects the saved file without opening a draft; begin opens your Session's draft and returns stage_id and revision; inspect returns your draft. Discuss the changes, update with exact old_text/new_text edits, show the diff and request explicit confirmation before save. A save activates automatically at the end of the turn; use the loopback TUI or `ready secrets set` / `ready login` for pending credentials. " +
		"Foreign sessions cannot see or change another session's staging and will be refused. " +
		"If save reports a source conflict, retain the draft and use reconcile with its stage_id and revision. Show its complete diff and ask the user to send the exact /confirm-config command returned by the tool; a model assertion is not approval. If reconciliation reports overlap, ask the user for direction. " +
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
		Enum:        enumValues("read", "begin", "inspect", "update", "reconcile", "save", "discard"),
		Description: "read inspects saved configuration; begin opens a private draft; inspect returns it; update edits; reconcile prepares a review after conflict; save commits; discard releases it",
	}
	stageID := &jsonschema.Schema{Type: jsTypeString, Description: "Current draft identifier from begin or inspect"}
	expectedRevision := &jsonschema.Schema{Type: "integer", Description: "Current revision from begin, inspect or update; refused when stale"}

	props := orderedmap.New[string, *jsonschema.Schema]()
	props.Set(configActionKey, action)
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
		Required:    []string{configActionKey},
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

// InvokableRun dispatches the actions. Every identity/ownership/revision
// check runs inside the manager's mutex before effects; invalid arguments
// fail before any state change.
func (t *ConfigManagerTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args configManagerArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return "", fmt.Errorf("%w: arguments do not parse as the expected object (action, stage_id, expected_revision, edits)", ErrConfigInvalidEdit)
	}

	args.Action = strings.ToLower(strings.TrimSpace(args.Action))
	switch args.Action {
	case "read":
		if args.StageID != "" || args.ExpectedRevision != nil || len(args.Edits) > 0 {
			return "", fmt.Errorf("%w: read takes no arguments — call it with action:'read' only", ErrConfigInvalidEdit)
		}

		return t.mgr.ReadSaved(ctx)
	case "begin", "inspect":
		if args.StageID != "" || args.ExpectedRevision != nil || len(args.Edits) > 0 {
			return "", fmt.Errorf("%w: %s takes no additional arguments", ErrConfigInvalidEdit, args.Action)
		}
		mgr, err := t.mgr.sessionManager(ctx, args.Action == "begin")
		if err != nil {
			return "", err
		}
		if args.Action == "inspect" {
			return mgr.InspectDraft(ctx)
		}
		result, err := mgr.Read(ctx)
		if err == nil {
			// A newly available draft specifically repairs an earlier inspect
			// refusal for the argument-free inspect action.
			errdigest.RecoverInvocation(ctx, ConfigManagerToolName, `{"action":"inspect"}`)
		}

		return result, err
	case "update":
		return t.correctableUpdate(ctx, args)
	case "save":
		if len(args.Edits) > 0 {
			return "", fmt.Errorf("%w: save takes no edits — commit the current staged state", ErrConfigInvalidEdit)
		}
		if args.ExpectedRevision == nil {
			return "", fmt.Errorf("%w: save requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
		}

		mgr, err := t.mgr.sessionManager(ctx, false)
		if err != nil {
			return "", err
		}

		return mgr.Save(ctx, args.StageID, *args.ExpectedRevision)
	case "reconcile":
		if args.ExpectedRevision == nil || len(args.Edits) != 0 {
			return "", fmt.Errorf("%w: reconcile requires stage_id and expected_revision and takes no edits", ErrConfigInvalidEdit)
		}
		mgr, err := t.mgr.sessionManager(ctx, false)
		if err != nil {
			return "", err
		}

		return mgr.Reconcile(ctx, args.StageID, *args.ExpectedRevision)
	case "discard":
		if args.ExpectedRevision == nil {
			return "", fmt.Errorf("%w: discard requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
		}

		mgr, err := t.mgr.sessionManager(ctx, false)
		if err != nil {
			return "", err
		}

		return mgr.Discard(ctx, args.StageID, *args.ExpectedRevision)
	case "":
		return "", fmt.Errorf("%w: the action field is required (read|begin|inspect|update|reconcile|save|discard)", ErrConfigInvalidEdit)
	default:
		return "", fmt.Errorf("%w: unknown action %q — supported: read, begin, inspect, update, reconcile, save, discard", ErrConfigInvalidEdit, args.Action)
	}
}

func (t *ConfigManagerTool) correctableUpdate(ctx context.Context, args configManagerArgs) (result string, err error) {
	if err = errdigest.BeginConfigCorrection(ctx); err != nil {
		return "", err
	}
	defer func() {
		if guardErr := errdigest.EndConfigCorrection(ctx, errors.Is(err, ErrConfigExactMatch), err == nil); guardErr != nil {
			result, err = "", guardErr
		}
	}()

	return t.update(ctx, args)
}

func (t *ConfigManagerTool) update(ctx context.Context, args configManagerArgs) (string, error) {
	if len(args.Edits) == 0 {
		return "", fmt.Errorf("%w: update requires a non-empty edits array", ErrConfigInvalidEdit)
	}
	if args.ExpectedRevision == nil {
		return "", fmt.Errorf("%w: update requires the current expected_revision from the staged view", ErrConfigInvalidEdit)
	}
	mgr, err := t.mgr.sessionManager(ctx, false)
	if err != nil {
		return "", err
	}

	return mgr.Update(ctx, args.StageID, *args.ExpectedRevision, args.Edits)
}

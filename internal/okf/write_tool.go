package okf

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// WriteToolName is the builtin tool name registered under builtin_tools.
const WriteToolName = "okf_write"

// Compile-time assertion that WriteTool implements InvokableTool.
var _ tool.InvokableTool = (*WriteTool)(nil)

// WriteToolOptions configures the tool.
type WriteToolOptions struct {
	// Writer is the shared writer (also used by the /verify handler).
	Writer *Writer
	// ModelFrom returns the "<provider>::<model>" reference of the model
	// attempt that emitted the call, "" when unavailable.
	ModelFrom func(ctx context.Context) string
}

// WriteTool is the okf_write builtin tool. It never verifies, deletes,
// renames or moves anything.
type WriteTool struct {
	w         *Writer
	modelFrom func(ctx context.Context) string
}

// NewWriteTool builds the okf_write tool.
func NewWriteTool(o WriteToolOptions) *WriteTool {
	return &WriteTool{w: o.Writer, modelFrom: o.ModelFrom}
}

type writeArgs struct {
	Action            string          `json:"action"`
	Bundle            string          `json:"bundle"`
	Path              string          `json:"path"`
	Concept           string          `json:"concept"`
	Fields            json.RawMessage `json:"fields"`
	Set               json.RawMessage `json:"set"`
	Unset             []string        `json:"unset"`
	Body              *string         `json:"body"`
	BaseDigest        string          `json:"base_digest"`
	SupersededBy      string          `json:"superseded_by"`
	Reason            string          `json:"reason"`
	BundleTitle       string          `json:"bundle_title"`
	BundleDescription string          `json:"bundle_description"`
}

// Info returns the tool metadata for the model.
func (t *WriteTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	str := func(desc string) *schema.ParameterInfo { return &schema.ParameterInfo{Type: schema.String, Desc: desc} }

	return &schema.ToolInfo{
		Name: WriteToolName,
		Desc: "Author Open Knowledge Format (OKF v0.3) concepts in the agent's bundles under okfs/. okf_query never writes; " +
			"okf_write never verifies: it stamps authorship (generated) itself, refuses generated and verified fields, and " +
			"human verification is recorded only when the person runs the /verify command (tell them to; you cannot). " +
			"Actions: create (bundle, path, fields incl. a non-blank string type, body; optional bundle_title/bundle_description " +
			"for a bundle without a root index.md); update (bundle, concept, base_digest from okf_query read, and set/unset/body; " +
			"set replaces a key's whole value, null unsets it); deprecate (bundle, concept, base_digest, optional superseded_by " +
			"and reason). Every change to a value re-stamps generated, so a human-reviewed concept drops to unverified and the " +
			"receipt says so. index.md and log.md are maintained by the tool; never edit them. It never deletes, renames or moves files. " +
			"A conflict means the file changed since you read it: read it again.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"action": {
				Type: schema.String, Required: true, Desc: "What to do.",
				Enum: []string{WriteCreate, WriteUpdate, WriteDeprecate},
			},
			"bundle":  {Type: schema.String, Required: true, Desc: "Bundle name (a directory name from okf_query list)."},
			"path":    str("create: the new concept path, e.g. metrics/net-revenue (lowercase letters, digits, '.', '_' and '-'; '.md' is appended)."),
			"concept": str("update/deprecate: the existing concept id or bundle-relative path."),
			"fields": {
				Type: schema.Object,
				Desc: "create: the frontmatter mapping, e.g. {\"type\": \"Metric\", \"title\": \"...\", \"description\": \"...\", \"tags\": [\"a\"]}. Not generated or verified.",
			},
			"set": {
				Type: schema.Object,
				Desc: "update: top-level frontmatter keys and their NEW whole values (a null value unsets the key). Not generated or verified.",
			},
			"unset":              {Type: schema.Array, ElemInfo: &schema.ParameterInfo{Type: schema.String}, Desc: "update: top-level frontmatter keys to remove (never type, generated, verified)."},
			"body":               str("create/update: the full Markdown body (replaces the current body on update)."),
			"base_digest":        str("update/deprecate: the digest okf_query read printed for the file you are changing."),
			"superseded_by":      str("deprecate: the replacement concept id in the same bundle; it must exist."),
			"reason":             str("deprecate: why it is deprecated; appended to the body as a **Deprecated:** paragraph."),
			"bundle_title":       str("create: title for a root index.md the tool creates when the bundle has none."),
			"bundle_description": str("create: description for a root index.md the tool creates when the bundle has none."),
		}),
	}, nil
}

func decodeObject(raw json.RawMessage, name string) (*omap, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil //nolint:nilnil // an absent object argument is not an error
	}
	v, err := parseOrdered(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidWriteArgs, name, err)
	}
	m, ok := v.(*omap)
	if !ok {
		return nil, fmt.Errorf("%w: %s must be a JSON object", ErrInvalidWriteArgs, name)
	}

	return m, nil
}

// InvokableRun runs one write action and returns the receipt.
func (t *WriteTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var a writeArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &a); err != nil {
		return "", fmt.Errorf("%w: cannot parse arguments: %w", ErrInvalidWriteArgs, err)
	}
	fields, err := decodeObject(a.Fields, "fields")
	if err != nil {
		return "", err
	}
	set, err := decodeObject(a.Set, "set")
	if err != nil {
		return "", err
	}
	req := WriteRequest{
		Action: a.Action, Bundle: a.Bundle, Fields: fields, Set: set, Unset: a.Unset, Body: a.Body,
		BaseDigest: a.BaseDigest, SupersededBy: a.SupersededBy, Reason: a.Reason,
		BundleTitle: a.BundleTitle, BundleDescription: a.BundleDescription,
	}
	if a.Action == WriteCreate {
		req.Path = a.Path
	} else {
		req.Path = a.Concept
		if req.Path == "" {
			req.Path = a.Path
		}
	}
	if t.modelFrom != nil {
		req.Model = t.modelFrom(ctx)
	}
	r, err := t.w.Write(ctx, req)
	if err != nil {
		return "", err
	}

	return r.String(), nil
}

package okf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// ToolName is the builtin tool name registered under builtin_tools.
const ToolName = "okf_query"

// Compile-time assertion that Tool implements InvokableTool.
var _ tool.InvokableTool = (*Tool)(nil)

// Options configures the tool.
type Options struct {
	// AgentDir is the agent directory holding the reserved okfs/ root.
	AgentDir string
	// Now is the injectable clock; nil means UTC wall time.
	Now func() time.Time
}

// Tool is the read-only okf_query builtin tool. It never writes, renames or
// deletes anything and never executes bundle content.
type Tool struct {
	k   *Knowledge
	now func() time.Time
}

// NewTool builds the okf_query tool.
func NewTool(o Options) *Tool {
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &Tool{k: NewKnowledge(o.AgentDir), now: now}
}

type queryArgs struct {
	Action     string   `json:"action"`
	Bundle     string   `json:"bundle"`
	Path       string   `json:"path"`
	Dir        string   `json:"dir"`
	Offset     int      `json:"offset"`
	Refs       []string `json:"refs"`
	Type       string   `json:"type"`
	Tag        string   `json:"tag"`
	Grep       string   `json:"grep"`
	Current    bool     `json:"current"`
	Stale      bool     `json:"stale"`
	Deprecated bool     `json:"deprecated"`
	Unverified bool     `json:"unverified"`
}

// Info returns the tool metadata for the model.
func (t *Tool) Info(_ context.Context) (*schema.ToolInfo, error) {
	str := func(desc string) *schema.ParameterInfo { return &schema.ParameterInfo{Type: schema.String, Desc: desc} }
	boolean := func(desc string) *schema.ParameterInfo {
		return &schema.ParameterInfo{Type: schema.Boolean, Desc: desc}
	}

	return &schema.ToolInfo{
		Name: ToolName,
		Desc: "Read-only access to the agent's Open Knowledge Format (OKF v0.3) bundles under okfs/; it never writes, " +
			"never executes anything and never reads outside the selected bundle. Actions: " +
			"list (bundles); index (tree of one bundle, optional dir); " +
			"scan (one row per concept with type, trust tier, status, freshness; filters type/tag/grep/current/stale/deprecated/unverified); " +
			"read (one file by concept id or bundle-relative path, with trust header, resolved links, path fields and citations; offset continues long files); " +
			"compare (orders 2-10 qualified refs '<bundle>/<id>' by OKF precedence: status, trust tier, freshness, recency; " +
			"you still judge whether they disagree); validate (conformance findings of one bundle). " +
			"Trust tiers and freshness are advice for you, never a reason to refuse a concept. " +
			"Concept ids in scan/index/read are relative to the bundle.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"action": {
				Type: schema.String, Required: true, Desc: "What to do.",
				Enum: []string{actionList, actionIndex, actionScan, actionRead, actionCompare, actionValidate},
			},
			"bundle":     str("Bundle name (a directory name from list); required except for list and compare."),
			"path":       str("read: concept id (e.g. tables/orders) or bundle-relative file path."),
			"dir":        str("index: restrict the tree to this bundle-relative sub-directory."),
			"offset":     {Type: schema.Integer, Desc: "read: byte offset to continue a long file (from the continuation hint)."},
			"refs":       {Type: schema.Array, Desc: "compare: 2 to 10 qualified concept references '<bundle>/<id>'.", ElemInfo: &schema.ParameterInfo{Type: schema.String}},
			"type":       str("scan: only concepts of this type (case-insensitive)."),
			"tag":        str("scan: only concepts with this tag (case-insensitive)."),
			"grep":       str("scan: only concepts whose id, title or file text contains this text (case-insensitive)."),
			"current":    boolean("scan: only current concepts (stable or draft)."),
			"stale":      boolean("scan: only stale concepts."),
			"deprecated": boolean("scan: only deprecated concepts."),
			"unverified": boolean("scan: only unverified concepts."),
		}),
	}, nil
}

// InvokableRun dispatches one action and returns compact text.
func (t *Tool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var a queryArgs
	if err := json.Unmarshal([]byte(argumentsInJSON), &a); err != nil {
		return "", fmt.Errorf("%w: cannot parse arguments: %w", ErrInvalidReference, err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("okf_query: %w", err)
	}
	now := t.now().UTC()
	switch a.Action {
	case actionList:
		return t.list(ctx)
	case actionCompare:
		return t.compare(ctx, a, now)
	case actionIndex, actionScan, actionRead, actionValidate:
		b, err := t.k.Open(a.Bundle)
		if err != nil {
			return "", err
		}
		defer b.Close()
		switch a.Action {
		case actionIndex:
			return t.index(ctx, b, a, now)
		case actionScan:
			return t.scan(ctx, b, a, now)
		case actionRead:
			return t.read(ctx, b, a, now)
		default:
			return t.validate(ctx, b, now)
		}
	}

	return "", fmt.Errorf("%w %q (expected list, index, scan, read, compare or validate)", ErrUnknownAction, a.Action)
}

func (t *Tool) list(ctx context.Context) (string, error) {
	infos, skipped, err := t.k.Discover(ctx)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Knowledge bundles in okfs/: %d\n", len(infos))
	for _, i := range infos {
		v := i.Version
		if v == "" {
			v = "not declared"
		}
		fmt.Fprintf(&sb, "%s  okf_version %s\n", i.Name, v)
		if i.Title != "" {
			fmt.Fprintf(&sb, "    title: %s\n", i.Title)
		}
		if i.Description != "" {
			fmt.Fprintf(&sb, "    description: %s\n", i.Description)
		}
		for _, n := range i.Notes {
			fmt.Fprintf(&sb, "    note: %s\n", n)
		}
	}
	for _, s := range skipped {
		fmt.Fprintf(&sb, "Skipped: %s (%s)\n", s.Name, s.Reason)
	}

	return sb.String(), nil
}

func (t *Tool) index(ctx context.Context, b *Bundle, a queryArgs, now time.Time) (string, error) {
	res, err := b.Index(ctx, a.Dir, now)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	scope := b.Name
	if a.Dir != "" {
		scope += "/" + a.Dir
	}
	fmt.Fprintf(&sb, "Index of %s: %d concepts\n", scope, res.Concepts)
	shown := res.Entries
	if len(shown) > MaxIndexEntries {
		shown = shown[:MaxIndexEntries]
	}
	for _, e := range shown {
		if e.Dir {
			fmt.Fprintf(&sb, "%s/\n", e.Path)

			continue
		}
		fmt.Fprintf(&sb, "  %s  [%s]  %s\n", e.Path, e.Type, e.Title)
	}
	if len(res.Entries) > MaxIndexEntries {
		fmt.Fprintf(&sb, "Showing %d of %d entries; pass dir=<sub-directory> to narrow the tree.\n", MaxIndexEntries, len(res.Entries))
	}
	if res.Truncated {
		fmt.Fprintf(&sb, "%s: walk truncated at the per-call cap; pass dir=<sub-directory> to cover the rest.\n", CodeTruncated)
	}

	return sb.String(), nil
}

func (t *Tool) scan(ctx context.Context, b *Bundle, a queryArgs, now time.Time) (string, error) {
	res, err := b.Scan(ctx, ScanOptions{Type: a.Type, Tag: a.Tag, Grep: a.Grep, Current: a.Current, Stale: a.Stale, Deprecated: a.Deprecated, Unverified: a.Unverified}, now)
	if err != nil {
		return "", err
	}
	s := res.Summary
	v := res.Bundle.Version
	if v == "" {
		v = "not declared"
	}
	name := res.Bundle.Title
	if name == "" {
		name = b.Name
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Bundle %s (%s): %d concepts, okf_version %s, now %s\n", name, b.Name, s.Concepts, v, res.Now.Format(timeFormat))
	if res.Bundle.Description != "" {
		fmt.Fprintf(&sb, "Scope: %s\n", res.Bundle.Description)
	}
	for _, n := range res.Bundle.Notes {
		fmt.Fprintf(&sb, "Note: %s\n", n)
	}
	types := make([]string, len(s.Types))
	for i, tc := range s.Types {
		types[i] = fmt.Sprintf("%s %d", tc.Type, tc.Count)
	}
	fmt.Fprintf(&sb, "Types: %s\n", strings.Join(types, ", "))
	fmt.Fprintf(&sb, "Trust: human-reviewed %d, machine-confirmed %d, unverified %d. Stale %d, deprecated %d, draft %d, not current %d.\n",
		s.Human, s.Machine, s.Unverified, s.Stale, s.Deprecated, s.Draft, s.NotCurrent)
	if len(res.NotConcepts) > 0 {
		const maxShown = 20
		parts := make([]string, 0, maxShown)
		for i, n := range res.NotConcepts {
			if i == maxShown {
				parts = append(parts, fmt.Sprintf("+%d more", len(res.NotConcepts)-maxShown))

				break
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", n.Path, n.Reason))
		}
		fmt.Fprintf(&sb, "Skipped %d Markdown files that are not concepts: %s\n", len(res.NotConcepts), strings.Join(parts, ", "))
	}
	if s.Selected != s.Concepts {
		fmt.Fprintf(&sb, "Showing %d concepts that match the filters.\n", s.Selected)
	}
	rows := res.Rows
	if len(rows) > MaxScanRows {
		rows = rows[:MaxScanRows]
		fmt.Fprintf(&sb, "Showing the first %d of %d rows; narrow with type, tag, grep, current, stale, deprecated or unverified.\n", MaxScanRows, len(res.Rows))
	}
	if res.Truncated {
		fmt.Fprintf(&sb, "%s: walk truncated at the per-call cap (%d files / %d MiB); results are incomplete, narrow the bundle.\n", CodeTruncated, MaxWalkFiles, MaxWalkBytes>>20)
	}
	sb.WriteByte('\n')
	for _, c := range rows {
		fmt.Fprintf(&sb, "%s  [%s]  %s, %s, %s", c.ID, c.Type, c.Tier, c.Status, c.Freshness())
		if c.Status == statusDeprecated && c.SupersededBy != "" {
			rep := c.SupersededBy
			if c.Replacement != "" {
				rep = c.Replacement
			}
			fmt.Fprintf(&sb, ", superseded by %s", rep)
		}
		sb.WriteByte('\n')
		line := "    " + c.Title
		if c.Desc != "" {
			line += ": " + c.Desc
		}
		sb.WriteString(line + "\n")
		for _, n := range c.Notes {
			fmt.Fprintf(&sb, "    note: %s\n", n)
		}
	}

	return sb.String(), nil
}

func statusLine(c *Concept) string {
	switch c.Status {
	case statusStable:
		return "stable"
	case statusDraft:
		return "draft (not reviewed, possibly incomplete)"
	case statusDeprecated:
		rep := "none declared"
		switch {
		case c.Replacement != "":
			rep = c.Bundle + "/" + c.Replacement
		case c.SupersededBy != "":
			rep = c.SupersededBy + " (does not resolve)"
		}

		return "deprecated, not current; replacement: " + rep
	}

	return c.Status + " (unknown status, not current)"
}

func (t *Tool) read(ctx context.Context, b *Bundle, a queryArgs, now time.Time) (string, error) {
	res, err := b.Read(ctx, a.Path, a.Offset, now)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if c := res.Concept; c != nil {
		fmt.Fprintf(&sb, "%s  [%s]\n", c.Qualified(), c.Type)
		fmt.Fprintf(&sb, "Trust: %s\n", c.Tier)
		for _, n := range c.Notes {
			fmt.Fprintf(&sb, "  note: %s\n", n)
		}
		fmt.Fprintf(&sb, "Status: %s\n", statusLine(c))
		fmt.Fprintf(&sb, "Freshness: %s\n", c.Freshness())
		changed := "unknown"
		if c.ChangeRaw != "" {
			changed = c.ChangeRaw
			if c.ChangeActor != "" {
				changed += " by " + c.ChangeActor
			}
		} else if c.ChangeActor != "" {
			changed = "by " + c.ChangeActor
		}
		fmt.Fprintf(&sb, "Changed: %s\n", changed)
	} else {
		fmt.Fprintf(&sb, "%s/%s\n", b.Name, res.Path)
	}
	fmt.Fprintf(&sb, "Digest: %s\n", res.Digest)
	sb.WriteString("===== content =====\n")
	sb.WriteString(res.Content)
	if !strings.HasSuffix(res.Content, "\n") {
		sb.WriteByte('\n')
	}
	sb.WriteString("===== end of content =====\n")
	if res.NextOffset > 0 {
		fmt.Fprintf(&sb, "Content truncated at %d of %d bytes; continue with offset=%d.\n", res.NextOffset, res.Total, res.NextOffset)
	}
	if len(res.Links) > 0 {
		sb.WriteString("Links:\n")
		for _, l := range res.Links {
			fmt.Fprintf(&sb, "  line %d: %s %s\n", l.Line, l.Target, l.Res)
		}
	}
	if len(res.Fields) > 0 {
		sb.WriteString("Path fields:\n")
		for _, f := range res.Fields {
			if f.Descriptor {
				fmt.Fprintf(&sb, "  %s: %s (scope descriptor, not a path)\n", f.Name, f.Value)

				continue
			}
			fmt.Fprintf(&sb, "  %s: %s %s\n", f.Name, f.Value, f.Res)
		}
	}
	if len(res.Citations) > 0 {
		sb.WriteString("Citations:\n")
		for _, c := range res.Citations {
			if !c.Found {
				fmt.Fprintf(&sb, "  [^%s] no sources entry with this id\n", c.Label)

				continue
			}
			title := c.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&sb, "  [^%s] %s: %s\n", c.Label, title, c.Resource)
		}
	}
	if ci := res.Computation; ci != nil {
		renderComputation(&sb, ci)
	}

	return sb.String(), nil
}

func renderComputation(sb *strings.Builder, ci *ComputationInfo) {
	sb.WriteString("Attested Computation (reported, never executed):\n")
	rt := ci.Runtime
	if rt == "" {
		rt = "absent"
	}
	fmt.Fprintf(sb, "  runtime: %s\n", rt)
	if len(ci.Params) == 0 {
		sb.WriteString("  parameters: none\n")
	}
	for _, p := range ci.Params {
		fmt.Fprintf(sb, "  parameter: %s (%s, required: %t)\n", p.Name, p.Type, p.Required)
	}
	switch {
	case ci.Location == "":
		sb.WriteString("  computation: missing\n")
	case ci.LocationMissing:
		fmt.Fprintf(sb, "  computation: %s (file does not exist)\n", ci.Location)
	default:
		fmt.Fprintf(sb, "  computation: %s\n", ci.Location)
	}
	if ci.Block != "" {
		fmt.Fprintf(sb, "```%s\n%s\n```\n", ci.BlockInfo, ci.Block)
	}
	if ci.ExecutorResource != "" {
		fmt.Fprintf(sb, "  executor.resource: %s\n", ci.ExecutorResource)
		fmt.Fprintf(sb, "  executor.receipt: %s\n", strings.Join(ci.Receipt, ", "))
	} else {
		sb.WriteString("  executor: absent\n")
	}
	if ci.HasAttester {
		fmt.Fprintf(sb, "  attester: present (%s)\n", ci.AttesterResource)
	} else {
		sb.WriteString("  attester: absent\n")
	}
	for _, n := range ci.Notes {
		fmt.Fprintf(sb, "  note: %s\n", n)
	}
	fmt.Fprintf(sb, "  %s\n", NoExecutionGuidance)
}

func (t *Tool) compare(ctx context.Context, a queryArgs, now time.Time) (string, error) {
	if len(a.Refs) < 2 || len(a.Refs) > MaxCompareRefs {
		return "", fmt.Errorf("%w: compare takes 2 to %d qualified references '<bundle>/<id>', got %d", ErrInvalidReference, MaxCompareRefs, len(a.Refs))
	}
	bundles := map[string]*Bundle{}
	defer func() {
		for _, b := range bundles {
			b.Close()
		}
	}()
	concepts := make([]*Concept, 0, len(a.Refs))
	for _, ref := range a.Refs {
		name, id, ok := strings.Cut(ref, "/")
		if !ok || name == "" || id == "" {
			return "", fmt.Errorf("%w %q: expected '<bundle>/<id>'", ErrInvalidReference, ref)
		}
		if err := ValidRelPath(id); err != nil {
			return "", err
		}
		b := bundles[name]
		if b == nil {
			var err error
			if b, err = t.k.Open(name); err != nil {
				return "", err
			}
			bundles[name] = b
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("okf_query: %w", err)
		}
		c, err := b.LoadConcept(id, now)
		if err != nil {
			return "", err
		}
		concepts = append(concepts, c)
	}
	res := Compare(concepts)
	var sb strings.Builder
	sb.WriteString("Ordered by OKF precedence (first difference decides: status, trust tier, freshness, recency).\n")
	sb.WriteString("This orders candidates only; whether they actually disagree is your judgment.\n")
	for i, c := range res.Ordered {
		change := "no change time"
		if c.ChangeRaw != "" && c.ChangeTime != nil {
			change = "changed " + c.ChangeRaw
		}
		fmt.Fprintf(&sb, "%d. %s  [%s]  %s, %s, %s, %s\n", i+1, c.Qualified(), c.Type, c.Tier, c.Status, c.Freshness(), change)
		if i < len(res.Steps) {
			if res.Steps[i] == StepUndecided {
				fmt.Fprintf(&sb, "   vs #%d: undecided — present both answers with their supporting concepts\n", i+2)
			} else {
				fmt.Fprintf(&sb, "   vs #%d: decided by %s\n", i+2, res.Steps[i])
			}
		}
	}

	return sb.String(), nil
}

func (t *Tool) validate(ctx context.Context, b *Bundle, now time.Time) (string, error) {
	res, err := b.Validate(ctx, now)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	verdict := "conformant"
	if !res.Conformant() {
		verdict = "not conformant"
	}
	fmt.Fprintf(&sb, "Bundle %s: %d files checked: %d errors, %d warnings, %d info. The bundle is %s. now %s\n",
		b.Name, res.FilesChecked, res.Errors, res.Warnings, res.Infos, verdict, res.Now.Format(timeFormat))
	shown := res.Findings
	if len(shown) > MaxFindings {
		shown = shown[:MaxFindings]
	}
	for _, f := range shown {
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(&sb, "%s: %s %s %s\n", loc, f.Level, f.Code, f.Message)
	}
	if len(res.Findings) > MaxFindings {
		fmt.Fprintf(&sb, "Showing %d of %d findings; fix the listed ones and validate again, or read the affected files to see the rest.\n", MaxFindings, len(res.Findings))
	}

	return sb.String(), nil
}

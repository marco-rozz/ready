package okf

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ScanOptions are the scan filters; they combine with AND.
type ScanOptions struct {
	Type       string // case-insensitive exact
	Tag        string // case-insensitive membership
	Grep       string // case-insensitive substring of id, title or whole file text
	Current    bool
	Stale      bool
	Deprecated bool
	Unverified bool
}

// NotConcept is a Markdown file that is not a concept, with its reason.
type NotConcept struct {
	Path   string
	Reason string
}

// TypeCount is one entry of the per-type summary.
type TypeCount struct {
	Type  string
	Count int
}

// ScanSummary is the whole-bundle summary (independent of the filters).
type ScanSummary struct {
	Concepts    int
	Selected    int
	Types       []TypeCount
	Human       int
	Machine     int
	Unverified  int
	Stale       int
	Deprecated  int
	Draft       int
	NotCurrent  int
	NotConcepts int
}

// ScanResult is the result of Scan. Rows holds every selected concept; the
// caller applies the output bound.
type ScanResult struct {
	Bundle      BundleInfo
	Now         time.Time
	Summary     ScanSummary
	Rows        []*Concept
	NotConcepts []NotConcept
	Truncated   bool
}

func isReserved(rel string) bool {
	base := path.Base(rel)

	return base == fileIndex || base == fileLog
}

func issueReason(err error) (code, reason string, ok bool) {
	var ri *readIssue
	if errors.As(err, &ri) {
		return ri.Code, ri.Reason, true
	}

	return "", "", false
}

// Scan lists the concepts of the bundle with trust, status and freshness.
func (b *Bundle) Scan(ctx context.Context, opts ScanOptions, now time.Time) (*ScanResult, error) {
	g := &budget{}
	files, err := b.walkMarkdown(ctx, "", g)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{Bundle: b.Info(), Now: now}
	var all []*Concept
	for _, f := range files {
		if isReserved(f.Rel) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("scanning bundle: %w", err)
		}
		if !g.allowFile() {
			break
		}
		text, err := b.readText(f.Rel, g)
		if err != nil {
			code, reason, ok := issueReason(err)
			if !ok {
				return nil, fmt.Errorf("%w: %w", ErrIO, err)
			}
			res.NotConcepts = append(res.NotConcepts, NotConcept{Path: f.Rel, Reason: "skipped: " + reason + " (" + code + ")"})

			continue
		}
		c, reason := b.conceptFromText(f.Rel, text, now)
		if c == nil {
			res.NotConcepts = append(res.NotConcepts, NotConcept{Path: f.Rel, Reason: reason})

			continue
		}
		all = append(all, c)
	}
	res.Truncated = g.truncated

	idx := map[string]int{}
	for _, c := range all {
		if _, ok := idx[c.Type]; !ok {
			idx[c.Type] = len(res.Summary.Types)
			res.Summary.Types = append(res.Summary.Types, TypeCount{Type: c.Type})
		}
		res.Summary.Types[idx[c.Type]].Count++
		switch c.Tier {
		case TierHuman:
			res.Summary.Human++
		case TierMachine:
			res.Summary.Machine++
		default:
			res.Summary.Unverified++
		}
		if c.Stale {
			res.Summary.Stale++
		}
		if c.Status == statusDeprecated {
			res.Summary.Deprecated++
		}
		if c.Status == statusDraft {
			res.Summary.Draft++
		}
		if !c.Current {
			res.Summary.NotCurrent++
		}
	}
	sort.SliceStable(res.Summary.Types, func(i, j int) bool { return res.Summary.Types[i].Count > res.Summary.Types[j].Count })
	res.Summary.Concepts = len(all)
	res.Summary.NotConcepts = len(res.NotConcepts)
	for _, c := range all {
		if matches(c, opts) {
			res.Rows = append(res.Rows, c)
		}
	}
	res.Summary.Selected = len(res.Rows)

	return res, nil
}

func matches(c *Concept, o ScanOptions) bool {
	if o.Type != "" && !strings.EqualFold(c.Type, o.Type) {
		return false
	}
	if o.Tag != "" {
		found := false
		for _, t := range c.Tags {
			if strings.EqualFold(t, o.Tag) {
				found = true

				break
			}
		}
		if !found {
			return false
		}
	}
	if o.Grep != "" {
		hay := strings.ToLower(c.ID + " " + c.Title + " " + c.Text)
		if !strings.Contains(hay, strings.ToLower(o.Grep)) {
			return false
		}
	}
	if o.Current && !c.Current {
		return false
	}
	if o.Stale && !c.Stale {
		return false
	}
	if o.Deprecated && c.Status != "deprecated" {
		return false
	}

	return !o.Unverified || c.Tier == TierNone
}

// IndexEntry is one line of the computed bundle tree.
type IndexEntry struct {
	Dir   bool
	Path  string // directory path (Dir) or concept id
	Type  string
	Title string
}

// IndexResult is the tree of one bundle (or sub-directory).
type IndexResult struct {
	Entries   []IndexEntry
	Concepts  int
	Truncated bool
}

// Index computes the tree of concepts from the filesystem, optionally
// restricted to a sub-directory.
func (b *Bundle) Index(ctx context.Context, sub string, now time.Time) (*IndexResult, error) {
	if sub != "" {
		if err := ValidRelPath(sub); err != nil {
			return nil, err
		}
		info, err := b.root.Stat(sub)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%w: directory %q", ErrUnknownConcept, sub)
		}
	}
	g := &budget{}
	files, err := b.walkMarkdown(ctx, sub, g)
	if err != nil {
		return nil, err
	}
	type row struct{ id, typ, title string }
	byDir := map[string][]row{}
	dirs := map[string]bool{}
	res := &IndexResult{Truncated: g.truncated}
	for _, f := range files {
		if isReserved(f.Rel) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("indexing bundle: %w", err)
		}
		if !g.allowFile() {
			res.Truncated = true

			break
		}
		text, err := b.readText(f.Rel, g)
		if err != nil {
			continue
		}
		c, _ := b.conceptFromText(f.Rel, text, now)
		if c == nil {
			continue
		}
		dir := path.Dir(f.Rel)
		byDir[dir] = append(byDir[dir], row{c.ID, c.Type, c.Title})
		for d := dir; d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
		res.Concepts++
	}
	all := make([]string, 0, len(dirs)+1)
	if _, ok := byDir["."]; ok {
		all = append(all, ".")
	}
	for d := range dirs {
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool {
		return lessSegments(strings.TrimPrefix(all[i], "."), strings.TrimPrefix(all[j], "."))
	})
	for _, d := range all {
		if d != "." {
			res.Entries = append(res.Entries, IndexEntry{Dir: true, Path: d})
		}
		for _, r := range byDir[d] {
			res.Entries = append(res.Entries, IndexEntry{Path: r.id, Type: r.typ, Title: r.title})
		}
	}

	return res, nil
}

// resolveFileArg maps a concept id or bundle-relative path argument to an
// existing bundle file. The argument is validated lexically before any
// filesystem access.
func (b *Bundle) resolveFileArg(arg string) (string, error) {
	if err := ValidRelPath(arg); err != nil {
		return "", err
	}
	cands := []string{arg}
	if !strings.HasSuffix(arg, ".md") {
		cands = []string{arg + ".md", arg}
	}
	for _, c := range cands {
		info, err := b.root.Stat(c)
		if err == nil {
			if !info.Mode().IsRegular() {
				continue
			}
			if ok, _ := b.exists(c); !ok {
				continue
			}

			return c, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w %q: resolves outside the bundle or is unreadable", ErrOutsideBundle, arg)
		}
	}

	return "", fmt.Errorf("%w %q in bundle %q", ErrUnknownConcept, arg, b.Name)
}

// LoadConcept loads one concept by id or relative path.
func (b *Bundle) LoadConcept(arg string, now time.Time) (*Concept, error) {
	rel, err := b.resolveFileArg(arg)
	if err != nil {
		return nil, err
	}
	text, err := b.readText(rel, nil)
	if err != nil {
		return nil, b.readErr(rel, err)
	}
	if !strings.HasSuffix(rel, ".md") || isReserved(rel) {
		return nil, fmt.Errorf("%w %q: not a concept (use read for plain files)", ErrInvalidReference, arg)
	}
	c, reason := b.conceptFromText(rel, text, now)
	if c == nil {
		return nil, fmt.Errorf("%w %q: not a concept (%s)", ErrInvalidReference, arg, reason)
	}

	return c, nil
}

func (b *Bundle) readErr(rel string, err error) error {
	if code, reason, ok := issueReason(err); ok {
		if code == CodeUnreadable {
			return fmt.Errorf("%w %q: %s (%s)", ErrOutsideBundle, rel, reason, code)
		}

		return fmt.Errorf("%w %q: %s (%s)", ErrInvalidReference, rel, reason, code)
	}

	return fmt.Errorf("%w: %w", ErrIO, err)
}

// ResolvedLink is a body link with its resolution and prose line.
type ResolvedLink struct {
	Line   int
	Target string
	Res    Resolution
}

// ResolvedField is a path field of the frontmatter with its resolution; a
// scope descriptor is reported with Descriptor set instead of a resolution.
type ResolvedField struct {
	Name       string
	Value      string
	Res        Resolution
	Descriptor bool
}

// Citation is a footnote lookup result.
type Citation struct {
	Label    string
	Found    bool
	Title    string
	Resource string
}

// ReadResult is the outcome of a read.
type ReadResult struct {
	Path       string
	Concept    *Concept // nil for plain Markdown files and text files
	Content    string
	Offset     int
	NextOffset int // 0 when the content reaches the end of the file
	Total      int
	// Digest is the content digest of the whole file's raw bytes.
	Digest      string
	Links       []ResolvedLink
	Fields      []ResolvedField
	Citations   []Citation
	Computation *ComputationInfo
}

// Read returns one file with, for a concept, the trust header data and the
// resolved links, path fields and citations.
func (b *Bundle) Read(ctx context.Context, arg string, offset int, now time.Time) (*ReadResult, error) {
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset must not be negative", ErrInvalidReference)
	}
	rel, err := b.resolveFileArg(arg)
	if err != nil {
		return nil, err
	}
	raw, err := b.readRaw(rel, nil)
	if err != nil {
		return nil, b.readErr(rel, err)
	}
	text := strings.TrimLeft(string(raw), bom)
	if strings.IndexByte(text, 0) >= 0 {
		return nil, fmt.Errorf("%w %q: binary content", ErrInvalidReference, arg)
	}
	res := &ReadResult{Path: rel, Total: len(text), Offset: offset, Digest: Digest(raw)}
	res.Content, res.Offset, res.NextOffset = sliceUTF8(text, offset, MaxReadBytes)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("reading bundle file: %w", err)
	}
	if strings.HasSuffix(rel, ".md") && !isReserved(rel) {
		if c, _ := b.conceptFromText(rel, text, now); c != nil {
			res.Concept = c
			b.annotate(res)
		}
	} else if rel == fileIndex {
		b.annotateRootIndex(res, text)
	}

	return res, nil
}

// sliceUTF8 returns text[offset:] cut at max bytes on rune boundaries.
func sliceUTF8(text string, offset, limit int) (string, int, int) {
	if offset > len(text) {
		offset = len(text)
	}
	for offset < len(text) && !utf8.RuneStart(text[offset]) {
		offset++
	}
	end := offset + limit
	if end >= len(text) {
		return text[offset:], offset, 0
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}

	return text[offset:end], offset, end
}

func (b *Bundle) annotateRootIndex(res *ReadResult, text string) {
	fm := ParseFrontmatter(text)
	if lic, ok := fm.Meta["license"].(string); ok && lic != "" && !spdxRE.MatchString(lic) {
		res.Fields = append(res.Fields, ResolvedField{Name: "license", Value: lic, Res: b.resolve(fileIndex, lic, true)})
	}
}

// annotate fills links, path fields, citations and the computation report of
// a concept read.
func (b *Bundle) annotate(res *ReadResult) {
	c := res.Concept
	meta := c.Meta
	addField := func(name string, v any) {
		s, ok := v.(string)
		if !ok || s == "" {
			return
		}
		res.Fields = append(res.Fields, ResolvedField{Name: name, Value: s, Res: b.resolve(c.Path, s, true)})
	}
	addField("resource", meta["resource"])
	if c.Status == statusDeprecated || !blank(meta["superseded_by"]) {
		addField("superseded_by", meta["superseded_by"])
	}
	addField("computation", meta["computation"])
	for _, key := range []string{keyExecutor, keyAttester} {
		if m, ok := asMap(meta[key]); ok {
			addField(key+".resource", m["resource"])
		}
	}
	sourceByID := map[string]map[string]any{}
	for i, e := range asList(meta["sources"]) {
		m, ok := asMap(e)
		if !ok || blank(m["resource"]) {
			continue
		}
		if !blank(m["id"]) {
			if _, dup := sourceByID[str(m["id"])]; !dup {
				sourceByID[str(m["id"])] = m
			}
		}
		rs, isStr := m["resource"].(string)
		if !isStr {
			continue
		}
		name := fmt.Sprintf("sources[%d].resource", i)
		if containsSpace(rs) {
			res.Fields = append(res.Fields, ResolvedField{Name: name, Value: rs, Descriptor: true})

			continue
		}
		addField(name, rs)
	}
	for _, l := range bodyLinks(c.Body, c.BodyLine) {
		res.Links = append(res.Links, ResolvedLink{Line: l.Line, Target: l.Target, Res: b.resolve(c.Path, l.Target, false)})
	}
	seen := map[string]bool{}
	for _, f := range bodyFootnotes(c.Body, c.BodyLine) {
		if seen[f.Label] {
			continue
		}
		seen[f.Label] = true
		cit := Citation{Label: f.Label}
		if src, ok := sourceByID[f.Label]; ok {
			cit.Found = true
			cit.Title = str(src["title"])
			if blank(src["title"]) {
				cit.Title = ""
			}
			cit.Resource = str(src["resource"])
		}
		res.Citations = append(res.Citations, cit)
	}
	if c.Type == typeAttestedComputation {
		res.Computation = b.inspectComputation(c)
	}
}

package okf

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Finding levels.
const (
	LevelError   = "error"
	LevelWarning = "warning"
	LevelInfo    = "info"
)

var (
	actorRE = regexp.MustCompile(`^([^\s/:]+/\S+|[A-Za-z][A-Za-z0-9+.-]*:\S+)$`)
	spdxRE  = regexp.MustCompile(`^[A-Za-z0-9.+-]+( (AND|OR|WITH) [A-Za-z0-9.+-]+)*$`)
)

// isKnownKey reports whether k is a key defined by OKF v0.3 (section 4.1).
func isKnownKey(k string) bool {
	switch k {
	case keyType, "title", "description", "resource", "tags", "sources", "usage_window",
		"generated", "verified", keyStatus, keySupersededBy, "stale_after", "runtime",
		"parameters", "computation", keyExecutor, keyAttester, "timestamp":
		return true
	}

	return false
}

// Finding is one validator finding. Line is 0 when the finding has no line.
type Finding struct {
	Path    string
	Line    int
	Level   string
	Code    string
	Message string
}

// ValidateResult is the conformance report of one bundle.
type ValidateResult struct {
	Bundle       string
	Version      string
	Now          time.Time
	FilesChecked int
	Errors       int
	Warnings     int
	Infos        int
	Findings     []Finding
	Truncated    bool
}

// Conformant reports the verdict: no error findings.
func (r *ValidateResult) Conformant() bool { return r.Errors == 0 }

type checker struct {
	b        *Bundle
	now      time.Time
	findings []Finding
	files    int
}

// Validate reports the OKF v0.3 conformance findings of the bundle. Findings
// are tool output, never tool errors; per-file problems use the READY-only
// R3xx codes.
func (b *Bundle) Validate(ctx context.Context, now time.Time) (*ValidateResult, error) {
	g := &budget{}
	files, err := b.walkMarkdown(ctx, "", g)
	if err != nil {
		return nil, err
	}
	ck := &checker{b: b, now: now}
	if !b.hasIndex {
		ck.warn(".", 0, "W125", `add a bundle-root index.md with okf_version: "0.3" (section 8.1)`)
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("validating bundle: %w", err)
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
			ck.files++
			ck.warn(f.Rel, 0, code, "file skipped: "+reason)

			continue
		}
		ck.files++
		switch {
		case path.Base(f.Rel) == fileIndex:
			ck.checkIndex(f.Rel, text)
		case path.Base(f.Rel) == fileLog:
			ck.checkLog(f.Rel, text)
		default:
			ck.checkMarkdown(f.Rel, text)
		}
	}
	if g.truncated {
		ck.warn(".", 0, CodeTruncated, fmt.Sprintf("walk truncated at the per-call cap (%d files / %d bytes): findings are incomplete", MaxWalkFiles, MaxWalkBytes))
	}
	res := &ValidateResult{Bundle: b.Name, Version: b.version, Now: now, FilesChecked: ck.files, Findings: ck.findings, Truncated: g.truncated}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		return lessSegments(strings.TrimPrefix(res.Findings[i].Path, "."), strings.TrimPrefix(res.Findings[j].Path, "."))
	})
	for _, f := range res.Findings {
		switch f.Level {
		case LevelError:
			res.Errors++
		case LevelWarning:
			res.Warnings++
		default:
			res.Infos++
		}
	}

	return res, nil
}

func (c *checker) add(p string, line int, level, code, msg string) {
	c.findings = append(c.findings, Finding{Path: p, Line: line, Level: level, Code: code, Message: msg})
}

func (c *checker) warn(p string, line int, code, msg string) { c.add(p, line, LevelWarning, code, msg) }
func (c *checker) info(p, code, msg string)                  { c.add(p, 1, LevelInfo, code, msg) }
func (c *checker) fail(p string, line int, code, msg string) { c.add(p, line, LevelError, code, msg) }

// frontmatter splits the file and reports the two failures any block can have.
func (c *checker) frontmatter(p, text string) Frontmatter {
	fm := ParseFrontmatter(text)
	switch fm.State {
	case FMDuplicate:
		c.fail(p, fm.DupLine, "E007", fmt.Sprintf("frontmatter repeats the key %s; keys must be unique (section 4)", pyRepr(fm.DupKey)))
	case FMInvalid, FMNotMapping, FMUnclosed:
		c.fail(p, 1, "E002", "frontmatter does not parse as a YAML mapping: "+fm.Problem+" (section 4)")
	case FMAbsent, FMOK:
	}

	return fm
}

func (c *checker) checkIndex(p, text string) {
	if path.Dir(p) != "." {
		if startsWithFrontmatter(text) {
			c.fail(p, 1, "E004", "an index.md below the bundle root has no frontmatter (section 8)")
		}

		return
	}
	fm := c.frontmatter(p, text)
	if fm.State == FMAbsent {
		c.warn(p, 1, "W125", `give the root index.md frontmatter with okf_version: "0.3" (section 8.1)`)

		return
	}
	if fm.State != FMOK {
		return
	}
	version := fm.Meta["okf_version"]
	switch {
	case blank(version):
		c.warn(p, 1, "W125", `add okf_version: "0.3" to the root index.md (section 8.1)`)
	default:
		if _, isStr := version.(string); !isStr {
			c.warn(p, 1, "W117", fmt.Sprintf(`quote okf_version as a string, for example "0.3" (section 8.1); found %s`, str(version)))
		}
	}
	if lic, ok := fm.Meta["license"].(string); ok && !spdxRE.MatchString(lic) && c.b.resolve(p, lic, true).Kind == RefMissing {
		c.warn(p, 1, "W116", fmt.Sprintf("license %s is not an SPDX expression or a file in the bundle (section 8.1)", pyRepr(lic)))
	}
}

func (c *checker) checkLog(p, text string) {
	if startsWithFrontmatter(text) {
		c.fail(p, 1, "E006", "log.md has no frontmatter (section 9)")
	}
	type dated struct {
		n int
		d string
	}
	var dates []dated
	for i, line := range splitLines(text) {
		if !strings.HasPrefix(line, "## ") {
			continue
		}
		heading := strings.TrimSpace(line[3:])
		if dateRE.MatchString(heading) {
			dates = append(dates, dated{i + 1, heading})
		} else {
			c.fail(p, i+1, "E005", fmt.Sprintf("a log.md date heading must be YYYY-MM-DD (section 9); found %s", pyRepr(heading)))
		}
	}
	for i := 0; i+1 < len(dates); i++ {
		newer, older := dates[i], dates[i+1]
		if older.d > newer.d {
			c.warn(p, older.n, "W118", fmt.Sprintf("log.md puts the newest date first (section 9); %s follows %s", older.d, newer.d))
		}
	}
}

func (c *checker) checkMarkdown(p, text string) {
	fm := c.frontmatter(p, text)
	if fm.State == FMAbsent {
		c.warn(p, 1, "W120", "not a concept: the file has no frontmatter (section 3.2)")

		return
	}
	if fm.State != FMOK {
		return
	}
	meta := fm.Meta
	if t, ok := meta[keyType].(string); !ok || strings.TrimSpace(t) == "" {
		c.warn(p, 1, "W120", "not a concept: the frontmatter has no non-empty type (section 3.2)")

		return
	}
	c.checkStrings(p, meta)
	c.checkTags(p, meta)
	c.checkLifecycle(p, meta)
	generatedAt, hasGenAt := c.checkGenerated(p, meta)
	c.checkVerified(p, meta, generatedAt, hasGenAt)
	sourceIDs := c.checkSources(p, meta)
	c.checkStaleAfter(p, meta)
	if !blank(meta["resource"]) {
		c.checkPathField(p, "resource", meta["resource"])
	}
	c.checkBody(p, fm.Body, fm.BodyLine, sourceIDs)
	if meta[keyType] == typeAttestedComputation {
		c.checkComputation(p, meta, fm.Body)
	}
	var unknown []string
	for k := range meta {
		if !isKnownKey(k) && !strings.HasPrefix(k, "x-") {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		c.info(p, "I203", fmt.Sprintf("extension keys without the x- prefix: %s (section 4.1)", strings.Join(unknown, ", ")))
	}
}

func (c *checker) checkStrings(p string, meta map[string]any) {
	for _, key := range []string{"type", "title", "description", "status"} {
		v, ok := meta[key]
		if !ok || v == nil {
			continue
		}
		if _, isStr := v.(string); !isStr {
			c.warn(p, 1, "W117", fmt.Sprintf("quote %s: it parses as %s, not a string (section 4)", key, typeName(v)))
		}
	}
}

func (c *checker) checkTags(p string, meta map[string]any) {
	tags, present := meta["tags"]
	if !present || tags == nil {
		return
	}
	if s, ok := tags.(string); ok {
		if strings.TrimSpace(s) != "" {
			hint := ""
			if strings.Contains(s, ",") {
				hint = fmt.Sprintf("; consumers read it as the single tag %s", pyRepr(s))
			}
			c.warn(p, 1, "W110", "write tags as a YAML list (sections 4.1 and 11.2)"+hint)
		}

		return
	}
	if _, ok := tags.([]any); !ok {
		c.warn(p, 1, "W110", "write tags as a YAML list of strings (section 4.1)")
	}
}

func (c *checker) checkLifecycle(p string, meta map[string]any) {
	status := meta["status"]
	if !blank(status) {
		s, _ := status.(string)
		if s != statusDraft && s != statusStable && s != statusDeprecated {
			c.warn(p, 1, "W105", fmt.Sprintf("status is draft, stable, or deprecated; consumers treat %s as not current (section 5.4)", pyRepr(status)))
		}
	}
	sup := meta["superseded_by"]
	if blank(sup) {
		return
	}
	if s, _ := status.(string); s != statusDeprecated {
		c.warn(p, 1, "W123", "superseded_by has a meaning only with status: deprecated (section 5.4)")
	}
	c.checkPathField(p, "superseded_by", sup)
}

func (c *checker) checkGenerated(p string, meta map[string]any) (time.Time, bool) {
	gen := meta["generated"]
	if blank(gen) {
		return time.Time{}, false
	}
	m, ok := asMap(gen)
	if !ok || blank(m["by"]) {
		c.warn(p, 1, "W102", "generated is a mapping with a by field (section 5.2)")

		return time.Time{}, false
	}
	c.checkActor(p, "generated.by", m["by"])
	if blank(m["at"]) {
		return time.Time{}, false
	}
	t, ok := c.checkTimestamp(p, "generated.at", m["at"])
	if ok && isDateOnly(m["at"]) {
		c.warn(p, 1, "W122", "write generated.at as a date-time, because consumers compare it with verified[].at (section 5)")
	}

	return t, ok
}

func (c *checker) checkVerified(p string, meta map[string]any, genAt time.Time, hasGenAt bool) {
	var times []time.Time
	for i, e := range asList(meta["verified"]) {
		field := fmt.Sprintf("verified[%d]", i)
		m, ok := asMap(e)
		if !ok || blank(m["by"]) || blank(m["at"]) {
			c.warn(p, 1, "W104", field+" needs by and at; consumers ignore it (section 5.2)")

			continue
		}
		c.checkActor(p, field+".by", m["by"])
		if t, ok := c.checkTimestamp(p, field+".at", m["at"]); ok {
			times = append(times, t)
			if isDateOnly(m["at"]) {
				c.warn(p, 1, "W122", fmt.Sprintf("write %s.at as a date-time, because consumers compare it with generated.at (section 5)", field))
			}
		}
	}
	if len(times) == 0 || !hasGenAt {
		return
	}
	for _, t := range times {
		if !t.Before(genAt) {
			return
		}
	}
	c.info(p, "I204", "no verification is current: each verified event is older than generated.at, so consumers read the concept as unverified (section 5.3)")
}

func (c *checker) checkSources(p string, meta map[string]any) map[string]bool {
	ids := map[string]bool{}
	shared := meta["usage_window"]
	if !blank(shared) {
		c.checkWindow(p, "usage_window", shared)
	}
	for i, e := range asList(meta["sources"]) {
		field := fmt.Sprintf("sources[%d]", i)
		m, ok := asMap(e)
		if !ok || blank(m["resource"]) {
			c.warn(p, 1, "W106", field+" needs a resource; consumers ignore it (section 5.1)")

			continue
		}
		if !blank(m["id"]) {
			id := str(m["id"])
			if ids[id] {
				c.warn(p, 1, "W107", fmt.Sprintf("%s.id %s is not unique (section 5.1)", field, pyRepr(id)))
			}
			ids[id] = true
		}
		if !blank(m["author"]) {
			c.checkActor(p, field+".author", m["author"])
		}
		if !blank(m["last_modified"]) {
			c.checkTimestamp(p, field+".last_modified", m["last_modified"])
		}
		if !blank(m["usage_window"]) {
			c.checkWindow(p, field+".usage_window", m["usage_window"])
		}
		if !blank(m["usage_count"]) && blank(shared) && blank(m["usage_window"]) {
			c.warn(p, 1, "W109", field+".usage_count needs a usage_window that gives its period (section 5.1)")
		}
		if rs, ok := m["resource"].(string); ok && !containsSpace(rs) {
			c.checkPathField(p, field+".resource", rs)
		}
	}

	return ids
}

func (c *checker) checkWindow(p, field string, window any) {
	m, ok := asMap(window)
	if !ok || blank(m["from"]) || blank(m["to"]) {
		c.warn(p, 1, "W101", field+" is a { from, to } mapping of timestamps (section 5.1)")

		return
	}
	c.checkTimestamp(p, field+".from", m["from"])
	c.checkTimestamp(p, field+".to", m["to"])
}

func (c *checker) checkStaleAfter(p string, meta map[string]any) {
	v := meta["stale_after"]
	if blank(v) {
		return
	}
	t, ok := parseTimestamp(v)
	switch {
	case !ok:
		c.warn(p, 1, "W101", fmt.Sprintf("stale_after does not parse, so consumers never treat the concept as fresh (section 5.5); found %s", pyRepr(v)))
	case !c.now.Before(t):
		c.info(p, "I201", fmt.Sprintf("the concept is stale: stale_after %s has passed (section 5.5)", str(v)))
	}
}

func (c *checker) checkComputation(p string, meta map[string]any, body string) {
	if blank(meta["runtime"]) {
		c.warn(p, 1, "W111", "an Attested Computation needs runtime (section 10.2)")
	}
	fences := len(computationFences(body))
	switch {
	case !blank(meta["computation"]):
		c.checkPathField(p, "computation", meta["computation"])
		if fences > 0 {
			c.warn(p, 1, "W112", "with a computation field, do not also put a fenced block in the Computation section (section 10.3)")
		}
	case fences == 0:
		c.warn(p, 1, "W112", "no computation: no computation field and no fenced block in a Computation section, so the concept is not attestable (section 10.3)")
	case fences > 1:
		c.warn(p, 1, "W126", fmt.Sprintf("put exactly one fenced block in the Computation section; consumers use the first of %d (section 10.3)", fences))
	}
	for i, e := range asList(meta["parameters"]) {
		m, ok := asMap(e)
		if !ok || blank(m["name"]) || blank(m["type"]) {
			c.warn(p, 1, "W113", fmt.Sprintf("parameters[%d] needs name and type (section 10.2)", i))
		}
	}
	for _, key := range []string{keyExecutor, keyAttester} {
		v := meta[key]
		if blank(v) {
			continue
		}
		m, ok := asMap(v)
		if !ok || blank(m["resource"]) {
			c.warn(p, 1, "W114", key+" is a mapping with a resource field (section 10.2)")

			continue
		}
		if _, isList := m["receipt"].([]any); key == keyExecutor && !isList {
			c.warn(p, 1, "W114", "executor.receipt lists the fields that a run returns (section 10.2)")
		}
		c.checkPathField(p, key+".resource", m["resource"])
	}
	if blank(meta[keyAttester]) {
		c.info(p, "I205", "without an attester, consumers cannot show values from this computation as attested (section 10.2)")
	}
}

func (c *checker) checkBody(p, body string, bodyLine int, sourceIDs map[string]bool) {
	reported := map[string]bool{}
	for _, pl := range proseLines(body) {
		n := bodyLine + pl.Offset
		for _, m := range footnoteRE.FindAllStringSubmatch(pl.Text, -1) {
			label := m[1]
			if !sourceIDs[label] && !reported[label] {
				reported[label] = true
				c.warn(p, n, "W108", fmt.Sprintf("footnote [^%s] has no sources entry with that id (section 5.1)", label))
			}
		}
	}
	for _, l := range bodyLinks(body, bodyLine) {
		if c.b.resolve(p, l.Target, false).Kind == RefMissing {
			c.warn(p, l.Line, "W115", fmt.Sprintf("link target %s is not in the bundle (section 6.1)", pyRepr(l.Target)))
		}
	}
}

func (c *checker) checkActor(p, field string, v any) {
	if s, ok := v.(string); !ok || !actorRE.MatchString(s) {
		c.warn(p, 1, "W103", fmt.Sprintf("%s is an actor: <producer>/<version> or <scheme>:<id>, such as human:<id> (section 7); found %s", field, pyRepr(v)))
	}
}

func (c *checker) checkTimestamp(p, field string, v any) (time.Time, bool) {
	t, ok := parseTimestamp(v)
	if !ok {
		c.warn(p, 1, "W101", fmt.Sprintf("%s is an RFC 3339 date-time with an offset, such as 2026-06-30T14:00:00Z, or a date such as 2026-06-30 (section 5); found %s", field, pyRepr(v)))
	}

	return t, ok
}

func (c *checker) checkPathField(p, field string, v any) {
	s, ok := v.(string)
	if !ok {
		return
	}
	switch c.b.resolve(p, s, true).Kind {
	case RefMissing:
		c.warn(p, 1, "W116", fmt.Sprintf("%s %s does not resolve to a file in the bundle (section 6.2)", field, pyRepr(s)))
	case RefFallback:
		c.warn(p, 1, "W119", fmt.Sprintf("%s %s resolves only from the bundle root, through the v0.2 fallback; write it as /%s (section 6.2)", field, pyRepr(s), s))
	case RefFound, RefExternal:
	}
}

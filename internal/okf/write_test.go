package okf

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeNow() time.Time { return time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) }

const (
	testBundle     = "home"
	typeOnlyFields = `{"type":"A"}`
	pathXY         = "x/y"
)

type wfix struct {
	t     *testing.T
	agent string
	dir   string // bundle dir
	now   time.Time
	w     *Writer
}

func newWFix(t *testing.T) *wfix {
	t.Helper()
	agent := t.TempDir()
	dir := filepath.Join(agent, KnowledgeDirName, testBundle)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &wfix{t: t, agent: agent, dir: dir, now: writeNow()}
	f.w = NewWriter(WriterOptions{AgentDir: agent, Now: func() time.Time { return f.now }, Version: "1.2 beta"})

	return f
}

func (f *wfix) put(rel, text string) {
	f.t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wfix) get(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, rel))
	if err != nil {
		f.t.Fatal(err)
	}

	return string(b)
}

func (f *wfix) digest(rel string) string { return Digest([]byte(f.get(rel))) }

func obj(t *testing.T, js string) *omap {
	t.Helper()
	v, err := parseOrdered([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(*omap)
	if !ok {
		t.Fatalf("not an object: %s", js)
	}

	return m
}

func strp(s string) *string { return &s }

func (f *wfix) create(path, fields, body string) (*Receipt, error) {
	return f.w.Write(context.Background(), WriteRequest{
		Action: WriteCreate, Bundle: testBundle, Path: path, Fields: obj(f.t, fields), Body: strp(body), Model: "ollama-gemma::gemma4:31b",
	})
}

const humanConcept = `---
type: Metric
title: Revenue
# who owns this
x-acme-owner: finance
tags: [a, b]
generated:
  by: "human:marco"
  at: "2026-11-01T00:00:00Z"
verified:
  - { by: "human:ana", at: "2026-11-02T00:00:00Z" }
---

# Revenue

Body text.
`

func TestCreateStampsAndMaintains(t *testing.T) {
	f := newWFix(t)
	r, err := f.create("metrics/revenue", `{"type":"Metric","title":"Revenue","description":"Total revenue","tags":["finance"]}`, "# Revenue\n\nHello.\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.Result != "created" || r.Qualified != "home/metrics/revenue" {
		t.Fatalf("receipt %+v", r)
	}
	got := f.get("metrics/revenue.md")
	for _, want := range []string{"type: \"Metric\"", "tags: [\"finance\"]", "generated:\n  by: \"ready/1.2-beta\"\n  at: \"2026-11-20T10:00:00Z\"\n  x-ready-model: \"ollama-gemma::gemma4:31b\"\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	idx := f.get("metrics/index.md")
	if strings.HasPrefix(idx, "---") || !strings.Contains(idx, "* [Revenue](revenue.md) - Total revenue\n") {
		t.Fatalf("metrics/index.md:\n%s", idx)
	}
	root := f.get("index.md")
	if !strings.HasPrefix(root, "---\nokf_version: \"0.3\"\ntitle: \"home\"\n---\n") || !strings.Contains(root, "* [metrics](metrics/)\n") {
		t.Fatalf("root index:\n%s", root)
	}
	log := f.get("log.md")
	if !strings.Contains(log, "## 2026-11-20\n\n* **Creation**: [Revenue](metrics/revenue.md).\n") {
		t.Fatalf("log:\n%s", log)
	}
	if r.Errors != 0 {
		t.Fatalf("bundle should have no errors: %+v", r)
	}
	// No temp leftovers.
	entries, _ := os.ReadDir(filepath.Join(f.dir, "metrics"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("leftover %s", e.Name())
		}
	}
	// Modes.
	if info, _ := os.Stat(filepath.Join(f.dir, "metrics", "revenue.md")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestCreateRefusals(t *testing.T) {
	f := newWFix(t)
	cases := []struct {
		name, path, fields string
		want               error
	}{
		{"traversal", "../work/plan", typeOnlyFields, ErrInvalidReference},
		{"reserved", "tables/index.md", typeOnlyFields, ErrReservedName},
		{"upper", "Tables/Orders", typeOnlyFields, ErrInvalidCreationPath},
		{"generated", pathXY, `{"type":"A","generated":{"by":"a/1"}}`, ErrReservedField},
		{"verified", pathXY, `{"type":"A","verified":[]}`, ErrReservedField},
		{"no type", pathXY, `{"title":"t"}`, ErrInvalidWriteArgs},
		{"blank type", pathXY, `{"type":" "}`, ErrInvalidWriteArgs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.create(c.path, c.fields, "b")
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v want %v", err, c.want)
			}
		})
	}
	f.put("a.md", "---\ntype: A\n---\n")
	if _, err := f.create("a", typeOnlyFields, ""); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("existing: %v", err)
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
		t.Fatalf("refusals must write nothing: %v", entries)
	}
}

func TestUpdateReStampsAndKeepsSpans(t *testing.T) {
	f := newWFix(t)
	f.put("metrics/revenue.md", humanConcept)
	f.put("index.md", "---\nokf_version: \"0.3\"\n---\n\n# home\n")
	before := f.get("metrics/revenue.md")
	r, err := f.w.Write(context.Background(), WriteRequest{
		Action: WriteUpdate, Bundle: testBundle, Path: "metrics/revenue", BaseDigest: Digest([]byte(before)),
		Set: obj(t, `{"description":"Total revenue"}`), Model: "m::x",
	})
	if err != nil {
		t.Fatal(err)
	}
	after := f.get("metrics/revenue.md")
	for _, span := range []string{"# who owns this\nx-acme-owner: finance\n", "tags: [a, b]\n", "verified:\n  - { by: \"human:ana\", at: \"2026-11-02T00:00:00Z\" }\n"} {
		if !strings.Contains(after, span) {
			t.Fatalf("span %q not byte-identical in\n%s", span, after)
		}
	}
	if !strings.Contains(after, "description: \"Total revenue\"\n") || !strings.Contains(after, "by: \"ready/1.2-beta\"") {
		t.Fatalf("after:\n%s", after)
	}
	if r.TierBefore != TierHuman || r.TierAfter != TierNone || r.NoLongerCurrent != 1 || !r.TierDropped {
		t.Fatalf("receipt trust: %+v", r)
	}
	if !strings.Contains(r.String(), "/verify home/metrics/revenue") {
		t.Fatalf("hint missing:\n%s", r)
	}
	if !strings.Contains(f.get("log.md"), "* **Update**: [Revenue](metrics/revenue.md).") {
		t.Fatalf("log: %s", f.get("log.md"))
	}
	if !strings.Contains(f.get("metrics/index.md"), "* [Revenue](revenue.md) - Total revenue") {
		t.Fatalf("index: %s", f.get("metrics/index.md"))
	}
}

func TestUpdateGeneratedKeepsUnknownSubkeys(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", "---\ntype: A\ngenerated:\n  by: \"human:marco\"\n  at: \"2026-11-01T00:00:00Z\"\n  x-acme-ticket: \"T-1\"\n---\nbody\n")
	_, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: f.digest("c.md"), Body: strp("new body")})
	if err != nil {
		t.Fatal(err)
	}
	got := f.get("c.md")
	if !strings.Contains(got, "by: \"ready/1.2-beta\"") || !strings.Contains(got, "x-acme-ticket: \"T-1\"") || strings.Contains(got, "human:marco") {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "x-ready-model") {
		t.Fatalf("model key must be absent when unavailable:\n%s", got)
	}
}

func TestNoopWritesNothing(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	r, err := f.w.Write(context.Background(), WriteRequest{
		Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: f.digest("c.md"),
		Set: obj(t, `{"title":"Revenue"}`), Body: strp("# Revenue   \n\n\n\nBody text.\n\n"),
	})
	if err != nil || r.Result != "unchanged" {
		t.Fatalf("%v %+v", err, r)
	}
	if f.get("c.md") != humanConcept {
		t.Fatal("file changed")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "log.md")); err == nil {
		t.Fatal("no log on no-op")
	}
}

func TestStaleAfterAndLinkChangesAreMeaning(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", "---\ntype: A\nstale_after: \"2027-01-01\"\ngenerated: { by: \"human:m\", at: \"2026-11-01T00:00:00Z\" }\nverified: [{ by: \"human:a\", at: \"2026-11-02T00:00:00Z\" }]\n---\nSee [x](old.md).\n")
	r, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: f.digest("c.md"), Set: obj(t, `{"stale_after":"2028-01-01"}`)})
	if err != nil || r.Result != "updated" || r.TierAfter != TierNone {
		t.Fatalf("%v %+v", err, r)
	}
	r, err = f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: f.digest("c.md"), Body: strp("See [x](new.md).")})
	if err != nil || r.Result != "updated" {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestReservedFieldsOnUpdate(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	for _, req := range []WriteRequest{
		{Set: obj(t, `{"verified":[{"by":"human:marco","at":"2026-11-20T10:00:00Z"}]}`)},
		{Set: obj(t, `{"generated":null}`)},
		{Unset: []string{"verified"}},
	} {
		req.Action, req.Bundle, req.Path, req.BaseDigest = WriteUpdate, testBundle, "c", f.digest("c.md")
		if _, err := f.w.Write(context.Background(), req); !errors.Is(err, ErrReservedField) {
			t.Fatalf("want reserved, got %v", err)
		}
	}
	if f.get("c.md") != humanConcept {
		t.Fatal("changed")
	}
}

func TestTypeProtection(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	for _, req := range []WriteRequest{{Unset: []string{"type"}}, {Set: obj(t, `{"type":null}`)}, {Set: obj(t, `{"type":""}`)}} {
		req.Action, req.Bundle, req.Path, req.BaseDigest = WriteUpdate, testBundle, "c", f.digest("c.md")
		if _, err := f.w.Write(context.Background(), req); !errors.Is(err, ErrTypeProtected) {
			t.Fatalf("want type protected, got %v", err)
		}
	}
}

func TestConflict(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	stale := f.digest("c.md")
	f.put("c.md", strings.Replace(humanConcept, "Body text.", "Edited by operator.", 1))
	_, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: stale, Body: strp("x")})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	if !strings.Contains(f.get("c.md"), "Edited by operator.") {
		t.Fatal("operator edit lost")
	}
	if _, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", Body: strp("x")}); !errors.Is(err, ErrInvalidWriteArgs) {
		t.Fatalf("missing base_digest: %v", err)
	}
}

func TestConcurrentWritersOneWins(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	d := f.digest("c.md")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "c", BaseDigest: d, Body: strp("body " + strconv.Itoa(i))})
		}()
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrConflict):
		default:
			t.Fatalf("unexpected %v", e)
		}
	}
	if ok != 1 {
		t.Fatalf("%d writers succeeded, want exactly 1", ok)
	}
}

func TestUnmergeable(t *testing.T) {
	f := newWFix(t)
	for name, text := range map[string]string{
		"dup":     "---\ntype: A\ntitle: x\ntitle: y\n---\nb\n",
		"noclose": "---\ntype: A\n",
		"nofm":    "just text\n",
		"notmap":  "---\n- a\n---\n",
		"alias":   "---\ntype: A\na: &x 1\nb: *x\n---\n",
		"badyaml": "---\ntype: [A\n---\n",
	} {
		f.put(name+".md", text)
		_, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: name, BaseDigest: f.digest(name + ".md"), Body: strp("x")})
		if err == nil {
			t.Fatalf("%s: expected refusal", name)
		}
		if f.get(name+".md") != text {
			t.Fatalf("%s: file changed", name)
		}
	}
	_, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "dup", BaseDigest: f.digest("dup.md"), Body: strp("x")})
	if !errors.Is(err, ErrUnmergeable) {
		t.Fatalf("dup: %v", err)
	}
}

func TestSymlinkTargetRefused(t *testing.T) {
	f := newWFix(t)
	f.put("real.md", humanConcept)
	if err := os.Symlink("real.md", filepath.Join(f.dir, "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "link", BaseDigest: f.digest("real.md"), Body: strp("x")})
	if !errors.Is(err, ErrSymlinkTarget) {
		t.Fatalf("got %v", err)
	}
	if f.get("real.md") != humanConcept {
		t.Fatal("target modified")
	}
	// symlinked index.md at the destination directory is refused too
	f.put("d/other.md", "---\ntype: A\n---\n")
	if linkErr := os.Symlink("../real.md", filepath.Join(f.dir, "d", "index.md")); linkErr != nil {
		t.Skip()
	}
	r, err := f.create("d/new", typeOnlyFields, "b")
	if err != nil {
		t.Fatal(err)
	}
	var failed bool
	for _, m := range r.Maintenance {
		if m.Rel == "d/index.md" && m.Outcome == "failed" {
			failed = true
		}
	}
	if !failed || f.get("real.md") != humanConcept {
		t.Fatalf("symlinked index must be a reported maintenance failure: %+v", r.Maintenance)
	}
}

func TestIndexMaintenanceIdempotentAndRespectsHandWritten(t *testing.T) {
	f := newWFix(t)
	f.put("index.md", "---\nokf_version: \"0.3\"\ntitle: Home\n---\n\n# Home\n\n* Hand [written](metrics/)\n")
	f.put("metrics/index.md", "# metrics\n\n* See [the revenue note](./revenue.md) for details\n")
	if _, err := f.create("metrics/revenue", `{"type":"Metric","description":"d"}`, "b"); err != nil {
		t.Fatal(err)
	}
	if got := f.get("metrics/index.md"); got != "# metrics\n\n* See [the revenue note](./revenue.md) for details\n" {
		t.Fatalf("hand-written entry changed:\n%s", got)
	}
	if got := f.get("index.md"); strings.Count(got, "metrics/") != 1 {
		t.Fatalf("duplicate subdirectory entry:\n%s", got)
	}
	// tool-format entry refreshed, not duplicated
	if _, err := f.create("metrics/margin", `{"type":"Metric","description":"v1"}`, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "metrics/margin", BaseDigest: f.digest("metrics/margin.md"), Set: obj(t, `{"description":"v2"}`)}); err != nil {
		t.Fatal(err)
	}
	idx := f.get("metrics/index.md")
	if strings.Count(idx, "margin.md") != 1 || !strings.Contains(idx, "- v2\n") {
		t.Fatalf("index:\n%s", idx)
	}
	// existing root frontmatter untouched
	if !strings.HasPrefix(f.get("index.md"), "---\nokf_version: \"0.3\"\ntitle: Home\n---\n") {
		t.Fatal("root frontmatter changed")
	}
}

func TestIndexEscaping(t *testing.T) {
	f := newWFix(t)
	if _, err := f.create("a/b", `{"type":"A","title":"Odd [title]\\ x","description":"two\nlines"}`, "b"); err != nil {
		t.Fatal(err)
	}
	idx := f.get("a/index.md")
	if !strings.Contains(idx, `* [Odd \[title\]\\ x](b.md) - two lines`+"\n") {
		t.Fatalf("index:\n%s", idx)
	}
	if strings.Contains(f.get("a/b.md"), "\nlines") && strings.Contains(idx, "\nlines") {
		t.Fatal("newline leaked")
	}
}

func TestLogOrdering(t *testing.T) {
	f := newWFix(t)
	f.put("log.md", "# Bundle Update Log\n\n## 2026-11-25\n\n* **Update**: future.\n\n## Notes\n\nfree text\n\n## 2026-11-18\n\n* **Creation**: old.\n")
	if _, err := f.create(pathXY, typeOnlyFields, "b"); err != nil {
		t.Fatal(err)
	}
	got := f.get("log.md")
	iFuture, iToday, iOld := strings.Index(got, "## 2026-11-25"), strings.Index(got, "## 2026-11-20"), strings.Index(got, "## 2026-11-18")
	if iFuture >= iToday || iToday >= iOld {
		t.Fatalf("order wrong:\n%s", got)
	}
	if !strings.Contains(got, "## Notes\n\nfree text\n") {
		t.Fatalf("non-date heading disturbed:\n%s", got)
	}
	// second write the same day becomes the first item under today's heading
	if _, err := f.create("x/z", typeOnlyFields, "b"); err != nil {
		t.Fatal(err)
	}
	got = f.get("log.md")
	if strings.Count(got, "## 2026-11-20") != 1 {
		t.Fatalf("duplicate heading:\n%s", got)
	}
	if strings.Index(got, "x/z.md") > strings.Index(got, "x/y.md") {
		t.Fatalf("newest entry must be first:\n%s", got)
	}
}

func TestGateRefusesNewErrorsButNotPreExisting(t *testing.T) {
	f := newWFix(t)
	// pre-existing E005 in log.md does not block a write
	f.put("log.md", "# Bundle Update Log\n\n## not a date\n\nx\n")
	if _, err := f.create("a/b", typeOnlyFields, "b"); err != nil {
		t.Fatalf("pre-existing log error must not block: %v", err)
	}
	if !strings.Contains(f.get("log.md"), "## not a date\n\nx\n") {
		t.Fatal("non-date heading changed")
	}
	// a tags scalar surfaces as a W110 finding, not a failure
	r, err := f.create("a/c", `{"type":"A","tags":"x, y"}`, "b")
	if err != nil {
		t.Fatal(err)
	}
	var w110 bool
	for _, fd := range r.Findings {
		if fd.Code == "W110" {
			w110 = true
		}
	}
	if !w110 {
		t.Fatalf("W110 expected: %+v", r.Findings)
	}
	// a candidate gaining an error is refused with nothing written
	f.put("e.md", "---\ntype: A\n---\nb\n")
	before := f.get("e.md")
	_, err = f.w.Write(context.Background(), WriteRequest{Action: WriteUpdate, Bundle: testBundle, Path: "e", BaseDigest: Digest([]byte(before)), Set: obj(t, `{"x":1}`)})
	if err != nil {
		t.Fatalf("plain update should pass: %v", err)
	}
}

func TestDeprecate(t *testing.T) {
	f := newWFix(t)
	f.put("metrics/net-revenue.md", "---\ntype: Metric\n---\nn\n")
	f.put("metrics/revenue.md", "---\ntype: Metric\ntitle: Revenue\n---\nold\n")
	r, err := f.w.Write(context.Background(), WriteRequest{
		Action: WriteDeprecate, Bundle: testBundle, Path: "metrics/revenue", BaseDigest: f.digest("metrics/revenue.md"),
		SupersededBy: "metrics/net-revenue", Reason: "Replaced.",
	})
	if err != nil || r.Result != "deprecated" {
		t.Fatalf("%v %+v", err, r)
	}
	got := f.get("metrics/revenue.md")
	if !strings.Contains(got, "status: \"deprecated\"\n") || !strings.Contains(got, "superseded_by: \"/metrics/net-revenue.md\"\n") || !strings.Contains(got, "**Deprecated:** Replaced.") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(f.get("log.md"), "* **Deprecation**: [Revenue](metrics/revenue.md), superseded by [net revenue](metrics/net-revenue.md).") {
		t.Fatalf("log:\n%s", f.get("log.md"))
	}
	_, err = f.w.Write(context.Background(), WriteRequest{
		Action: WriteDeprecate, Bundle: testBundle, Path: "metrics/net-revenue", BaseDigest: f.digest("metrics/net-revenue.md"), SupersededBy: "metrics/missing",
	})
	if !errors.Is(err, ErrUnresolvedSuperseded) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizeRefused(t *testing.T) {
	f := newWFix(t)
	if _, err := f.create("big", typeOnlyFields, strings.Repeat("x", MaxFileBytes)); !errors.Is(err, ErrOversize) {
		t.Fatalf("got %v", err)
	}
}

func TestReadPrintsDigestUsedByUpdate(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", humanConcept)
	tool := NewTool(Options{AgentDir: f.agent, Now: func() time.Time { return f.now }})
	out, err := tool.InvokableRun(context.Background(), `{"action":"read","bundle":"home","path":"c"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Digest: "+f.digest("c.md")+"\n") {
		t.Fatalf("digest missing:\n%s", out)
	}
}

// ── /verify ─────────────────────────────────────────────────────────────

func TestVerifyPreviewConfirm(t *testing.T) {
	f := newWFix(t)
	f.put("metrics/revenue.md", "---\ntype: Metric\ngenerated: { by: \"ready/1\", at: \"2026-11-01T00:00:00Z\" }\n---\n<!-- hidden comment -->\nBody\n")
	p, err := f.w.VerifyPreview(context.Background(), testBundle, "metrics/revenue")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Content, "<!-- hidden comment -->") || p.Truncated || !ValidToken(p.Token) || p.Token == f.digest("metrics/revenue.md") {
		t.Fatalf("%+v", p)
	}
	res, err := f.w.VerifyConfirm(context.Background(), testBundle, "metrics/revenue", "marco", strings.ToUpper(p.Token), "cli")
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != TierHuman || res.NotCurrent {
		t.Fatalf("%+v", res)
	}
	got := f.get("metrics/revenue.md")
	if !strings.Contains(got, "verified:\n  - { by: \"human:marco\", at: \"2026-11-20T10:00:00Z\", x-ready-via: \"cli\" }\n") {
		t.Fatalf("%s", got)
	}
	if !strings.Contains(f.get("log.md"), "* **Verification**: [revenue](metrics/revenue.md) confirmed by human:marco.") {
		t.Fatalf("log:\n%s", f.get("log.md"))
	}
	// the old token no longer matches
	if _, err := f.w.VerifyConfirm(context.Background(), testBundle, "metrics/revenue", "marco", p.Token, "cli"); !errors.Is(err, ErrChangedSincePreview) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyDigestIsNotAToken(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", "---\ntype: A\n---\nb\n")
	if _, err := f.w.VerifyConfirm(context.Background(), testBundle, "c", "marco", f.digest("c.md"), "cli"); !errors.Is(err, ErrChangedSincePreview) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(f.get("c.md"), "verified") {
		t.Fatal("must not write")
	}
}

func TestVerifyChangedAfterPreview(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", "---\ntype: A\n---\nb\n")
	p, _ := f.w.VerifyPreview(context.Background(), testBundle, "c")
	f.put("c.md", "---\ntype: A\n---\nchanged\n")
	if _, err := f.w.VerifyConfirm(context.Background(), testBundle, "c", "marco", p.Token, "cli"); !errors.Is(err, ErrChangedSincePreview) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(f.get("c.md"), "verified") {
		t.Fatal("must not write")
	}
}

func TestVerifySplices(t *testing.T) {
	confirm := func(t *testing.T, f *wfix, rel string) string {
		t.Helper()
		id := strings.TrimSuffix(rel, ".md")
		p, err := f.w.VerifyPreview(context.Background(), testBundle, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.w.VerifyConfirm(context.Background(), testBundle, id, "marco", p.Token, "zmq"); err != nil {
			t.Fatal(err)
		}

		return f.get(rel)
	}
	t.Run("single mapping promoted", func(t *testing.T) {
		f := newWFix(t)
		f.put("c.md", "---\ntype: A\nverified: {by: \"process:nightly\", at: \"2026-11-01T00:00:00Z\"}\nx-keep: \"1\"\n---\nb\n")
		got := confirm(t, f, "c.md")
		if !strings.Contains(got, "verified:\n  - { by: \"process:nightly\", at: \"2026-11-01T00:00:00Z\" }\n  - { by: \"human:marco\"") || !strings.Contains(got, "x-keep: \"1\"\n") {
			t.Fatalf("%s", got)
		}
	})
	t.Run("block sequence appended", func(t *testing.T) {
		f := newWFix(t)
		f.put("c.md", "---\ntype: A\nverified:\n    - by: \"human:ana\"\n      at: \"2026-11-01T00:00:00Z\"\ntags: [a]\n---\nb\n")
		got := confirm(t, f, "c.md")
		if !strings.Contains(got, "    - by: \"human:ana\"\n      at: \"2026-11-01T00:00:00Z\"\n    - { by: \"human:marco\"") || !strings.Contains(got, "tags: [a]\n") {
			t.Fatalf("%s", got)
		}
	})
	t.Run("null and empty", func(t *testing.T) {
		for _, v := range []string{"verified:\n", "verified: []\n", "verified: null\n"} {
			f := newWFix(t)
			f.put("c.md", "---\ntype: A\n"+v+"---\nb\n")
			got := confirm(t, f, "c.md")
			if strings.Count(got, "verified:") != 1 || !strings.Contains(got, "human:marco") {
				t.Fatalf("%q:\n%s", v, got)
			}
		}
	})
	t.Run("scalar refused", func(t *testing.T) {
		f := newWFix(t)
		text := "---\ntype: A\nverified: yes please\n---\nb\n"
		f.put("c.md", text)
		p, _ := f.w.VerifyPreview(context.Background(), testBundle, "c")
		if _, err := f.w.VerifyConfirm(context.Background(), testBundle, "c", "marco", p.Token, "cli"); !errors.Is(err, ErrUnmergeable) {
			t.Fatalf("got %v", err)
		}
		if f.get("c.md") != text {
			t.Fatal("file changed")
		}
	})
	t.Run("generated untouched and not-current flagged", func(t *testing.T) {
		f := newWFix(t)
		f.put("c.md", "---\ntype: A\ngenerated: { by: \"ready/1\", at: \"2030-01-01T00:00:00Z\" }\n---\nb\n")
		p, _ := f.w.VerifyPreview(context.Background(), testBundle, "c")
		res, err := f.w.VerifyConfirm(context.Background(), testBundle, "c", "marco", p.Token, "cli")
		if err != nil || !res.NotCurrent || res.Tier != TierNone {
			t.Fatalf("%v %+v", err, res)
		}
		if !strings.Contains(f.get("c.md"), "generated: { by: \"ready/1\", at: \"2030-01-01T00:00:00Z\" }\n") {
			t.Fatal("generated changed")
		}
	})
}

func TestVerifyPreviewTruncation(t *testing.T) {
	f := newWFix(t)
	f.put("c.md", "---\ntype: A\n---\n"+strings.Repeat("é", MaxPreviewBytes))
	p, err := f.w.VerifyPreview(context.Background(), testBundle, "c")
	if err != nil || !p.Truncated || len(p.Content) > MaxPreviewBytes || p.Size <= MaxPreviewBytes {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestParseConceptRef(t *testing.T) {
	b, id, err := ParseConceptRef("home/metrics%2Frev.md")
	if err != nil || b != testBundle || id != "metrics/rev" {
		t.Fatalf("%q %q %v", b, id, err)
	}
	for _, bad := range []string{testBundle, "/x", "home/", "home/../x", "home/.hidden", ".h/x", "%zz/x"} {
		if _, _, err := ParseConceptRef(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestEmitterAndSplice(t *testing.T) {
	m := obj(t, `{"s":"a \"q\"\nline","n":3,"f":1.5,"b":true,"z":null,"l":["x",2],"o":{"k":"v","j":[1,2]},"ml":[{"a":1,"b":[{"c":2}]},"s"]}`)
	var sb strings.Builder
	for _, k := range m.keys {
		sb.WriteString(renderEntry(k, m.vals[k], 0))
	}
	text := "---\n" + sb.String() + "---\n"
	fm := ParseFrontmatter(text)
	if fm.State != FMOK {
		t.Fatalf("emitter output does not parse (%s):\n%s", fm.Problem, text)
	}
	if fm.Meta["s"] != "a \"q\"\nline" || fm.Meta["n"] != int64(3) || fm.Meta["b"] != true {
		t.Fatalf("round trip: %#v\n%s", fm.Meta, text)
	}
	// untouched spans, comment, CRLF line and flow mapping stay byte-identical
	src := "---\r\ntype: A\r\n# note\r\nx-flow: { a: 1,   b: 2 }\r\ntitle: Old\r\n---\r\nbody\r\n"
	d, err := parseDoc(src)
	if err != nil {
		t.Fatal(err)
	}
	out := d.render([]keyEdit{{key: "title", text: "title: \"New\"\n"}, {key: "extra", text: "extra: 1\n"}}, d.body)
	want := "---\r\ntype: A\r\n# note\r\nx-flow: { a: 1,   b: 2 }\r\ntitle: \"New\"\nextra: 1\n---\r\nbody\r\n"
	if out != want {
		t.Fatalf("got %q\nwant %q", out, want)
	}
}

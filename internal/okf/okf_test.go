package okf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scanFixture(t *testing.T, name string) map[string]*Concept {
	t.Helper()
	res, err := openFixture(t, name).Scan(context.Background(), ScanOptions{}, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*Concept{}
	for _, c := range res.Rows {
		m[c.ID] = c
	}

	return m
}

func TestFrontmatterEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		state FMState
	}{
		{"bom", "\ufeff---\ntype: A\n---\nbody", FMOK},
		{"crlf", "---\r\ntype: A\r\n---\r\nbody", FMOK},
		{"absent", "no fm", FMAbsent},
		{"unclosed", "---\ntype: A\n", FMUnclosed},
		{"empty block", "---\n---\nbody", FMOK},
		{"not a mapping", "---\n- a\n---\n", FMNotMapping},
		{"invalid", "---\na: [b\n---\n", FMInvalid},
		{"two documents", "---\na: 1\n---\n", FMOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseFrontmatter(c.text); got.State != c.state {
				t.Fatalf("state %v, want %v (%s)", got.State, c.state, got.Problem)
			}
		})
	}
	fm := ParseFrontmatter("---\ntype: A\ngenerated:\n  by: a/1\n  by: b/2\n---\n")
	if fm.State != FMDuplicate || fm.DupKey != "by" || fm.DupLine != 5 {
		t.Fatalf("nested duplicate: %+v", fm)
	}
	fm = ParseFrontmatter("---\ntitle: yes\nwhen: 2026-06-30T14:00:00Z\nn: 1:20\nnum: 3\n---\n")
	if fm.Meta["title"] != "yes" || fm.Meta["when"] != "2026-06-30T14:00:00Z" || fm.Meta["n"] != "1:20" {
		t.Fatalf("core schema resolution: %#v", fm.Meta)
	}
	if fm.Meta["num"] != int64(3) {
		t.Fatalf("int not resolved: %#v", fm.Meta["num"])
	}
}

func TestYAML12Divergence(t *testing.T) {
	c := scanFixture(t, "div-yaml12")["c"]
	if c.Title != "yes" || c.Desc != "on" || c.Status != "no" {
		t.Fatalf("title=%q desc=%q status=%q", c.Title, c.Desc, c.Status)
	}
	res, _ := openFixture(t, "div-yaml12").Validate(context.Background(), fixedNow())
	for _, f := range res.Findings {
		if f.Code == "W117" {
			t.Fatalf("unexpected W117: %+v", f)
		}
	}
}

func TestMalformedGeneratedIsUnverified(t *testing.T) {
	m := scanFixture(t, "div-malformed-generated")
	for _, id := range []string{"c", "d"} {
		c := m[id]
		if c.Tier != TierNone {
			t.Errorf("%s tier %s", id, c.Tier)
		}
		if !contains(c.TierNotes, MalformedGeneratedNote) {
			t.Errorf("%s missing malformed note: %v", id, c.TierNotes)
		}
	}
}

func TestTimestampFallbackOnlyInOlderBundles(t *testing.T) {
	// 0.3 bundle: timestamp ignored, every event current.
	c := scanFixture(t, "div-timestamp-fallback")["c"]
	if c.Tier != TierHuman || c.ChangeTime != nil {
		t.Fatalf("0.3 bundle: tier %s change %v", c.Tier, c.ChangeTime)
	}
	// 0.2 bundle: timestamp is the change time.
	ts := scanFixture(t, "legacy-v02")["sub/ts"]
	if ts.Tier != TierNone || !ts.VerifiedBeforeChange {
		t.Fatalf("0.2 bundle: tier %s before=%v", ts.Tier, ts.VerifiedBeforeChange)
	}
}

func TestTrustScenarios(t *testing.T) {
	m := scanFixture(t, "status")
	want := map[string]Tier{
		statusHumanID: TierHuman, "machine-single": TierMachine, "team": TierMachine,
		"upper-human": TierMachine, "before-change": TierNone, "date-only": TierNone, "offset": TierHuman,
	}
	for id, tier := range want {
		if m[id].Tier != tier {
			t.Errorf("%s: tier %s, want %s", id, m[id].Tier, tier)
		}
	}
	if !m["before-change"].VerifiedBeforeChange {
		t.Error("before-change should note verified-before-change")
	}
	if m["deprecated"].Replacement != statusHumanID || m["deprecated"].Current {
		t.Errorf("deprecated: %+v", m["deprecated"])
	}
	if m["deprecated-raw"].Replacement != "" || m["deprecated-raw"].SupersededBy != "/gone.md" {
		t.Errorf("deprecated-raw: %+v", m["deprecated-raw"])
	}
	if m["unknown-status"].Current || m["unknown-status"].Status != "Stable" {
		t.Errorf("unknown-status: %+v", m["unknown-status"])
	}
	frs := map[string]string{
		"stale": "stale since 2026-06-30", "stale-edge": "stale since 2026-07-01T00:00:00Z",
		"stale-malformed": "stale (malformed stale_after)", "fresh": "fresh until 2026-07-01T00:00:01Z",
		"with-blank": "no stale_after", "human": "fresh until 2026-12-31T00:00:00Z",
	}
	for id, f := range frs {
		if got := m[id].Freshness(); got != f {
			t.Errorf("%s freshness %q, want %q", id, got, f)
		}
	}
	if _, ok := m[".hidden/secret"]; ok {
		t.Error("hidden path must not be a concept")
	}
	if m["no-title-file_name"].Title != "no title file name" {
		t.Errorf("derived title %q", m["no-title-file_name"].Title)
	}
}

func TestScalarTagsNeverSplit(t *testing.T) {
	m := scanFixture(t, "fields")
	if got := m["w110-scalar"].Tags; len(got) != 1 || got[0] != "sales, orders" {
		t.Fatalf("tags %v", got)
	}
	res, _ := openFixture(t, "fields").Scan(context.Background(), ScanOptions{Tag: "sales"}, fixedNow())
	for _, c := range res.Rows {
		if c.ID == "w110-scalar" {
			t.Fatal("tag filter must not split at commas")
		}
	}
}

func TestPathResolution(t *testing.T) {
	b := openFixture(t, "paths")
	res, err := b.Read(context.Background(), "a/b/c", 0, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range res.Links {
		got[l.Target] = l.Res.String()
	}
	want := map[string]string{
		"/top.md": "-> top.md", "../../top.md": "-> top.md", "../sibling.md": sibRes,
		"../sibling%20name.md": "-> a/sibling name.md", "../sibling.md#f": sibRes, "../sibling.md?x=1": sibRes,
		"https://x.example": extNotRead, "//host/x": extNotRead,
		"../../../outside.md": extNotRead, "nope.md": "missing", "mailto:a@b.c": extNotRead,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("link %q: %q, want %q", k, got[k], w)
		}
	}
	if len(res.Fields) != 1 || res.Fields[0].Res.Target != "a/sibling.md" {
		t.Errorf("fields %+v", res.Fields)
	}
	// Field-only fallback in a 0.2 bundle, never for body links.
	lb := openFixture(t, "legacy-v02")
	r2, err := lb.Read(context.Background(), "sub/c", 0, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if r2.Fields[0].Res.Kind != RefFallback {
		t.Errorf("expected fallback for bare path field, got %v", r2.Fields[0].Res)
	}
	for _, l := range r2.Links {
		if l.Target == "data/x.csv" && l.Res.Kind != RefMissing {
			t.Errorf("body link must not use the fallback: %v", l.Res)
		}
	}
}

func TestCitationLookup(t *testing.T) {
	res, err := openFixture(t, "clean").Read(context.Background(), "tables/orders", 0, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations) != 1 || !res.Citations[0].Found || res.Citations[0].Title != "ERP orders" || res.Citations[0].Resource != "https://erp.example/orders" {
		t.Fatalf("citations %+v", res.Citations)
	}
}

func mk(t *testing.T, id, meta string) *Concept {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("---\nokf_version: \"0.3\"\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".md"), []byte("---\ntype: Note\n"+meta+"---\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBundleDir(dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := b.LoadConcept(id, fixedNow())
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestCompareOrdering(t *testing.T) {
	humanStale := mk(t, "a", "generated: {by: x/1, at: \"2026-06-01T00:00:00Z\"}\nverified: {by: \"human:m\", at: \"2026-06-02T00:00:00Z\"}\nstale_after: \"2026-06-10T00:00:00Z\"\n")
	machineFresh := mk(t, "b", "generated: {by: x/1, at: \"2026-06-01T00:00:00Z\"}\n")
	r := Compare([]*Concept{machineFresh, humanStale})
	if r.Ordered[0] != humanStale || r.Steps[0] != StepTier {
		t.Fatalf("tier should decide: %v %v", r.Ordered[0].ID, r.Steps)
	}
	depHuman := mk(t, "c", "status: deprecated\nverified: {by: \"human:m\", at: \"2026-06-02T00:00:00Z\"}\n")
	draft := mk(t, "d", "status: draft\n")
	r = Compare([]*Concept{depHuman, draft})
	if r.Ordered[0] != draft || r.Steps[0] != StepStatus {
		t.Fatalf("status should decide: %v", r.Steps)
	}
	same1, same2 := mk(t, "e", ""), mk(t, "f", "")
	if r = Compare([]*Concept{same1, same2}); r.Steps[0] != StepUndecided {
		t.Fatalf("expected undecided, got %v", r.Steps)
	}
	older := mk(t, "g", "generated: {by: x/1, at: \"2026-06-01T00:00:00Z\"}\n")
	newer := mk(t, "h", "generated: {by: x/1, at: \"2026-06-05T00:00:00Z\"}\n")
	none := mk(t, "i", "")
	r = Compare([]*Concept{none, older, newer})
	if r.Ordered[0] != newer || r.Ordered[2] != none || r.Steps[0] != StepRecency {
		t.Fatalf("recency: %v %v", r.Ordered[0].ID, r.Steps)
	}
}

func TestAttestedComputationReport(t *testing.T) {
	res, err := openFixture(t, "clean").Read(context.Background(), "computations/revenue", 0, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	ci := res.Computation
	if ci == nil || !ci.Attestable || !strings.Contains(ci.Block, "SUM(amount)") || ci.Runtime != "bigquery" || len(ci.Params) != 1 || !ci.Params[0].Required || ci.HasAttester != true {
		t.Fatalf("%+v", ci)
	}
	if ci.ExecutorResource != "/references/run.txt" || len(ci.Receipt) != 2 {
		t.Fatalf("executor %+v", ci)
	}
	// No computation, no runtime, no attester.
	res, err = openFixture(t, "fields").Read(context.Background(), "w111", 0, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if res.Computation.Attestable || len(res.Computation.Notes) != 2 {
		t.Fatalf("%+v", res.Computation)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}

	return false
}

// ----- agent-directory level tests (tool, confinement, bounds) -----

func agentWith(t *testing.T, files map[string]string) string {
	t.Helper()
	agent := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(agent, "okfs", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return agent
}

func run(t *testing.T, agent, args string) (string, error) {
	t.Helper()

	return NewTool(Options{AgentDir: agent, Now: fixedNow}).InvokableRun(context.Background(), args)
}

const (
	statusHumanID = "human"
	noteDoc       = "---\ntype: Note\n---\nx\n"
	homeIndexPath = "home/index.md"
	extNotRead    = "external (not read)"
	sibRes        = "-> a/sibling.md"
)

const homeIndex = "---\nokf_version: \"0.3\"\ntitle: Home\ndescription: Home notes\n---\n"

func TestListActions(t *testing.T) {
	agent := agentWith(t, map[string]string{
		homeIndexPath:      homeIndex,
		"notes/c.md":       noteDoc,
		"loose.md":         "ignored",
		".hidden/index.md": homeIndex,
	})
	if err := os.Symlink(t.TempDir(), filepath.Join(agent, "okfs", "shared")); err != nil {
		t.Skip("symlinks unavailable")
	}
	out, err := run(t, agent, `{"action":"list"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bundles in okfs/: 2", "home  okf_version 0.3", "title: Home", "notes  okf_version not declared", "no root index.md", "Skipped: shared"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".hidden") || strings.Contains(out, "loose") {
		t.Errorf("hidden/file entries must not be listed:\n%s", out)
	}
}

func TestNoBundles(t *testing.T) {
	out, err := run(t, t.TempDir(), `{"action":"list"}`)
	if err != nil || !strings.Contains(out, ": 0") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRefusals(t *testing.T) {
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/c.md": noteDoc, "work/plan.md": "---\ntype: Note\n---\nsecret\n"})
	cases := []struct {
		name string
		args string
		want error
	}{
		{"unknown action", `{"action":"nope"}`, ErrUnknownAction},
		{"traversal bundle", `{"action":"scan","bundle":"../secrets"}`, ErrUnknownBundle},
		{"unknown bundle", `{"action":"scan","bundle":"ghost"}`, ErrUnknownBundle},
		{"empty bundle", `{"action":"scan"}`, ErrUnknownBundle},
		{"traversal path", `{"action":"read","bundle":"home","path":"../work/plan.md"}`, ErrInvalidReference},
		{"absolute path", `{"action":"read","bundle":"home","path":"/etc/passwd"}`, ErrInvalidReference},
		{"hidden segment", `{"action":"read","bundle":"home","path":".git/config"}`, ErrInvalidReference},
		{"backslash", `{"action":"read","bundle":"home","path":"a\\b"}`, ErrInvalidReference},
		{"unknown concept", `{"action":"read","bundle":"home","path":"ghost"}`, ErrUnknownConcept},
		{"index dir traversal", `{"action":"index","bundle":"home","dir":".."}`, ErrInvalidReference},
		{"compare unqualified", `{"action":"compare","refs":["a","b"]}`, ErrInvalidReference},
		{"compare too few", `{"action":"compare","refs":["home/c"]}`, ErrInvalidReference},
		{"compare traversal", `{"action":"compare","refs":["home/c","home/../work/plan"]}`, ErrInvalidReference},
		{"bad json", `{`, ErrInvalidReference},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := run(t, agent, c.args)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
		})
	}
	_, err := run(t, agent, `{"action":"scan","bundle":"ghost"}`)
	if !strings.Contains(err.Error(), "available: home, work") {
		t.Errorf("refusal should list bundles: %v", err)
	}
}

func TestSymlinkEscapes(t *testing.T) {
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/ok.md": noteDoc})
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("---\ntype: Note\n---\nleak\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(agent, "okfs", "home", "tables.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	// A directory symlink inside the bundle is never followed.
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(agent, "okfs", "home", "linkdir")); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, agent, `{"action":"scan","bundle":"home"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "leak") || !strings.Contains(out, "R303") || strings.Contains(out, "linkdir") {
		t.Errorf("scan must skip the escaping symlink with an R303 note:\n%s", out)
	}
	out, err = run(t, agent, `{"action":"validate","bundle":"home"}`)
	if err != nil || !strings.Contains(out, "R303") {
		t.Errorf("validate must report R303: %v\n%s", err, out)
	}
	if _, err = run(t, agent, `{"action":"read","bundle":"home","path":"tables"}`); !errors.Is(err, ErrOutsideBundle) {
		t.Errorf("read of escaping symlink: %v", err)
	}
	if _, err = run(t, agent, `{"action":"read","bundle":"home","path":"linkdir/outside"}`); err == nil {
		t.Error("read through a directory symlink must fail")
	}
}

func TestInBundleFileSymlinkIsRead(t *testing.T) {
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/real.md": "---\ntype: Note\n---\nreal\n"})
	if err := os.Symlink("real.md", filepath.Join(agent, "okfs", "home", "alias.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	out, err := run(t, agent, `{"action":"read","bundle":"home","path":"alias"}`)
	if err != nil || !strings.Contains(out, "real") {
		t.Fatalf("%v %s", err, out)
	}
}

func TestKnowledgeRootLinkedOut(t *testing.T) {
	agent := t.TempDir()
	ext := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ext, "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(agent, "okfs")); err != nil {
		t.Skip("symlinks unavailable")
	}
	for _, a := range []string{`{"action":"list"}`, `{"action":"scan","bundle":"home"}`, `{"action":"compare","refs":["home/a","home/b"]}`} {
		if _, err := run(t, agent, a); !errors.Is(err, ErrKnowledgeRootEscape) {
			t.Errorf("%s: %v", a, err)
		}
	}
}

func TestInvalidUTF8AndOversizeDoNotAbort(t *testing.T) {
	agent := agentWith(t, map[string]string{
		homeIndexPath:  homeIndex,
		"home/good.md": "---\ntype: Note\n---\nok\n",
		"home/bad.md":  "---\ntype: Note\n---\n\xff\xfe broken\n",
		"home/big.md":  "---\ntype: Note\n---\n" + strings.Repeat("x", MaxFileBytes+10),
	})
	out, err := run(t, agent, `{"action":"validate","bundle":"home"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bad.md: warning R301") || !strings.Contains(out, "big.md: warning R302") {
		t.Errorf("expected R301 and R302 findings:\n%s", out)
	}
	out, err = run(t, agent, `{"action":"scan","bundle":"home"}`)
	if err != nil || !strings.Contains(out, "good") || !strings.Contains(out, "R301") {
		t.Errorf("scan must still list good.md:\n%s %v", out, err)
	}
	if _, err := run(t, agent, `{"action":"read","bundle":"home","path":"bad"}`); !errors.Is(err, ErrInvalidReference) {
		t.Errorf("read of invalid UTF-8: %v", err)
	}
}

func TestScanBoundsAndFilters(t *testing.T) {
	files := map[string]string{"big/index.md": homeIndex}
	for i := 0; i < 120; i++ {
		stale := ""
		if i%2 == 0 {
			stale = "stale_after: \"2026-01-01T00:00:00Z\"\n"
		}
		files[fmt.Sprintf("big/c%03d.md", i)] = "---\ntype: Note\ntags: [t]\n" + stale + "---\nneedle" + fmt.Sprint(i) + "\n"
	}
	agent := agentWith(t, files)
	out, err := run(t, agent, `{"action":"scan","bundle":"big"}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "  [Note]"); n != MaxScanRows {
		t.Errorf("rows %d, want %d", n, MaxScanRows)
	}
	if !strings.Contains(out, "120 concepts") || !strings.Contains(out, "Showing the first 50 of 120 rows") {
		t.Errorf("summary/hint missing:\n%s", out[:400])
	}
	out, _ = run(t, agent, `{"action":"scan","bundle":"big","stale":true}`)
	if !strings.Contains(out, "Showing 60 concepts that match the filters") || !strings.Contains(out, "stale since 2026-01-01") {
		t.Errorf("stale filter:\n%s", out[:400])
	}
	out, _ = run(t, agent, `{"action":"scan","bundle":"big","grep":"NEEDLE117"}`)
	if n := strings.Count(out, "  [Note]"); n != 1 {
		t.Errorf("grep should match whole-file text case-insensitively, rows=%d", n)
	}
}

func TestReadBoundsAndOffset(t *testing.T) {
	body := strings.Repeat("é", 60000) // 120 000 bytes, multibyte
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/long.md": "---\ntype: Note\n---\n" + body + "\n"})
	out, err := run(t, agent, `{"action":"read","bundle":"home","path":"long"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "continue with offset=") {
		t.Fatalf("no continuation hint")
	}
	var next int
	idx := strings.Index(out, "offset=")
	if idx < 0 {
		t.Fatal("missing offset hint")
	}
	if _, err := fmt.Sscanf(out[idx:], "offset=%d", &next); err != nil {
		t.Fatal(err)
	}
	if next > MaxReadBytes || next < MaxReadBytes-4 {
		t.Errorf("cut at %d, want within one rune of %d", next, MaxReadBytes)
	}
	total := 0
	for off := 0; ; {
		o, err := run(t, agent, fmt.Sprintf(`{"action":"read","bundle":"home","path":"long","offset":%d}`, off))
		if err != nil {
			t.Fatal(err)
		}
		total += strings.Count(o, "é")
		i := strings.Index(o, "continue with offset=")
		if i < 0 {
			break
		}
		if _, err := fmt.Sscanf(o[i:], "continue with offset=%d", &off); err != nil {
			t.Fatal(err)
		}
	}
	if total != 60000 {
		t.Errorf("reassembled %d runes, want 60000", total)
	}
}

func TestReadVariants(t *testing.T) {
	agent := agentWith(t, map[string]string{
		homeIndexPath:          homeIndex,
		"home/tables/index.md": "# Tables\n",
		"home/tables/o.md":     "---\ntype: T\nstatus: deprecated\nsuperseded_by: /tables/n.md\n---\nold\n",
		"home/tables/n.md":     "---\ntype: T\n---\nnew\n",
		"home/data.csv":        "a,b\n",
	})
	o, err := run(t, agent, `{"action":"read","bundle":"home","path":"tables/o"}`)
	if err != nil || !strings.Contains(o, "deprecated, not current; replacement: home/tables/n") {
		t.Fatalf("%v\n%s", err, o)
	}
	o, err = run(t, agent, `{"action":"read","bundle":"home","path":"tables/index.md"}`)
	if err != nil || strings.Contains(o, "Trust:") {
		t.Fatalf("directory index must not carry a trust header: %v\n%s", err, o)
	}
	o, err = run(t, agent, `{"action":"read","bundle":"home","path":"data.csv"}`)
	if err != nil || !strings.Contains(o, "a,b") || strings.Contains(o, "Trust:") {
		t.Fatalf("%v\n%s", err, o)
	}
}

func TestIndexAction(t *testing.T) {
	agent := agentWith(t, map[string]string{
		homeIndexPath: homeIndex, "home/top.md": "---\ntype: A\ntitle: Top\n---\n",
		"home/x/y/deep.md": "---\ntype: B\ntitle: Deep\n---\n", "home/x/other.md": "---\ntype: C\n---\n",
	})
	o, err := run(t, agent, `{"action":"index","bundle":"home"}`)
	if err != nil || !strings.Contains(o, "3 concepts") || !strings.Contains(o, "x/y/\n  x/y/deep  [B]  Deep") || !strings.Contains(o, "  top  [A]  Top") {
		t.Fatalf("%v\n%s", err, o)
	}
	o, err = run(t, agent, `{"action":"index","bundle":"home","dir":"x/y"}`)
	if err != nil || !strings.Contains(o, "1 concepts") || strings.Contains(o, "top") {
		t.Fatalf("%v\n%s", err, o)
	}
	if _, err = run(t, agent, `{"action":"index","bundle":"home","dir":"nope"}`); !errors.Is(err, ErrUnknownConcept) {
		t.Fatalf("%v", err)
	}
}

func TestValidateBounds(t *testing.T) {
	files := map[string]string{"v/index.md": homeIndex}
	for i := 0; i < 130; i++ {
		files[fmt.Sprintf("v/n%03d.md", i)] = "no frontmatter\n"
	}
	o, err := run(t, agentWith(t, files), `{"action":"validate","bundle":"v"}`)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(o, "W120"); n != MaxFindings || !strings.Contains(o, "Showing 100 of 130 findings") || !strings.Contains(o, "130 warnings") {
		t.Errorf("findings bound: %d\n%s", n, o[:300])
	}
}

func TestValidateInlineFindings(t *testing.T) {
	agent := agentWith(t, map[string]string{
		"v/index.md":  homeIndex,
		"v/dup.md":    "---\ntype: A\ntype: B\n---\n",
		"v/notype.md": "---\ntitle: x\n---\n",
	})
	o, err := run(t, agent, `{"action":"validate","bundle":"v"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o, "dup.md:3: error E007") || !strings.Contains(o, "not conformant") || !strings.Contains(o, "notype.md:1: warning W120") {
		t.Errorf("%s", o)
	}
}

func TestCancellation(t *testing.T) {
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/c.md": "---\ntype: A\n---\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := NewTool(Options{AgentDir: agent})
	for _, a := range []string{`{"action":"scan","bundle":"home"}`, `{"action":"validate","bundle":"home"}`, `{"action":"list"}`} {
		if _, err := tool.InvokableRun(ctx, a); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", a, err)
		}
	}
}

func TestCompareAcrossBundles(t *testing.T) {
	agent := agentWith(t, map[string]string{
		"a/index.md": homeIndex, "a/x.md": "---\ntype: M\nstatus: deprecated\n---\n",
		"b/index.md": homeIndex, "b/y.md": "---\ntype: M\n---\n",
	})
	o, err := run(t, agent, `{"action":"compare","refs":["a/x","b/y"]}`)
	if err != nil || !strings.Contains(o, "1. b/y") || !strings.Contains(o, "decided by status") || !strings.Contains(o, "judgment") {
		t.Fatalf("%v\n%s", err, o)
	}
	if _, err = run(t, agent, `{"action":"compare","refs":["a/x","b/ghost"]}`); !errors.Is(err, ErrUnknownConcept) {
		t.Fatalf("%v", err)
	}
	if _, err = run(t, agent, `{"action":"compare","refs":["a/x","c/y"]}`); !errors.Is(err, ErrUnknownBundle) {
		t.Fatalf("%v", err)
	}
}

func TestToolReadsNothingOutsideBundleOnWalk(t *testing.T) {
	// Another bundle's secret must never surface from a scan of this one.
	agent := agentWith(t, map[string]string{
		homeIndexPath: homeIndex, "home/a.md": "---\ntype: A\n---\n[x](../../work/plan.md)\n",
		"work/index.md": homeIndex, "work/plan.md": "---\ntype: A\n---\nTOPSECRET\n",
	})
	for _, a := range []string{`{"action":"scan","bundle":"home","grep":"TOPSECRET"}`, `{"action":"read","bundle":"home","path":"a"}`} {
		o, err := run(t, agent, a)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(o, "TOPSECRET") {
			t.Errorf("leak through %s:\n%s", a, o)
		}
	}
	o, _ := run(t, agent, `{"action":"read","bundle":"home","path":"a"}`)
	if !strings.Contains(o, extNotRead) {
		t.Errorf("link to another bundle must be external:\n%s", o)
	}
}

func TestCaseSensitivePaths(t *testing.T) {
	agent := agentWith(t, map[string]string{homeIndexPath: homeIndex, "home/Sibling.md": "---\ntype: A\n---\n", "home/a.md": "---\ntype: A\n---\n[l](sibling.md) [m](Sibling.md)\n"})
	o, err := run(t, agent, `{"action":"read","bundle":"home","path":"a"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(o, "sibling.md missing") || !strings.Contains(o, "Sibling.md -> Sibling.md") {
		t.Errorf("paths are case-sensitive:\n%s", o)
	}
}

func TestInfoSchema(t *testing.T) {
	info, err := NewTool(Options{AgentDir: t.TempDir()}).Info(context.Background())
	if err != nil || info.Name != ToolName || !strings.Contains(info.Desc, "never writes") {
		t.Fatalf("%v %+v", err, info)
	}
}

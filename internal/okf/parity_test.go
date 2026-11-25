package okf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixedNow is the --now handed to the reference scripts when the goldens
// were generated.
func fixedNow() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }

func fixtureDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("testdata", "okf"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}

	return out
}

func openFixture(t *testing.T, name string) *Bundle {
	t.Helper()
	b, err := OpenBundleDir(filepath.Join("testdata", "okf", name), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)

	return b
}

type goldenValidate struct {
	Version  any `json:"okf_version"`
	Files    int `json:"files_checked"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
	Findings []struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Level string `json:"level"`
		Code  string `json:"code"`
	} `json:"findings"`
}

func tuples(fs []string) string {
	sort.Strings(fs)

	return strings.Join(fs, "\n")
}

// TestValidateParity: for every non-divergence fixture the multiset of
// (path, line, level, code) equals the committed golden output of the pinned
// validate.py run with a fixed now.
func TestValidateParity(t *testing.T) {
	for _, name := range fixtureDirs(t) {
		if strings.HasPrefix(name, "div-") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".validate.json"))
			if err != nil {
				t.Fatal(err)
			}
			var g goldenValidate
			if err = json.Unmarshal(raw, &g); err != nil {
				t.Fatal(err)
			}
			want := make([]string, 0, len(g.Findings))
			for _, f := range g.Findings {
				line := 0
				if f.Line != nil {
					line = *f.Line
				}
				want = append(want, fmt.Sprintf("%s|%d|%s|%s", f.Path, line, f.Level, f.Code))
			}
			res, err := openFixture(t, name).Validate(context.Background(), fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(res.Findings))
			for _, f := range res.Findings {
				got = append(got, fmt.Sprintf("%s|%d|%s|%s", f.Path, f.Line, f.Level, f.Code))
			}
			if tuples(got) != tuples(want) {
				t.Errorf("findings differ\n got:\n%s\nwant:\n%s", tuples(got), tuples(want))
			}
			if res.FilesChecked != g.Files || res.Errors != g.Errors || res.Warnings != g.Warnings || res.Infos != g.Info {
				t.Errorf("summary got files=%d e=%d w=%d i=%d, want files=%d e=%d w=%d i=%d",
					res.FilesChecked, res.Errors, res.Warnings, res.Infos, g.Files, g.Errors, g.Warnings, g.Info)
			}
		})
	}
}

type goldenScan struct {
	Concepts []struct {
		ID            string   `json:"id"`
		Type          string   `json:"type"`
		Title         string   `json:"title"`
		Trust         string   `json:"trust"`
		Status        string   `json:"status"`
		Current       bool     `json:"current"`
		Stale         bool     `json:"stale"`
		ReplacementID *string  `json:"replacement_id"`
		Tags          []string `json:"tags"`
	} `json:"concepts"`
	NotConcepts []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"not_concepts"`
	Summary struct {
		Concepts   int `json:"concepts"`
		Stale      int `json:"stale"`
		Deprecated int `json:"deprecated"`
		Draft      int `json:"draft"`
		NotCurrent int `json:"not_current"`
	} `json:"summary"`
}

// TestScanParity compares the computed concept state with the pinned
// scan.py output for every non-divergence fixture.
func TestScanParity(t *testing.T) {
	for _, name := range fixtureDirs(t) {
		if strings.HasPrefix(name, "div-") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".scan.json"))
			if err != nil {
				t.Skip("no scan golden: " + err.Error())
			}
			var g goldenScan
			if err = json.Unmarshal(raw, &g); err != nil {
				t.Fatal(err)
			}
			res, err := openFixture(t, name).Scan(context.Background(), ScanOptions{}, fixedNow())
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != len(g.Concepts) {
				t.Fatalf("concept count %d, want %d", len(res.Rows), len(g.Concepts))
			}
			byID := map[string]*Concept{}
			for _, c := range res.Rows {
				byID[c.ID] = c
			}
			for _, w := range g.Concepts {
				c := byID[w.ID]
				if c == nil {
					t.Errorf("missing concept %s", w.ID)

					continue
				}
				rep := ""
				if w.ReplacementID != nil {
					rep = *w.ReplacementID
				}
				got := fmt.Sprintf("%s|%s|%s|%s|%t|%t|%s|%v", c.Type, c.Title, c.Tier, c.Status, c.Current, c.Stale, c.Replacement, c.Tags)
				want := fmt.Sprintf("%s|%s|%s|%s|%t|%t|%s|%v", w.Type, w.Title, w.Trust, w.Status, w.Current, w.Stale, rep, w.Tags)
				if got != want {
					t.Errorf("%s:\n got %s\nwant %s", w.ID, got, want)
				}
			}
			if len(res.NotConcepts) != len(g.NotConcepts) {
				t.Errorf("not-concepts %d, want %d", len(res.NotConcepts), len(g.NotConcepts))
			}
			s := res.Summary
			if s.Stale != g.Summary.Stale || s.Deprecated != g.Summary.Deprecated || s.Draft != g.Summary.Draft || s.NotCurrent != g.Summary.NotCurrent {
				t.Errorf("summary counters differ: %+v vs %+v", s, g.Summary)
			}
		})
	}
}

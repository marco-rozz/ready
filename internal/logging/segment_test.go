package logging

import (
	"path/filepath"
	"testing"
	"time"
)

// Test fixtures shared by the segment and rotation tests.
const (
	baseReady  = "ready"
	legacyName = "ready.log"
	segToday   = "ready-2026-09-30.log"
	lockName   = "ready.log.lock"
)

func TestSegmentSchemeSplit(t *testing.T) {
	s := newSegmentScheme(filepath.Join(".ready", "logs", legacyName))
	if s.dir != filepath.Join(".ready", "logs") || s.base != baseReady || s.ext != ".log" {
		t.Errorf("scheme = %+v", s)
	}

	noExt := newSegmentScheme(baseReady)
	if noExt.base != baseReady || noExt.ext != "" {
		t.Errorf("scheme without ext = %+v", noExt)
	}
}

func TestSegmentNameRoundTrip(t *testing.T) {
	loc := time.UTC
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, loc)

	for _, fp := range []string{legacyName, baseReady, "ready.json.log"} {
		s := newSegmentScheme(fp)

		for _, idx := range []int{0, 1, 2, 10, 123} {
			n := s.name(day, idx)

			gotDay, gotIdx, ok := s.parse(n, loc)
			if !ok || !gotDay.Equal(day) || gotIdx != idx {
				t.Errorf("%s: parse(%q) = %v, %d, %v; want %v, %d, true", fp, n, gotDay, gotIdx, ok, day, idx)
			}
		}
	}

	s := newSegmentScheme(legacyName)
	if got := s.name(day, 0); got != segToday {
		t.Errorf("name idx 0 = %q", got)
	}

	if got := s.name(day, 1); got != "ready-2026-09-30.1.log" {
		t.Errorf("name idx 1 = %q", got)
	}

	if got := newSegmentScheme(baseReady).name(day, 1); got != "ready-2026-09-30.1" {
		t.Errorf("extension-less name = %q", got)
	}
}

func TestSegmentParseRejects(t *testing.T) {
	s := newSegmentScheme(legacyName)
	rejected := []string{
		legacyName,
		lockName,
		"ready-2026-02-30.log",
		"ready-2026-13-01.log",
		"ready-2026-09-30.01.log",
		"ready-2026-09-30.0.log",
		"ready-2026-09-30..log",
		"ready-2026-09-30.1a.log",
		"ready-2026-09-30.-1.log",
		"ready-2026-9-30.log",
		"ready-2026-09-30.log.gz",
		"ready-2026-09-30.txt",
		"other-2026-09-30.log",
		"ready-dev-2026-09-30.log",
		"ready-notes.txt",
		"thoughts.jsonl",
		"ready-2026-09-30x.log",
	}

	for _, n := range rejected {
		if _, _, ok := s.parse(n, time.UTC); ok {
			t.Errorf("parse(%q) accepted, want rejected", n)
		}
	}

	noExt := newSegmentScheme(baseReady)
	for _, n := range []string{segToday, "ready-2026-09-30.gz", baseReady} {
		if _, _, ok := noExt.parse(n, time.UTC); ok {
			t.Errorf("extension-less parse(%q) accepted, want rejected", n)
		}
	}
}

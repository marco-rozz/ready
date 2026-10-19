package main

import (
	"regexp"
	"testing"

	"github.com/marco-rozz/ready/internal/llm"
)

// TestCurrentTimeStamp_TailDynamicMinuteGranularity verifies that two calls
// within the same minute produce an identical rendered timestamp under
// LayoutTailDynamic, so the prefix is stable for as long as the wall-clock
// minute does not roll over.
func TestCurrentTimeStamp_TailDynamicMinuteGranularity(t *testing.T) {
	got1 := currentTimeStamp(llm.LayoutTailDynamic)
	got2 := currentTimeStamp(llm.LayoutTailDynamic)

	if got1 != got2 {
		t.Fatalf("expected identical timestamp string within the same minute, got %q vs %q", got1, got2)
	}

	// The format is "Monday, 02 January 2006, 15:04 MST" — the clock has no
	// HH:MM:SS triple.
	if regexp.MustCompile(`\d{2}:\d{2}:\d{2}`).MatchString(got1) {
		t.Fatalf("expected no HH:MM:SS seconds component in %q", got1)
	}
}

// TestCurrentTimeStamp_HeadSafeHourGranularity verifies LayoutHeadSafe
// quantizes to the hour: the rendered string is identical regardless of the
// actual minute (stable within the hour).
func TestCurrentTimeStamp_HeadSafeHourGranularity(t *testing.T) {
	got := currentTimeStamp(llm.LayoutHeadSafe)

	if !regexp.MustCompile(`\d{2}:00 `).MatchString(got) {
		t.Fatalf("expected minutes quantized to literal :00 in %q", got)
	}

	got2 := currentTimeStamp(llm.LayoutHeadSafe)
	if got != got2 {
		t.Fatalf("expected identical timestamp string across calls within the same hour, got %q vs %q", got, got2)
	}
}

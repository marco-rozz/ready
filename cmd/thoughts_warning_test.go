package main

import (
	"testing"

	"github.com/rs/zerolog"
)

// TestThoughtsLevelMismatch covers the boolean predicate used by
// warnThoughtsLevelMismatch so the warning logic is unit-tested in isolation.
func TestThoughtsLevelMismatch(t *testing.T) {
	cases := []struct {
		name          string
		file, console zerolog.Level
		hasFile       bool
		mismatch      bool
	}{
		{"console trace", zerolog.InfoLevel, zerolog.TraceLevel, false, false},
		{"console debug", zerolog.InfoLevel, zerolog.DebugLevel, false, false},
		{"console info, no file", zerolog.DebugLevel, zerolog.InfoLevel, false, true},
		{"file debug routes thoughts to the file", zerolog.DebugLevel, zerolog.WarnLevel, true, false},
		{"file trace", zerolog.TraceLevel, zerolog.ErrorLevel, true, false},
		{"both above debug", zerolog.InfoLevel, zerolog.WarnLevel, true, true},
		{"both info", zerolog.InfoLevel, zerolog.InfoLevel, true, true},
	}

	for _, tc := range cases {
		if got := thoughtsLevelMismatch(tc.file, tc.console, tc.hasFile); got != tc.mismatch {
			t.Errorf("%s: thoughtsLevelMismatch(%v, %v, %v) = %v, want %v", tc.name, tc.file, tc.console, tc.hasFile, got, tc.mismatch)
		}
	}
}

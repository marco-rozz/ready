package logutil

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

type ctxKey string

type capturedEntry struct {
	ctx context.Context
	rec slog.Record
}

// recordSink collects handled slog records for assertions.
type recordSink struct {
	mu      sync.Mutex
	entries []capturedEntry
}

func (s *recordSink) add(ctx context.Context, r slog.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, capturedEntry{ctx: ctx, rec: r})
}

func (s *recordSink) snapshot() []capturedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *recordSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func attrsMap(r slog.Record) map[string]slog.Value {
	m := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value
		return true
	})
	return m
}

func restoreGlobal(t *testing.T) func() {
	t.Helper()
	old := Default()
	return func() { SetDefault(old) }
}

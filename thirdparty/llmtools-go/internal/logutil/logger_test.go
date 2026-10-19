package logutil

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
)

func TestSetDefault_NilBecomesDiscardAndIsSilent(t *testing.T) {
	defer restoreGlobal(t)()

	sink := &recordSink{}
	l := newCaptureLogger(sink)

	SetDefault(l)
	Info("before", "k", "v")
	if got := sink.len(); got != 1 {
		t.Fatalf("expected 1 record before nil SetDefault, got %d", got)
	}

	SetDefault(nil)
	if Default() == nil {
		t.Fatalf("Default() returned nil after SetDefault(nil)")
	}
	if Default() == l {
		t.Fatalf("expected Default() to change after SetDefault(nil)")
	}

	// Should go to discard logger, not our capture logger.
	Info("after", "k", "v")
	if got := sink.len(); got != 1 {
		t.Fatalf("expected no additional records after SetDefault(nil); got %d total", got)
	}
}

func TestDefault_ReturnsCurrentlyInstalledLogger(t *testing.T) {
	defer restoreGlobal(t)()

	s1 := &recordSink{}
	l1 := newCaptureLogger(s1)
	SetDefault(l1)
	if got := Default(); got != l1 {
		t.Fatalf("Default() != l1")
	}

	s2 := &recordSink{}
	l2 := newCaptureLogger(s2)
	SetDefault(l2)
	if got := Default(); got != l2 {
		t.Fatalf("Default() != l2")
	}
}

func TestTopLevelFunctions_LogLevelsMessagesAttrsAndContext(t *testing.T) {
	defer restoreGlobal(t)()

	const (
		msg   = "hello"
		ctxK  = ctxKey("reqID")
		ctxV  = "abc-123"
		attrK = "k"
		attrV = "v"
	)

	tests := []struct {
		name       string
		call       func(ctx context.Context)
		wantLevel  slog.Level
		wantMsg    string
		wantAttrs  map[string]slog.Value
		wantCtxVal any
		checkCtx   bool
	}{
		{
			name:      "Debug",
			call:      func(context.Context) { Debug(msg, attrK, attrV) },
			wantLevel: slog.LevelDebug,
			wantMsg:   msg,
			wantAttrs: map[string]slog.Value{attrK: slog.StringValue(attrV)},
		},
		{
			name:       "DebugContext",
			call:       func(ctx context.Context) { DebugContext(ctx, msg, attrK, attrV) },
			wantLevel:  slog.LevelDebug,
			wantMsg:    msg,
			wantAttrs:  map[string]slog.Value{attrK: slog.StringValue(attrV)},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
		{
			name:      "Info",
			call:      func(context.Context) { Info(msg, attrK, attrV) },
			wantLevel: slog.LevelInfo,
			wantMsg:   msg,
			wantAttrs: map[string]slog.Value{attrK: slog.StringValue(attrV)},
		},
		{
			name:       "InfoContext",
			call:       func(ctx context.Context) { InfoContext(ctx, msg, attrK, attrV) },
			wantLevel:  slog.LevelInfo,
			wantMsg:    msg,
			wantAttrs:  map[string]slog.Value{attrK: slog.StringValue(attrV)},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
		{
			name:      "Warn",
			call:      func(context.Context) { Warn(msg, attrK, attrV) },
			wantLevel: slog.LevelWarn,
			wantMsg:   msg,
			wantAttrs: map[string]slog.Value{attrK: slog.StringValue(attrV)},
		},
		{
			name:       "WarnContext",
			call:       func(ctx context.Context) { WarnContext(ctx, msg, attrK, attrV) },
			wantLevel:  slog.LevelWarn,
			wantMsg:    msg,
			wantAttrs:  map[string]slog.Value{attrK: slog.StringValue(attrV)},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
		{
			name:      "Error",
			call:      func(context.Context) { Error(msg, attrK, attrV) },
			wantLevel: slog.LevelError,
			wantMsg:   msg,
			wantAttrs: map[string]slog.Value{attrK: slog.StringValue(attrV)},
		},
		{
			name:       "ErrorContext",
			call:       func(ctx context.Context) { ErrorContext(ctx, msg, attrK, attrV) },
			wantLevel:  slog.LevelError,
			wantMsg:    msg,
			wantAttrs:  map[string]slog.Value{attrK: slog.StringValue(attrV)},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
		{
			name:       "Log",
			call:       func(ctx context.Context) { Log(ctx, slog.LevelWarn, msg, attrK, attrV) },
			wantLevel:  slog.LevelWarn,
			wantMsg:    msg,
			wantAttrs:  map[string]slog.Value{attrK: slog.StringValue(attrV)},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
		{
			name: "LogAttrs",
			call: func(ctx context.Context) {
				LogAttrs(ctx, slog.LevelInfo, msg,
					slog.String(attrK, attrV),
					slog.Int("n", 7),
				)
			},
			wantLevel: slog.LevelInfo,
			wantMsg:   msg,
			wantAttrs: map[string]slog.Value{
				attrK: slog.StringValue(attrV),
				"n":   slog.IntValue(7),
			},
			wantCtxVal: ctxV,
			checkCtx:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &recordSink{}
			SetDefault(newCaptureLogger(sink))

			ctx := context.WithValue(t.Context(), ctxK, ctxV)
			tt.call(ctx)

			entries := sink.snapshot()
			if len(entries) != 1 {
				t.Fatalf("expected 1 record, got %d", len(entries))
			}

			gotRec := entries[0].rec
			if gotRec.Level != tt.wantLevel {
				t.Fatalf("level: got %v want %v", gotRec.Level, tt.wantLevel)
			}
			if gotRec.Message != tt.wantMsg {
				t.Fatalf("message: got %q want %q", gotRec.Message, tt.wantMsg)
			}

			gotAttrs := attrsMap(gotRec)
			for k, wantV := range tt.wantAttrs {
				gotV, ok := gotAttrs[k]
				if !ok {
					t.Fatalf("missing attr %q; got attrs=%v", k, gotAttrs)
				}
				if !reflect.DeepEqual(gotV, wantV) {
					t.Fatalf("attr %q: got %#v want %#v", k, gotV, wantV)
				}
			}

			if tt.checkCtx {
				got := entries[0].ctx.Value(ctxK)
				if got != tt.wantCtxVal {
					t.Fatalf("ctx value: got %#v want %#v", got, tt.wantCtxVal)
				}
			}
		})
	}
}

func TestWith_AddsAttrsToReturnedLogger(t *testing.T) {
	defer restoreGlobal(t)()

	sink := &recordSink{}
	SetDefault(newCaptureLogger(sink))

	l := With("a", "b")
	l.Info("msg")

	entries := sink.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 record, got %d", len(entries))
	}

	gotAttrs := attrsMap(entries[0].rec)
	got, ok := gotAttrs["a"]
	if !ok {
		t.Fatalf("missing attr from With: got attrs=%v", gotAttrs)
	}
	if !reflect.DeepEqual(got, slog.StringValue("b")) {
		t.Fatalf("attr a: got %#v want %#v", got, slog.StringValue("b"))
	}
}

func TestEdgeCases_BadArgs_DoNotPanicAndStillLog(t *testing.T) {
	defer restoreGlobal(t)()

	tests := []struct {
		name string
		call func()
	}{
		{
			name: "odd number of args",
			call: func() { Info("msg", "k") },
		},
		{
			name: "non-string key",
			call: func() { Info("msg", 123, "v") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &recordSink{}
			SetDefault(newCaptureLogger(sink))

			tt.call()

			entries := sink.snapshot()
			if len(entries) != 1 {
				t.Fatalf("expected 1 record, got %d", len(entries))
			}
			// Edge cases: just assert it logged and carried *some* attrs (slog may synthesize keys).
			gotAttrs := attrsMap(entries[0].rec)
			if len(gotAttrs) == 0 {
				t.Fatalf("expected some attrs, got none")
			}
		})
	}
}

func TestConcurrentLogging_AllRecordsCaptured(t *testing.T) {
	defer restoreGlobal(t)()

	sink := &recordSink{}
	SetDefault(newCaptureLogger(sink))

	const (
		goroutines = 20
		perG       = 200
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(g int) {
			defer wg.Done()
			for i := range perG {
				Info("m", "g", g, "i", i)
			}
		}(g)
	}
	wg.Wait()

	want := goroutines * perG
	if got := sink.len(); got != want {
		t.Fatalf("expected %d records, got %d", want, got)
	}
}

// captureHandler is a minimal slog.Handler that captures records.
// It also supports WithAttrs so tests can validate logutil.With(...).
type captureHandler struct {
	sink     *recordSink
	preAttrs []slog.Attr
	groups   []string // not used in assertions; kept to satisfy Handler semantics
}

func newCaptureLogger(sink *recordSink) *slog.Logger {
	return slog.New(&captureHandler{sink: sink})
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	rc := r.Clone()
	if len(h.preAttrs) > 0 {
		rc.AddAttrs(h.preAttrs...)
	}
	h.sink.add(ctx, rc)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &captureHandler{
		sink:     h.sink,
		preAttrs: append(append([]slog.Attr(nil), h.preAttrs...), attrs...),
		groups:   append([]string(nil), h.groups...),
	}
	return next
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	next := &captureHandler{
		sink:     h.sink,
		preAttrs: append([]slog.Attr(nil), h.preAttrs...),
		groups:   append(append([]string(nil), h.groups...), name),
	}
	return next
}

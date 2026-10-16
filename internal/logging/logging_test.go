//nolint:goconst
package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

// syncBuffer is a goroutine-safe bytes.Buffer used as the console output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func mustBuild(t *testing.T, cfg config.LoggingConfig, out *syncBuffer, color bool, mod func(*rotateOptions)) Result {
	t.Helper()

	res, err := build(cfg, envOf(nil), out, color, mod)
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}

	t.Cleanup(func() { _ = res.Closer.Close() })

	return res
}

func segmentOf(t *testing.T, dir string) string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "agent-") {
			return filepath.Join(dir, e.Name())
		}
	}

	t.Fatalf("no segment in %s", dir)

	return ""
}

func TestParseLevelAllValid(t *testing.T) {
	cases := []struct {
		name string
		want zerolog.Level
	}{
		{"trace", zerolog.TraceLevel},
		{"debug", zerolog.DebugLevel},
		{"info", zerolog.InfoLevel},
		{"warn", zerolog.WarnLevel},
		{"error", zerolog.ErrorLevel},
	}

	for _, tc := range cases {
		l, ok := parseLevel(tc.name)
		if !ok {
			t.Errorf("parseLevel(%q) ok=false, want true", tc.name)
		}

		if l != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.name, l, tc.want)
		}
	}
}

func TestParseLevelInvalid(t *testing.T) {
	_, ok := parseLevel("verbose")
	if ok {
		t.Error("parseLevel(verbose) ok=true, want false")
	}
}

func TestNewDefaultsConsole(t *testing.T) {
	res, err := New(config.LoggingConfig{Level: "info", Format: "console"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = res.Closer.Close() })

	if res.Logger.GetLevel() != zerolog.InfoLevel || res.HasFile {
		t.Errorf("level = %v, hasFile = %v; want info, false", res.Logger.GetLevel(), res.HasFile)
	}
}

func TestNoFileCreatedWithoutFilePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	out := &syncBuffer{}
	res := mustBuild(t, config.LoggingConfig{Level: "info", Format: "console"}, out, false, nil)
	res.Logger.Info().Msg("hello")

	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files created without file_path: %v", entries)
	}
}

func TestFileSinkWritesJSONSegment(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LoggingConfig{Level: "debug", Format: "console", FilePath: filepath.Join(dir, "sub", "agent.log")}

	res := mustBuild(t, cfg, &syncBuffer{}, false, nil)
	res.Logger.Info().Str("key", "value").Msg("test message")

	if err := res.Closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(segmentOf(t, filepath.Join(dir, "sub")))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var record map[string]any
	if jsonErr := json.Unmarshal(bytes.TrimSpace(data), &record); jsonErr != nil {
		t.Fatalf("segment not valid JSON: %v — content: %s", jsonErr, data)
	}

	if record["message"] != "test message" {
		t.Errorf("message = %q, want %q", record["message"], "test message")
	}
}

func TestUnusableLogLocationFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")

	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The parent "directory" is a regular file: MkdirAll must fail.
	cfg := config.LoggingConfig{Level: "info", FilePath: filepath.Join(blocker, "agent.log")}
	if _, err := build(cfg, envOf(nil), &syncBuffer{}, false, nil); err == nil || !strings.Contains(err.Error(), "agent.log") {
		t.Errorf("err = %v, want an error naming the path", err)
	}
}

func TestConsoleLevelSeparateFromFileLevel(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LoggingConfig{Level: "debug", ConsoleLevel: "warn", Format: "json", FilePath: filepath.Join(dir, "agent.log")}
	out := &syncBuffer{}

	res := mustBuild(t, cfg, out, false, nil)
	res.Logger.Debug().Msg("dbg")
	res.Logger.Info().Msg("inf")
	res.Logger.Warn().Msg("wrn")
	res.Logger.Error().Msg("err")

	if err := res.Closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, _ := os.ReadFile(segmentOf(t, dir))
	file, console := string(data), out.String()

	for _, m := range []string{"dbg", "inf", "wrn", "err"} {
		if !strings.Contains(file, m) {
			t.Errorf("file missing %q", m)
		}
	}

	for _, m := range []string{"dbg", "inf"} {
		if strings.Contains(console, m) {
			t.Errorf("console contains %q below console_level", m)
		}
	}

	for _, m := range []string{"wrn", "err"} {
		if !strings.Contains(console, m) {
			t.Errorf("console missing %q", m)
		}
	}
}

func TestConsoleColours(t *testing.T) {
	for _, color := range []bool{true, false} {
		out := &syncBuffer{}
		res := mustBuild(t, config.LoggingConfig{Level: "info", Format: "console"}, out, color, nil)
		res.Logger.Info().Msg("hello")

		if got := strings.Contains(out.String(), "\x1b["); got != color {
			t.Errorf("color=%v: ANSI present = %v", color, got)
		}
	}
}

func TestResolveLevels(t *testing.T) {
	cases := []struct {
		name        string
		cfg         config.LoggingConfig
		env         map[string]string
		file, cons  zerolog.Level
		warnSubstrs []string
	}{
		{"yaml only", config.LoggingConfig{Level: "info", ConsoleLevel: "warn"}, nil, zerolog.InfoLevel, zerolog.WarnLevel, nil},
		{"console defaults to level", config.LoggingConfig{Level: "debug"}, nil, zerolog.DebugLevel, zerolog.DebugLevel, nil},
		{"invalid console falls back", config.LoggingConfig{Level: "debug", ConsoleLevel: "verbose"}, nil, zerolog.DebugLevel, zerolog.DebugLevel, nil},
		{
			"env forces both",
			config.LoggingConfig{Level: "info", ConsoleLevel: "warn"},
			map[string]string{envLevel: "debug"},
			zerolog.DebugLevel, zerolog.DebugLevel, nil,
		},
		{
			// The former prefix and legacy alias are NOT recognised at all:
			// levels stay YAML-driven with no deprecation warning (D12).
			"former names are ignored",
			config.LoggingConfig{Level: "info"},
			map[string]string{"READY_HOME_LOG_LEVEL": "debug", "HOME_AGENT_LOG_LEVEL": "debug"},
			zerolog.InfoLevel, zerolog.InfoLevel, nil,
		},
		{
			"invalid env falls back",
			config.LoggingConfig{Level: "info", ConsoleLevel: "warn"},
			map[string]string{envLevel: "verbose"},
			zerolog.InfoLevel, zerolog.WarnLevel,
			[]string{envLevel, "verbose"},
		},
		{"console off keeps fatal", config.LoggingConfig{Level: "debug", ConsoleLevel: "off"}, nil, zerolog.DebugLevel, zerolog.FatalLevel, nil},
		{
			"env overrides console off",
			config.LoggingConfig{Level: "info", ConsoleLevel: "off"},
			map[string]string{envLevel: "debug"},
			zerolog.DebugLevel, zerolog.DebugLevel, nil,
		},
		{
			"empty env ignored",
			config.LoggingConfig{Level: "error"},
			map[string]string{envLevel: ""},
			zerolog.ErrorLevel, zerolog.ErrorLevel, nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, cons, warns := resolveLevels(tc.cfg, envOf(tc.env))
			if file != tc.file || cons != tc.cons {
				t.Errorf("levels = %v/%v, want %v/%v", file, cons, tc.file, tc.cons)
			}

			if len(tc.warnSubstrs) == 0 {
				if len(warns) != 0 {
					t.Errorf("unexpected warnings %v", warns)
				}

				return
			}

			if len(warns) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warns)
			}

			for _, s := range tc.warnSubstrs {
				if !strings.Contains(warns[0], s) {
					t.Errorf("warning %q missing %q", warns[0], s)
				}
			}
		})
	}
}

func TestRootLevelIsLowerOfSinks(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LoggingConfig{Level: "debug", ConsoleLevel: "error", FilePath: filepath.Join(dir, "agent.log")}

	res := mustBuild(t, cfg, &syncBuffer{}, false, nil)
	if res.Logger.GetLevel() != zerolog.DebugLevel || res.FileLevel != zerolog.DebugLevel || res.ConsoleLevel != zerolog.ErrorLevel || !res.HasFile {
		t.Errorf("result = level %v file %v console %v hasFile %v", res.Logger.GetLevel(), res.FileLevel, res.ConsoleLevel, res.HasFile)
	}
}

// TestFailureWarningBypassesConsoleLevel covers task 4.6's console_level
// clause: the healthy→failing WARN reaches the console even at error level.
func TestFailureWarningBypassesConsoleLevel(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LoggingConfig{Level: "info", ConsoleLevel: "error", Format: "json", FilePath: filepath.Join(dir, "agent.log")}
	out := &syncBuffer{}
	ffs := &faultFS{}
	clock := &fakeClock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}

	res := mustBuild(t, cfg, out, false, func(o *rotateOptions) {
		o.FS = ffs
		o.Now = clock.Now
		o.Location = time.UTC
	})

	res.Logger.Info().Msg("ok")
	ffs.set(func(f *faultFS) { f.failWrites = syscall.ENOSPC })
	res.Logger.Info().Msg("dropped-1")
	res.Logger.Info().Msg("dropped-2")

	if got := strings.Count(out.String(), "log file is failing"); got != 1 {
		t.Errorf("failing WARNs on console = %d, want 1; console:\n%s", got, out.String())
	}
}

func TestConsoleOffKeepsOnlyFatalAndFileNotices(t *testing.T) {
	dir := t.TempDir()
	cfg := config.LoggingConfig{Level: "debug", ConsoleLevel: "off", Format: "json", FilePath: filepath.Join(dir, "agent.log")}
	out := &syncBuffer{}
	ffs := &faultFS{}
	clock := &fakeClock{t: time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)}

	res := mustBuild(t, cfg, out, false, func(o *rotateOptions) {
		o.FS = ffs
		o.Now = clock.Now
		o.Location = time.UTC
	})

	res.Logger.Debug().Msg("dbg")
	res.Logger.Error().Msg("err")
	res.Logger.WithLevel(zerolog.FatalLevel).Msg("fatal-record") // WithLevel: no os.Exit

	if console := out.String(); strings.Contains(console, "dbg") || strings.Contains(console, `"err"`) || !strings.Contains(console, "fatal-record") {
		t.Errorf("console with off = %q, want only the fatal record", console)
	}

	ffs.set(func(f *faultFS) { f.failWrites = syscall.ENOSPC })
	res.Logger.Info().Msg("dropped")

	if !strings.Contains(out.String(), "log file is failing") {
		t.Error("log-file failure notice suppressed by console_level off")
	}

	ffs.set(func(f *faultFS) { f.failWrites = nil })
	_ = res.Closer.Close()

	data, _ := os.ReadFile(segmentOf(t, dir))
	for _, m := range []string{"dbg", "err", "fatal-record"} {
		if !strings.Contains(string(data), m) {
			t.Errorf("file missing %q", m)
		}
	}
}

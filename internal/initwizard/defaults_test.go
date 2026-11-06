package initwizard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/logging"
)

func TestFreshDefaultsEnableRotatingJSONLogsAtInfo(t *testing.T) {
	t.Setenv("READY_LOG_LEVEL", "")
	var output strings.Builder
	opts := baseOptions(freshInterviewAnswer+"n\n", true, &output)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	opts.ReadyGuideCheck = func() bool { return true }
	outcome, err := Run(t.Context(), opts)
	if err != nil || !outcome.Committed {
		t.Fatalf("init: committed=%v err=%v", outcome.Committed, err)
	}
	cfg, err := config.Load(t.Context(), outcome.Resolution.Path)
	if err != nil {
		t.Fatal(err)
	}
	logsDir := filepath.Join(outcome.Resolution.AgentDir, "logs")
	if _, statErr := os.Stat(logsDir); !os.IsNotExist(statErr) {
		t.Fatal("init must save logging settings without starting logging")
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "json" || cfg.Logging.FilePath != filepath.Join(logsDir, "ready.log") {
		t.Fatalf("wrong logging defaults: level=%s format=%s path=%s", cfg.Logging.Level, cfg.Logging.Format, cfg.Logging.FilePath)
	}
	r := cfg.Logging.FileRotation
	if r.EffectiveRetentionDays() != 7 || r.EffectiveMaxSegmentSize() != 10<<20 || r.EffectiveMaxTotalSize() != 100<<20 {
		t.Fatal("log rotation must keep seven calendar days within the configured size limits")
	}
	// Exercise the real logging consumer, including level filtering and the
	// dated file scheme, using the settings published by init.
	result, err := logging.New(cfg.Logging)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Closer.Close() })
	result.Logger.Debug().Msg("hidden-debug-record")
	result.Logger.Info().Msg("visible-info-record")
	if closeErr := result.Closer.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	files, err := filepath.Glob(filepath.Join(logsDir, "ready-*.log"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one dated log file: files=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("hidden-debug-record")) {
		t.Fatal("INFO logging included a DEBUG event")
	}
	var record struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("log is not a JSON event: %v", err)
	}
	if record.Level != "info" || record.Message != "visible-info-record" {
		t.Fatalf("wrong log event: %+v", record)
	}
	assertSkillAndConversationDefaults(t, cfg, outcome.Resolution.AgentDir)
	if !strings.Contains(output.String(), firstChatPrompt) || !strings.Contains(output.String(), "logging.level: info") {
		t.Fatal("wizard omitted default explanations or its starter chat prompt")
	}
}

func assertSkillAndConversationDefaults(t *testing.T, cfg *config.Config, agentDir string) {
	t.Helper()
	if cfg.Skills.Sync != config.SkillsSyncAuto || !cfg.Skills.HotReload || filepath.Clean(cfg.Skills.Path) != filepath.Join(agentDir, "skills") {
		t.Fatal("skill synchronization/reloading defaults were not applied")
	}
	if filepath.Clean(cfg.Sessions.Path) != filepath.Join(agentDir, "sessions") || cfg.Sessions.Reset.Mode != config.ResetModeDaily || cfg.Sessions.Reset.DailyAt != "04:00" || !cfg.Sessions.Reset.NotifyEnabled() {
		t.Fatal("conversation directory or daily reset defaults were not applied")
	}
	if cfg.Sessions.Retention.Days() != 30 || cfg.Sessions.Retention.MaxTotalSize != 200<<20 || !cfg.Compaction.AutoEnabled() {
		t.Fatal("conversation retention or context summary defaults were not applied")
	}
}

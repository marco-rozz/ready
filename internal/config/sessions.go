package config

import (
	"fmt"
	"strings"
	"time"
)

// Session reset modes (session-lifecycle).
const (
	ResetModeNever     = "never"
	ResetModeDaily     = "daily"
	ResetModeIdle      = "idle"
	ResetModeDailyIdle = "daily+idle"
)

// Session and compaction defaults (session-lifecycle the design doc). The
// sessions directory default is runtime-home-anchored (ApplyRuntimeHome —
// <runtime home>/sessions), not a cwd-relative literal.
const (
	defaultResetDailyAt          = "04:00"
	defaultRetentionDays         = 30
	defaultRetentionTotalSize    = ByteSize(200 << 20)
	defaultCompactionSkill       = "home-compaction"
	defaultReserveTokens         = 16384
	defaultKeepRecentTokens      = 20000
	defaultCompactionMaxTurns    = 20
	defaultCompactionKeepTurns   = 5
	defaultCompactionTimeout     = 180 * time.Second
	sessionResetDailyAtLayout    = "15:04"
	sessionResetFieldPrefix      = "sessions.reset"
	channelSessionResetFieldTmpl = "channels.%s.session_reset"
)

// SessionsConfig holds the conversation Session settings: where Session
// files live, the reset rule, and the retention of ended Sessions.
type SessionsConfig struct {
	// Path is the root of the per-address Session directories
	// (<path>/<address>/<timestamp>_<uuidv7>.jsonl). Default: <runtime home>/sessions/ (resolved by config.ApplyRuntimeHome).
	Path string `yaml:"path"`
	// Reset is the global Session reset rule; channels may override it with
	// their own session_reset block.
	Reset SessionResetConfig `yaml:"reset"`
	// Retention bounds the ended Sessions kept on disk.
	Retention SessionRetentionConfig `yaml:"retention"`
}

// SessionResetConfig is a Session reset rule. Every field is optional; a
// channel override replaces only the fields it sets.
type SessionResetConfig struct {
	// Mode is one of never, daily, idle, daily+idle. Default daily.
	Mode string `yaml:"mode"`
	// DailyAt is the local HH:MM daily boundary. Default 04:00.
	DailyAt string `yaml:"daily_at"`
	// IdleAfter is the idle window after the last Turn (idle modes only).
	IdleAfter time.Duration `yaml:"idle_after"`
	// Notify sends the fixed reset notice on message-oriented channels.
	// Default true.
	Notify *bool `yaml:"notify"`
}

// NotifyEnabled reports the effective notify flag (default true).
func (r SessionResetConfig) NotifyEnabled() bool {
	return r.Notify == nil || *r.Notify
}

// SessionRetentionConfig bounds the ended Sessions kept on disk.
type SessionRetentionConfig struct {
	// RetentionDays keeps ended Sessions whose name's day falls within the
	// last N calendar days, today included. Default 30; 0 deletes an ended
	// Session at its next pass.
	RetentionDays *int `yaml:"retention_days"`
	// MaxTotalSize caps the total size of ended Sessions across addresses
	// (oldest deleted first). Default 200MB.
	MaxTotalSize ByteSize `yaml:"max_total_size"`
}

// Days returns the effective retention window in days.
func (r SessionRetentionConfig) Days() int {
	if r.RetentionDays == nil {
		return defaultRetentionDays
	}

	return *r.RetentionDays
}

// CompactionConfig holds the context-compaction settings (the architecture decision records).
type CompactionConfig struct {
	// Enabled toggles automatic compaction (default true). /compact works
	// regardless.
	Enabled *bool `yaml:"enabled"`
	// Skill names the Compaction skill whose SKILL.md is the summariser's
	// system prompt. Default home-compaction.
	Skill string `yaml:"skill"`
	// ReserveTokens is the room left for the next Turn below the smallest
	// declared context_window. Default 16384.
	ReserveTokens int `yaml:"reserve_tokens"`
	// KeepRecentTokens is the verbatim tail kept by a token-mode compaction.
	// Default 20000.
	KeepRecentTokens int `yaml:"keep_recent_tokens"`
	// MaxTurns triggers a compaction when no candidate declares a
	// context_window. Default 20.
	MaxTurns int `yaml:"max_turns"`
	// KeepRecentTurns is the verbatim tail kept by a turn-count compaction.
	// Default 5.
	KeepRecentTurns int `yaml:"keep_recent_turns"`
	// Timeout bounds the summariser call. Default 180s — sized for local
	// models (Gemma 31B on Apple Silicon needs minutes for a long summary).
	Timeout time.Duration `yaml:"timeout"`
}

// AutoEnabled reports whether automatic compaction is on (default true).
func (c CompactionConfig) AutoEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// applySessionDefaults fills the sessions and compaction defaults. The
// sessions path default lives in ApplyRuntimeHome (it needs the runtime
// home); applySessionDefaults only fills the rule knobs.
func (c *Config) applySessionDefaults() {
	s := &c.Sessions

	if s.Reset.Mode == "" {
		s.Reset.Mode = ResetModeDaily
	}

	if s.Reset.DailyAt == "" {
		s.Reset.DailyAt = defaultResetDailyAt
	}

	if s.Retention.MaxTotalSize == 0 {
		s.Retention.MaxTotalSize = defaultRetentionTotalSize
	}

	cp := &c.Compaction
	if cp.Skill == "" {
		cp.Skill = defaultCompactionSkill
	}

	if cp.ReserveTokens == 0 {
		cp.ReserveTokens = defaultReserveTokens
	}

	if cp.KeepRecentTokens == 0 {
		cp.KeepRecentTokens = defaultKeepRecentTokens
	}

	if cp.MaxTurns == 0 {
		cp.MaxTurns = defaultCompactionMaxTurns
	}

	if cp.KeepRecentTurns == 0 {
		cp.KeepRecentTurns = defaultCompactionKeepTurns
	}

	if cp.Timeout == 0 {
		cp.Timeout = defaultCompactionTimeout
	}
}

// channelSessionResets maps each channel name to its optional override.
func (c *Config) channelSessionResets() map[string]*SessionResetConfig {
	return map[string]*SessionResetConfig{
		"cli":      c.Channels.CLI.SessionReset,
		"zmqcli":   c.Channels.ZMQCLI.SessionReset,
		"serial":   c.Channels.Serial.SessionReset,
		"telegram": c.Channels.Telegram.SessionReset,
	}
}

// SessionResetFor returns the effective reset rule of a channel config key
// (cli, zmqcli, serial, telegram): the global rule with the
// channel's override fields applied on top.
func (c *Config) SessionResetFor(channelKey string) SessionResetConfig {
	rule := c.Sessions.Reset
	if rule.Mode == "" {
		rule.Mode = ResetModeDaily
	}

	if rule.DailyAt == "" {
		rule.DailyAt = defaultResetDailyAt
	}

	override := c.channelSessionResets()[channelKey]
	if override == nil {
		return rule
	}

	if override.Mode != "" {
		rule.Mode = override.Mode
	}

	if override.DailyAt != "" {
		rule.DailyAt = override.DailyAt
	}

	if override.IdleAfter != 0 {
		rule.IdleAfter = override.IdleAfter
	}

	if override.Notify != nil {
		rule.Notify = override.Notify
	}

	return rule
}

// validateSessions checks the sessions and compaction blocks.
func (c *Config) validateSessions() error {
	if err := validateSessionReset(sessionResetFieldPrefix, c.Sessions.Reset); err != nil {
		return err
	}

	for key := range c.channelSessionResets() {
		if c.channelSessionResets()[key] == nil {
			continue
		}

		if err := validateSessionReset(fmt.Sprintf(channelSessionResetFieldTmpl, key), c.SessionResetFor(key)); err != nil {
			return err
		}
	}

	if d := c.Sessions.Retention.Days(); d < 0 {
		return fmt.Errorf("sessions.retention.retention_days must be >= 0, got %d", d) // user-facing config error
	}

	cp := c.Compaction
	for name, v := range map[string]int{
		"compaction.reserve_tokens":     cp.ReserveTokens,
		"compaction.keep_recent_tokens": cp.KeepRecentTokens,
		"compaction.max_turns":          cp.MaxTurns,
		"compaction.keep_recent_turns":  cp.KeepRecentTurns,
	} {
		if v < 0 {
			return fmt.Errorf("%s must be a positive integer, got %d", name, v) // user-facing config error
		}
	}

	if cp.Timeout < 0 {
		return fmt.Errorf("compaction.timeout must be a positive duration, got %s", cp.Timeout) // user-facing config error
	}

	if cp.KeepRecentTurns >= cp.MaxTurns && cp.MaxTurns > 0 {
		return fmt.Errorf("compaction.keep_recent_turns (%d) must be lower than compaction.max_turns (%d)", cp.KeepRecentTurns, cp.MaxTurns) // user-facing config error
	}

	return nil
}

// validateSessionReset checks one (effective) reset rule.
func validateSessionReset(field string, r SessionResetConfig) error {
	switch r.Mode {
	case "", ResetModeNever, ResetModeDaily, ResetModeIdle, ResetModeDailyIdle:
	default:
		return fmt.Errorf("%s.mode must be one of never, daily, idle, daily+idle, got %q", field, r.Mode) // user-facing config error
	}

	if r.DailyAt != "" {
		if _, err := time.Parse(sessionResetDailyAtLayout, r.DailyAt); err != nil {
			return fmt.Errorf("%s.daily_at must be a local HH:MM time, got %q", field, r.DailyAt) // user-facing config error
		}
	}

	if r.IdleAfter < 0 {
		return fmt.Errorf("%s.idle_after must be a positive duration, got %s", field, r.IdleAfter) // user-facing config error
	}

	if strings.Contains(r.Mode, ResetModeIdle) && r.IdleAfter == 0 {
		return fmt.Errorf("%s.idle_after is required when mode is %q", field, r.Mode) // user-facing config error
	}

	return nil
}

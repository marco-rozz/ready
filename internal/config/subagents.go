package config

import (
	"fmt"
	"time"
)

// Subagent defaults (openspec change subagent-runtime, D13). The runtime
// path default is runtime-home-anchored (ApplyRuntimeHome):
// <runtime home>/subagents. The library default mirrors the skills library
// root: <program storage>/library/subagents.
const (
	SubagentsSyncAuto   = "auto"
	SubagentsSyncNotify = "notify"
	SubagentsSyncOff    = "off"

	defaultSubagentsMaxDepth              = 1
	defaultSubagentsMaxConcurrent         = 4
	defaultSubagentsMaxConcurrentJobs     = 1
	defaultSubagentsMailboxBuffer         = 32
	defaultSubagentsWaitTimeout           = 60 * time.Second
	defaultSubagentsHandbackMaxTokens     = 4096
	defaultSubagentsHandbackTurnBudgetTok = 16384
)

// SubagentsConfig holds the Subagent Runtime settings (subagent-runtime
// D13). `enabled` gates USER-FACING delegation only: the `subagent_*` tool
// surface and channel-owner spawns. Internal jobs are infrastructure and
// always available, each governed by the flag of the feature that launches
// it.
type SubagentsConfig struct {
	// Enabled gates user-facing delegation (default false).
	Enabled *bool `yaml:"enabled"`
	// MaxDepth is the nesting cap: 1 (default) or 2. At the cap a child's
	// catalog carries no subagent_* tool at all; at 2 the owner's children
	// may spawn dispatch-scoped grandchildren.
	MaxDepth *int `yaml:"max_depth"`
	// MaxConcurrent caps running dispatches per owner Session
	// (grandchildren included). Default 4.
	MaxConcurrent int `yaml:"max_concurrent"`
	// MaxConcurrentJobs caps the daemon-wide concurrent jobs. Default 1.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs"`
	// MailboxBuffer is each child's bounded FIFO capacity. Default 32.
	MailboxBuffer int `yaml:"mailbox_buffer"`
	// WaitTimeout is subagent_wait's default blocking window. Default 60s.
	WaitTimeout time.Duration `yaml:"wait_timeout"`
	// HandbackMaxTokens caps a handback's report (token-estimated).
	// Default 4096.
	HandbackMaxTokens int `yaml:"handback_max_tokens"`
	// HandbackTurnBudgetTokens bounds the handbacks injected at the head of
	// one owner Turn (further capped at 25% of the smallest declared
	// context window). Default 16384.
	HandbackTurnBudgetTokens int `yaml:"handback_turn_budget_tokens"`
	// Path is the definitions directory, inside the agent directory.
	// Default <runtime home>/subagents/.
	Path string `yaml:"path"`
	// Sync is the library-sync policy for the definitions (like skills.sync).
	// Default auto.
	Sync string `yaml:"sync"`
	// LibraryPath is the shipped Subagent-definition library the runtime
	// copy is synced from (default ~/.local/share/ready/library/subagents/).
	LibraryPath string `yaml:"library_path"`
}

// EnabledOn reports the effective enabled flag (default false).
func (s SubagentsConfig) EnabledOn() bool {
	return s.Enabled != nil && *s.Enabled
}

// EffectiveMaxDepth returns the depth cap (1–2).
func (s SubagentsConfig) EffectiveMaxDepth() int {
	if s.MaxDepth == nil || *s.MaxDepth <= 0 {
		return defaultSubagentsMaxDepth
	}

	return *s.MaxDepth
}

// SyncMode returns the effective library-sync mode: "auto" when unset.
func (s SubagentsConfig) SyncMode() string {
	if s.Sync == "" {
		return SubagentsSyncAuto
	}

	return s.Sync
}

// validateSubagents checks the block invariants; the error names the field.
func validateSubagents(s SubagentsConfig) error {
	if d := s.EffectiveMaxDepth(); d < 1 || d > 2 {
		return fmt.Errorf("subagents.max_depth must be 1 or 2 (resolved %d)", d) // user-facing config error
	}

	if s.MaxDepth != nil && (*s.MaxDepth < 1 || *s.MaxDepth > 2) {
		return fmt.Errorf("subagents.max_depth must be 1 or 2, got %d", *s.MaxDepth) // user-facing config error
	}

	if s.MaxConcurrent < 0 {
		return fmt.Errorf("subagents.max_concurrent must be non-negative, got %d", s.MaxConcurrent) // user-facing config error
	}

	if s.MaxConcurrentJobs < 0 {
		return fmt.Errorf("subagents.max_concurrent_jobs must be non-negative, got %d", s.MaxConcurrentJobs) // user-facing config error
	}

	if s.MailboxBuffer < 0 {
		return fmt.Errorf("subagents.mailbox_buffer must be non-negative, got %d", s.MailboxBuffer) // user-facing config error
	}

	if s.WaitTimeout < 0 {
		return fmt.Errorf("subagents.wait_timeout must be non-negative, got %s", s.WaitTimeout) // user-facing config error
	}

	if s.HandbackMaxTokens < 0 {
		return fmt.Errorf("subagents.handback_max_tokens must be non-negative, got %d", s.HandbackMaxTokens) // user-facing config error
	}

	if s.HandbackTurnBudgetTokens < 0 {
		return fmt.Errorf("subagents.handback_turn_budget_tokens must be non-negative, got %d", s.HandbackTurnBudgetTokens) // user-facing config error
	}

	switch s.Sync {
	case "", SubagentsSyncAuto, SubagentsSyncNotify, SubagentsSyncOff:
	default:
		return fmt.Errorf("subagents.sync must be one of auto, notify, off, got %q", s.Sync) // user-facing config error
	}

	return nil
}

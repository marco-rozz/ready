// dependencies.go — the daemon-provided environment of the Subagent
// Runtime (openspec change subagent-runtime, D1/D14/D15): everything a
// child dispatch needs from the active runtime generation, behind narrow
// function seams so internal/subagent never imports cmd.
package subagent

import (
	"context"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/compaction"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/usage"
)

// GenEnv is one generation's dispatch environment, handed out under a
// generation lease (D14). The child resolves its runner set, catalog and
// skills from it; the release func ends the lease.
type GenEnv struct {
	// ChainRefs is the compiled candidate chain's references, in priority
	// order (for model-pin validation).
	ChainRefs []string

	// OwnerTools is the generation's owner catalog under the active profile
	// (post-policy) — the base the child catalog is derived from (D12).
	OwnerTools []tool.BaseTool

	// BootstrapSkills are the owner path's bootstrap skill names (D19:
	// home-integration when HA is enabled) preloaded into the child prompt.
	BootstrapSkills []string

	// ExecuteTurn runs one turn on the generation's orchestrator. The child
	// fills TurnRequest.Runners (its runner set), .Only (a model pin) and
	// .Priority (its slot class); the owner fills none of them.
	ExecuteTurn func(ctx context.Context, req orchestrator.TurnRequest, onChunk func(string)) (agent.TurnOutcome, error)

	// RunnerSet builds (or returns the memoised) per-candidate runner set
	// for a definition snapshot (D17): one runner per chain candidate built
	// for the child's catalog and iteration cap, keyed by candidate ref.
	// pinnedModel ("" = the full chain) must be a chain member; a pin
	// outside the chain is the fixed structural error.
	RunnerSet func(ctx context.Context, def Definition, pinnedModel string, tools []tool.BaseTool) (map[string]agent.Runner, error)
}

// SkillsService creates and releases the child's agentskills session and
// renders its skill catalog (D19). cmd wires it to the bootstrapper and the
// skills manager.
type SkillsService interface {
	// Session returns (creating) the child's agentskills session id.
	Session(ctx context.Context, childSessionID string) (string, error)
	// MetadataPrompt renders the skill catalog metadata for the session.
	MetadataPrompt(ctx context.Context, sid string) (string, error)
	// LoadSkill marks a skill loaded/Active for the session (preloading).
	LoadSkill(ctx context.Context, sid, name string) error
	// SkillBody loads a skill's SKILL.md content through the skill cache.
	SkillBody(ctx context.Context, name string) (string, bool)
	// Close releases the child's agentskills session (closed with the
	// child, never with a dispatch).
	Close(ctx context.Context, childSessionID string) error
}

// ChildCompaction carries the child's compaction parameters (the owner's,
// D2) and the summariser entry point already attributed to the caller's
// accumulator by cmd's closure.
type ChildCompaction struct {
	// Enabled mirrors compaction.enabled.
	Enabled bool
	// KeepRecentTokens / KeepRecentTurns mirror the compaction config.
	KeepRecentTokens int
	KeepRecentTurns  int
	// Window / Reserve / MaxTurns mirror the compaction trigger config
	// (the owner's parameters, D2).
	Window   int
	Reserve  int
	MaxTurns int
	// Summarise runs one summariser call under the Compaction skill; the
	// cmd-side closure attributes usage to the accumulator carried in ctx.
	Summarise func(ctx context.Context, req compaction.Request) (string, error)
}

// Deps is the Runtime's daemon-provided environment, wired once at boot.
type Deps struct {
	Logger zerolog.Logger

	// Limits are the structural caps.
	Limits Limits
	// WaitTimeout is subagent_wait's default blocking window.
	WaitTimeout time.Duration
	// HandbackMaxTokens caps a handback report (token-estimated).
	HandbackMaxTokens int
	// HandbackTurnBudgetTokens bounds the per-Turn delivery budget.
	HandbackTurnBudgetTokens int
	// Enabled gates user-facing delegation (channel owners); jobs always
	// run.
	Enabled bool

	// Generation acquires the active generation's dispatch environment
	// under a lease (D14): the release func MUST be called at dispatch end.
	Generation func(ctx context.Context) (GenEnv, func())

	// Skills is the child agentskills/skills service.
	Skills SkillsService

	// ChildHistoryFactory returns (creating) a child's transcript history
	// for the child folder path — cmd owns the ConversationHistory
	// construction so logger wiring stays daemon-side. The Runtime owns the
	// returned history object (not the owner's LRU store).
	ChildHistoryFactory func(ctx context.Context, path, childSessionID string) (*persistence.ConversationHistory, error)

	// ChildCompaction carries the child compaction parameters (nil = no
	// child compaction).
	ChildCompaction *ChildCompaction

	// OwnerUsage returns the owner Session's live accumulator for the
	// child's forwarding accumulator (nil owner = unattributed child).
	OwnerUsage func(ownerSessionID string) *usage.Accumulator

	// Severity maps a dispatch failure to the error-digest severity class
	// (the daemon classifier; "" when unclassified).
	Severity func(err error) string

	// Redact renders prompt text with the daemon's path redaction (the
	// child prompt carries skill bodies).
	Redact func(text string) string

	// JobSessionFactory builds a job owner Session at the reserved address
	// `system` (task 5b: the layout lives daemon-side).
	JobSessionFactory func(ctx context.Context, jobID string, now time.Time) (Owner, error)

	// Notify delivers the passive-policy pending-results notice of a
	// channel owner (D6c): one fixed no-LLM message per idle period. nil =
	// no notice (stdin CLI, tests).
	Notify func(ctx context.Context, owner Owner, pendingCount int)

	// FleetChanged fires (coalescing happens in the wiring) whenever a
	// fleet or pending-handback change should emit the `subagents` snapshot
	// event (D11). nil = no snapshots (non-ZMQ, tests).
	FleetChanged func(ownerSessionID string)

	// HandbackDelivered fires on every handback delivery to a channel
	// owner (immediately, no idle-period latch): the connected TUI learns
	// the Subagent finished without waiting for the owner's next turn.
	// elapsedMS is the dispatch's wall-clock duration in milliseconds
	// (protocol 6, subagent-panel-observability D5); 0 = unknown — the
	// wiring omits the wire field.
	// nil = no notice (jobs, non-ZMQ, tests).
	HandbackDelivered func(ownerSessionID, name, status string, elapsedMS int64)
}

// Package subagent implements the daemon-first Subagent Runtime (openspec
// change subagent-runtime): spawn, per-child mailbox, structured handback,
// depth/concurrency limits, the reopenable lifecycle and delivery-time scan
// behind a Go API whose every operation names an Owner — the owner Session's
// ID, its Conversation address, its kind and its handback delivery policy.
// The LLM tools (subagent_spawn/send/wait/list/interrupt) are one door into
// this package; internal daemon jobs (RunJob) are the other.
//
// The Runtime is process-lifetime: it sits outside the reloadable runtime
// generation and child dispatches hold generation leases instead of the
// session worker's gate (design D14).
package subagent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/marco-rozz/ready/internal/persistence"
)

// OwnerKind identifies who owns a Subagent hierarchy. `channel` owners are
// channel Sessions (passive handback delivery); `job` owners are internal
// daemon jobs at the reserved address `system` (callback delivery). The
// address `main` is reserved for the autonomous owner kind of the parked
// change autonomous-agent — no `main` Session is ever created here.
type OwnerKind string

const (
	// OwnerChannel is a channel Session owner.
	OwnerChannel OwnerKind = "channel"
	// OwnerJob is an internal job owner at the reserved address `system`.
	OwnerJob OwnerKind = "job"
)

// Reserved Conversation addresses: `system` hosts job-owner Sessions, `main`
// is the autonomous owner's reservation (unimplemented here). AddressOf can
// never yield either (pinned by a session-package test).
const (
	AddressSystem = "system"
	AddressMain   = "main"
)

// Owner identifies the Session a Subagent hierarchy hangs from. History is
// the owner Session's transcript — the durable handback queue — accessed
// through the narrow History interface so the runtime never depends on a
// concrete store.
type Owner struct {
	// SessionID is the owner Session's UUID.
	SessionID string
	// Address is the owner's Conversation address ("cli",
	// "telegram-<chat_id>", or the reserved "system" for jobs).
	Address string
	// Kind is the owner kind; it selects the delivery policy semantics.
	Kind OwnerKind
	// History is the owner Session's transcript (nil only in tests that
	// never record).
	History History
	// Delivery is the owner's handback delivery policy.
	Delivery Delivery
}

// DeliveryKind selects the handback delivery policy.
type DeliveryKind string

const (
	// DeliveryPassive is the channel policy: pending handbacks wait for the
	// owner's next Turn; a deterministic no-LLM notice may be sent (D6c).
	DeliveryPassive DeliveryKind = "passive"
	// DeliveryCallback is the job policy: every handback — whatever its
	// status — is delivered exactly once to a Go callback (D20).
	DeliveryCallback DeliveryKind = "callback"
)

// Delivery is the owner's handback delivery policy.
type Delivery struct {
	Kind DeliveryKind
	// Callback is the job policy's handback sink; nil for passive owners.
	Callback func(ctx context.Context, hb Handback)
}

// History is the narrow owner-transcript surface the runtime needs: the
// detached bookkeeping append (handback records), the closed check, and the
// owner file's path for reconciliation. Implemented by
// persistence.ConversationHistory (compile-time asserted in history.go).
type History interface {
	// AppendBookkeeping appends a record line (a handback) without flushing
	// the view's unsaved entries; it refuses a closed Session.
	AppendBookkeeping(ctx context.Context, rec persistence.HistoryMessage) error
	// AppendRecord appends a full record (a job's closing record), flushing
	// the unsaved view first.
	AppendRecord(ctx context.Context, rec persistence.HistoryMessage) error
	// RestoredUsage returns the usage rebuilt by the last restore from the
	// file's turn_usage and compaction records (the D9 subtree walk's owner
	// side).
	RestoredUsage() persistence.UsageRecord
	// Closed reports whether the Session's closing record was written.
	Closed() bool
	// Path returns the Session file path ("" when memory-only).
	Path() string
}

// Status is a Subagent's lifecycle status. A child runs one dispatch per
// input and stays addressable after it (reopenable lifecycle, D4).
type Status string

const (
	// StatusRunning marks a Subagent with a dispatch in flight.
	StatusRunning Status = "running"
	// StatusDone marks a Subagent whose last dispatch ended normally.
	StatusDone Status = "done"
	// StatusFailed marks a Subagent whose last dispatch failed.
	StatusFailed Status = "failed"
	// StatusInterrupted marks a Subagent whose dispatch was cancelled.
	StatusInterrupted Status = "interrupted"
)

// HandbackStatus is the outcome class of one dispatch.
type HandbackStatus string

const (
	// HandbackCompleted: the dispatch delivered a final answer.
	HandbackCompleted HandbackStatus = "completed"
	// HandbackIterationLimit: the dispatch hit the definition's iteration
	// cap; the last pre-action text is the partial result.
	HandbackIterationLimit HandbackStatus = "iteration_limit"
	// HandbackFailed: the dispatch failed (severity class carried).
	HandbackFailed HandbackStatus = "failed"
	// HandbackInterrupted: the dispatch was cancelled (subagent_interrupt,
	// owner end, or restart).
	HandbackInterrupted HandbackStatus = "interrupted"
	// HandbackNothingDelivered: the model produced neither content nor a
	// tool call.
	HandbackNothingDelivered HandbackStatus = "nothing_delivered"
)

// UsageSnapshot is the informational usage block of a handback (never a
// rebuild input for owner totals — design D9).
type UsageSnapshot struct {
	ReadTokens    int           `json:"read_tokens,omitempty"`
	WrittenTokens int           `json:"written_tokens,omitempty"`
	LLMCalls      int           `json:"llm_calls,omitempty"`
	Duration      time.Duration `json:"-"`
}

// Handback is the structured result of one dispatch — never the child's
// transcript (design D6). Every field is data the owner model or the job
// launcher can act on.
type Handback struct {
	// HandbackID is unique per handback (handback-<uuid>).
	HandbackID string `json:"handback_id"`
	// DispatchID is child Session ID + dispatch sequence.
	DispatchID string `json:"dispatch_id"`
	// SubagentID is the child's Subagent ID (its child Session ID).
	SubagentID string `json:"subagent_id"`
	// Name is the child's name in the owner's fleet.
	Name string `json:"name"`
	// Status is the outcome class.
	Status HandbackStatus `json:"status"`
	// Report is the raw child report capped at handback_max_tokens
	// (token-estimated). For iteration_limit it is the fixed note plus the
	// last pre-action text.
	Report string `json:"report"`
	// Severity is the error-digest class for failed handbacks
	// ("transient" | "permanent"; empty otherwise).
	Severity string `json:"severity,omitempty"`
	// Usage is informational.
	Usage UsageSnapshot `json:"usage,omitempty"`
	// CreatedAt is the handback's arrival time.
	CreatedAt time.Time `json:"created_at"`
}

// Mail is one owner message queued for a running child (design D7).
type Mail struct {
	Text      string
	Scanned   string // the scan-mutated text delivered to the child
	QueuedAt  time.Time
	OwnerName string // the sender (for logs; the owner sends, never a peer)
}

// dispatchSeq formats a dispatch id: child Session ID + dispatch sequence.
func dispatchSeq(childSessionID string, n int) string {
	return fmt.Sprintf("%s-%d", childSessionID, n)
}

// nameRE is the Subagent name rule: lowercase letters, digits and hyphens.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// reservedNames are unusable as Subagent names.
//
//nolint:gochecknoglobals // reserved-name set
var reservedNames = map[string]bool{"main": true, "owner": true}

// ValidName reports whether name satisfies the naming rule.
func ValidName(name string) bool {
	return nameRE.MatchString(name) && !reservedNames[name]
}

// Structural errors — fixed, never silent, returned at spawn/send/wait time.
var (
	// ErrDepthLimit: a spawn would exceed subagents.max_depth.
	ErrDepthLimit = errors.New("subagent depth limit reached")
	// ErrConcurrencyLimit: a spawn or reopen exceeds
	// subagents.max_concurrent running dispatches for the owner.
	ErrConcurrencyLimit = errors.New("subagent concurrency limit reached")
	// ErrMailboxFull: the child's bounded mailbox is full (backpressure).
	ErrMailboxFull = errors.New("subagent mailbox full")
	// ErrNameConflict: a spawn named an existing Subagent of the owner.
	ErrNameConflict = errors.New("subagent name already in use")
	// ErrUnknownAgent: the addressed Subagent does not exist (or the name is
	// ambiguous).
	ErrUnknownAgent = errors.New("unknown subagent")
	// ErrAgentEnded: the addressed Subagent's hierarchy ended (its owner
	// Session closed or was cascade-closed).
	ErrAgentEnded = errors.New("subagent has ended")
	// ErrFeatureDisabled: a channel-owner operation was requested while
	// subagents.enabled is false (jobs remain available).
	ErrFeatureDisabled = errors.New("subagents feature is disabled")
	// ErrModelPinOutsideChain: a definition or spawn named a model reference
	// outside the compiled candidate chain.
	ErrModelPinOutsideChain = errors.New("model pin is not a member of the LLM candidate chain")
	// ErrJobsCap: more than subagents.max_concurrent_jobs jobs running.
	ErrJobsCap = errors.New("too many concurrent jobs")
	// ErrParentDispatchEnded: a depth-2 child's parent dispatch ended; the
	// child is never reopened.
	ErrParentDispatchEnded = errors.New("parent dispatch ended")
	// ErrNotRunning: an interrupt was requested for a child with no dispatch
	// in flight.
	ErrNotRunning = errors.New("subagent is not running")
	// ErrDefinitionUnknown: a spawn named a type with no loaded definition.
	ErrDefinitionUnknown = errors.New("unknown subagent type")
)

// Convenience structural error constructors (fixed English product strings
// naming the limit).
func depthLimitError(limit int) error {
	return fmt.Errorf("%w (max_depth=%d)", ErrDepthLimit, limit)
}

func concurrencyLimitError(limit int) error {
	return fmt.Errorf("%w (max_concurrent=%d)", ErrConcurrencyLimit, limit)
}

func jobsCapError(limit int) error {
	return fmt.Errorf("%w (max_concurrent_jobs=%d)", ErrJobsCap, limit)
}

// mailboxFullError names the child and the buffer size.
func mailboxFullError(name string, buf int) error {
	return fmt.Errorf("%w: %s holds %d queued messages", ErrMailboxFull, name, buf)
}

// nameConflictError names the child.
func nameConflictError(name string) error {
	return fmt.Errorf("%w: %q (use subagent_send instead)", ErrNameConflict, name)
}

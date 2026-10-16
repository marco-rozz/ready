// Package orchestrator implements the daemon-global LLM candidate-chain
// orchestrator (openspec change llm-failover-orchestrator).
//
// It owns the immutable ordered candidate chain compiled from llm.default +
// llm.failover, per-candidate health (two behavioural classes — attemptable
// and cooling — plus a `verified` flag for observability), the two cooldown
// ladders, and atomic promotion only after a real success. Candidate
// selection happens at the agent.Runner boundary: one runner per
// candidate is built at boot and chosen between per attempt, so a chain that
// mixes wire protocols needs no transcript conversion and is never filtered
// by protocol.
//
// A retry restarts the turn from the assemble closure (persisted history +
// verbatim user message + per-layout turn context) and is permitted only
// while the turn reports no started tool call and no delivered
// final-answer content (the llm-outage-delivery TurnOutcome booleans).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/usage"
	"github.com/rs/zerolog"
)

// State is the global orchestrator state. Unavailable means no candidate is
// currently healthy (all constructed candidates are cooling or failed
// construction); the daemon still runs and keeps probing on the recovery
// path.
type State string

const (
	// StateAvailable: at least one candidate is healthy and one is active.
	StateAvailable State = "available"
	// StateUnavailable: no candidate is healthy.
	StateUnavailable State = "unavailable"
)

// recoveryTickInterval is how often the background recovery worker wakes to
// check cooldown expiry. Probes themselves are bounded by
// Recovery.ProbeTimeout; a short tick keeps recovery latency low without
// busy-looping (a tick that finds nothing expired is a few mutex ops).
const recoveryTickInterval = time.Second

// subscriberBufferSize bounds the transition channel; a slow consumer never
// blocks the state machine — overflow drops the transition.
const subscriberBufferSize = 32

// Candidate is one immutable entry of the candidate chain as built by the
// daemon: the registry descriptor plus everything needed to use it. A
// candidate whose construction failed carries a nil Runner/Aux/Model and a
// non-nil ConstructErr; it is excluded from every selection but remains
// visible in snapshots.
type Candidate struct {
	llm.CandidateDescriptor

	// Runner is the production agent.Runner for this candidate (protocol-
	// blind: *agent.Agent for chat protocols, *agent.AgenticAgent for
	// `responses`).
	Runner agent.Runner

	// Aux is the model.BaseChatModel used by the auxiliary consumers
	// (formatter post-step, compaction summariser) for this candidate.
	Aux model.BaseChatModel

	// Model is the built ResolvedModel, kept for readiness-probe
	// construction (the probe builds its own transient runner over it).
	Model llm.ResolvedModel

	// ConstructErr is the per-candidate construction failure (build or
	// credential), logged as WARN at boot; nil when construction succeeded.
	ConstructErr error
}

// Config configures an Orchestrator. Candidates, Registry, Recovery and
// AttemptTimeout come from the resolved daemon configuration; Clock is
// injectable for deterministic tests.
type Config struct {
	// Candidates is the ordered candidate chain, `[default, failover...]`.
	Candidates []Candidate

	// Registry is the LLM provider registry, used by the boot catalog
	// preflight.
	Registry *config.LLMConfig

	// Recovery is the resolved (defaults-merged) recovery configuration.
	Recovery config.LLMRecoveryConfig

	// AttemptTimeout is the time-to-first-token budget for streaming
	// attempts. Zero explicitly disables the liveness budget.
	AttemptTimeout time.Duration

	// Retries is the number of same-candidate retries for RetrySame failures.
	Retries int

	// RetryBackoff is the exponential retry base; RetryBackoffCap bounds it.
	RetryBackoff    time.Duration
	RetryBackoffCap time.Duration

	// Clock returns "now"; nil means time.Now. Tests inject a fake.
	Clock func() time.Time
}

// health is the mutable per-candidate state. Two behavioural classes:
// attemptable (!cooling) and cooling; `verified` records whether a readiness
// probe or real attempt has ever succeeded and never changes control flow.
type health struct {
	cooling       bool
	cooldownUntil time.Time
	ladder        llm.Ladder
	demotions     int
	verified      bool
}

// unproven reports whether this candidate has never been verified and is not
// cooling — never probed, never successfully attempted.
func (h health) unproven() bool {
	return !h.verified && !h.cooling
}

type candidateState struct {
	cand   Candidate
	health health
}

// turnState tracks one turn's (or one aux call's) walk of the chain.
type turnState struct {
	order     []int // indexes of constructed candidates, in priority order
	attempted map[int]bool
	forced    bool // forced attempt (through a cooldown) already used
}

// Orchestrator is the daemon-global LLM candidate orchestrator. All mutable
// state lives under mu; immutable candidate descriptors are set at
// construction and never change. In-flight turns are never migrated: each
// turn walks the chain itself, and promotion only affects future selections.
type Orchestrator struct {
	mu     sync.Mutex
	cands  []candidateState
	active int // index of the active candidate; -1 when none
	state  State

	lastTraffic time.Time

	subs []chan Transition

	cfg    Config
	logger zerolog.Logger
	clock  func() time.Time

	wg sync.WaitGroup
}

// New creates an Orchestrator. All candidates start attemptable and
// unproven; Boot performs the lazy readiness evaluation that selects the
// boot-active candidate.
func New(cfg Config, logger zerolog.Logger) *Orchestrator {
	if cfg.Retries > 0 && cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 2 * time.Second
	}
	if cfg.RetryBackoffCap <= 0 {
		cfg.RetryBackoffCap = 30 * time.Second
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	cands := make([]candidateState, len(cfg.Candidates))
	for i, cand := range cfg.Candidates {
		cands[i] = candidateState{cand: cand}
	}

	o := &Orchestrator{
		cands:  cands,
		active: -1,
		state:  StateUnavailable,
		cfg:    cfg,
		logger: logger.With().Str("component", "orchestrator").Logger(),
		clock:  clock,
	}

	return o
}

// Start launches the background recovery worker. It stops when ctx is
// cancelled; use Wait to join.
func (o *Orchestrator) Start(ctx context.Context) {
	o.wg.Add(1)

	go func() {
		defer o.wg.Done()

		ticker := time.NewTicker(recoveryTickInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.recoveryTick(ctx)
			}
		}
	}()
}

// Wait blocks until the background recovery worker has stopped.
func (o *Orchestrator) Wait() {
	o.wg.Wait()
}

// ── Turn execution (main agent) ───────────────────────────────────────────

// TurnRequest carries the unassembled turn parts (task 5.2): the assemble
// closure receives the attempting candidate's prompt layout and returns the
// full conversation for that attempt. It is invoked once per attempt so a
// candidate's llm.PromptLayout is always the one its messages were built
// for.
//
// ResumeReady (session-transcript-persistence §D10) reports, from the live
// in-memory session transcript, that no tool-call group is open — every
// recorded tool call has its recorded result — so a retry or failover can
// resume from the last complete group without re-executing a tool. nil keeps
// the legacy gate: not resume-ready once a tool call started.
type TurnRequest struct {
	// SessionKey is the dispatcher's partitioned session key ("cli",
	// "telegram-<chat_id>"), forwarded verbatim to every attempt's runner.
	SessionKey  string
	Assemble    func(layout llm.PromptLayout) []*schema.Message
	ResumeReady func() bool
}

// unsafeRestart reports whether a failed attempt forbids any further attempt
// of the turn: an open tool-call group (or, without a ResumeReady predicate,
// any tool activity), or final-answer content already delivered on any
// channel.
func (r TurnRequest) unsafeRestart(outcome agent.TurnOutcome) bool {
	if outcome.ContentDelivered {
		return true
	}

	if r.ResumeReady == nil {
		return outcome.ToolCallStarted || outcome.ToolCallCompleted
	}

	return !r.ResumeReady()
}

// mergeAttempt folds the tool-activity flags of every attempt into outcome:
// a resumed turn whose later attempt ran no tool still reports the tools an
// earlier attempt ran, so terminal-message selection stays conservative.
func mergeAttempt(acc *agent.TurnOutcome, outcome agent.TurnOutcome) agent.TurnOutcome {
	acc.ToolCallStarted = acc.ToolCallStarted || outcome.ToolCallStarted
	acc.ToolCallCompleted = acc.ToolCallCompleted || outcome.ToolCallCompleted

	outcome.ToolCallStarted = acc.ToolCallStarted
	outcome.ToolCallCompleted = acc.ToolCallCompleted

	return outcome
}

// ExecuteTurn is the cmd-facing shape of RunTurn (the turnExecutor seam in
// cmd/serve.go). The orchestrator itself is the executor when wired; tests
// and non-orchestrator wiring use a plain-runner adapter instead.
func (o *Orchestrator) ExecuteTurn(
	ctx context.Context,
	sessionKey string,
	assemble func(llm.PromptLayout) []*schema.Message,
	resumeReady func() bool,
	onChunk func(string),
) (agent.TurnOutcome, error) {
	return o.RunTurn(ctx, TurnRequest{SessionKey: sessionKey, Assemble: assemble, ResumeReady: resumeReady}, onChunk)
}

// RunTurn executes one agent turn, walking the ordered candidate chain per
// the design doc/D3: each healthy candidate gets one retry cycle, in declared
// order, while the turn is resume-ready (no open tool-call group — see
// TurnRequest.ResumeReady) and no final-answer content was delivered. Each
// attempt re-assembles from the live history, so a retry continues from the
// last complete tool-call group. A successful attempt atomically promotes
// its candidate. An exhausted chain returns an error satisfying
// errors.Is(err, llm.ErrChainExhausted) wrapping the last candidate's error.
func (o *Orchestrator) RunTurn(ctx context.Context, req TurnRequest, onChunk func(string)) (agent.TurnOutcome, error) {
	o.markTraffic()

	ts := &turnState{order: o.constructedOrder(), attempted: make(map[int]bool)}

	var (
		lastErr       error
		lastOutcome   agent.TurnOutcome
		attemptedRefs []string
		acc           agent.TurnOutcome // tool activity across attempts
	)

	for {
		i := o.nextCandidate(ts)
		if i < 0 {
			break
		}

		untried := o.countUntried(ts)
		ts.attempted[i] = true

		cand := o.candidateCopy(i)
		attemptedRefs = append(attemptedRefs, cand.Ref)

		candidateCtx, candidateCancel, candidateBudgetHit := candidateCycleContext(ctx, onChunk == nil, untried, o.cfg.AttemptTimeout)

		advance := false
		for attempt := 0; attempt <= o.cfg.Retries; attempt++ {
			if attempt > 0 && o.candidateCooling(i) {
				advance = true

				break
			}

			o.logger.Debug().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Int("attempt", attempt+1).
				Msg("LLM candidate turn attempt")

			conversation := req.Assemble(cand.Layout)
			outcome, err := o.attemptRunner(candidateCtx, cand, req.SessionKey, conversation, onChunk)
			outcome = mergeAttempt(&acc, outcome)
			if err == nil {
				candidateCancel()
				o.recordSuccess(i, true, "turn")

				return outcome, nil
			}

			lastErr, lastOutcome = err, outcome

			// Parent cancellation/deadline is a property of the turn, not the
			// candidate. Never retry, advance, or demote for it.
			if ctx.Err() != nil {
				candidateCancel()

				return outcome, err
			}

			if candidateBudgetHit() {
				err = fmt.Errorf("%w: %w", llm.ErrAttemptBudgetExceeded, err)
				lastErr = err
			}

			cls := o.classifyFailure(cand, err)
			if !cls.Advance || req.unsafeRestart(outcome) {
				if cls.Demote {
					o.demote(i, cand, cls, "turn-failure")
				}
				candidateCancel()

				return outcome, err
			}

			if !cls.RetrySame || attempt >= o.cfg.Retries || candidateBudgetHit() {
				if cls.Demote {
					o.demote(i, cand, cls, "turn-failure")
				}
				advance = true

				break
			}

			// A concurrent turn may have demoted this candidate while this turn
			// was running. Respect the new global cooldown instead of retrying
			// through it.
			if o.candidateCooling(i) {
				advance = true

				break
			}

			backoff := retryBackoff(o.cfg.RetryBackoff, o.cfg.RetryBackoffCap, attempt)
			o.logger.Warn().
				Str("model", cand.Ref).
				Str("api", cand.API).
				Int("attempt", attempt+2).
				Dur("backoff", backoff).
				Msg("LLM retrying same candidate in turn")

			if waitErr := waitRetry(candidateCtx, backoff); waitErr != nil {
				if ctx.Err() != nil {
					candidateCancel()

					return outcome, err
				}

				budgetErr := fmt.Errorf("%w: %w", llm.ErrAttemptBudgetExceeded, err)
				budgetClass := o.classifyFailure(cand, budgetErr)
				o.demote(i, cand, budgetClass, "turn-retry-budget-exhausted")
				lastErr = budgetErr
				advance = true

				break
			}
		}

		candidateCancel()
		if !advance {
			break
		}
	}

	// Chain exhaustion (task 3.5): a distinct sentinel wrapping the last
	// candidate's error so errors.Is matches llm.ErrChainExhausted while
	// errors.Unwrap walks to the last candidate's error (spec: the underlying
	// cause stays inspectable).
	if lastErr == nil {
		lastErr = errors.New("no eligible LLM candidate") // wrapped into ErrChainExhausted below
	}

	o.logger.Warn().
		Strs("attempted", attemptedRefs).
		Err(lastErr).
		Msg("LLM candidate chain exhausted for turn")

	return lastOutcome, &ChainExhaustedError{Attempted: attemptedRefs, Cause: lastErr}
}

// attemptRunner runs one candidate's runner under the attempt budget: on a
// streaming call (onChunk != nil) the budget is time-to-first-token — armed
// until the first chunk arrives, then disarmed so only the outer deadline
// applies; on a non-streaming call the budget is a proportional
// share of the outer deadline's remaining time divided by the candidates
// still untried this turn, enforced as a real context deadline so a hung
// candidate cannot consume the turn.
func (o *Orchestrator) attemptRunner(
	ctx context.Context,
	cand Candidate,
	sessionKey string,
	conversation []*schema.Message,
	onChunk func(string),
) (agent.TurnOutcome, error) {
	if onChunk == nil {
		return cand.Runner.RunMessagesStreaming(ctx, sessionKey, conversation, nil)
	}

	actx, cancel, budgetHit, stopTimer := cancelBudgetContext(ctx, o.cfg.AttemptTimeout)
	defer cancel()
	actx = agent.WithLiveness(actx, stopTimer)

	outcome, err := cand.Runner.RunMessagesStreaming(actx, sessionKey, conversation, onChunk)

	// If the attempt budget fired while the outer context is still alive,
	// the failure is an attempt-budget expiry (hard class) regardless of the
	// underlying error text.
	if err != nil && budgetHit() && ctx.Err() == nil {
		err = fmt.Errorf("%w: %w", llm.ErrAttemptBudgetExceeded, err)
	}

	return outcome, err
}

// candidateCycleContext allocates one proportional deadline to a complete
// non-streaming candidate cycle (all attempts plus backoff). Streaming cycles
// use their parent context because each attempt owns its own TTFT budget.
func candidateCycleContext(ctx context.Context, nonStreaming bool, untried int, fallback time.Duration) (context.Context, context.CancelFunc, func() bool) {
	if !nonStreaming {
		return ctx, func() {}, func() bool { return false }
	}

	budget := proportionalShare(ctx, untried, fallback)
	if budget <= 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			return ctx, func() {}, func() bool { return false }
		}
	}

	bctx, cancel := context.WithTimeout(ctx, budget)

	return bctx, cancel, func() bool { return bctx.Err() != nil && ctx.Err() == nil }
}

func retryBackoff(base, maxDelay time.Duration, attempt int) time.Duration {
	d := base
	for i := 0; i < attempt && d < maxDelay; i++ {
		if d > maxDelay/2 {
			return maxDelay
		}
		d *= 2
	}
	if d > maxDelay {
		return maxDelay
	}

	return d
}

func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ChainExhaustedError is returned when a turn (or auxiliary call) has
// attempted every eligible candidate without success. It satisfies
// errors.Is(err, llm.ErrChainExhausted) and unwraps to the last candidate's
// error.
type ChainExhaustedError struct {
	Attempted []string
	Cause     error
}

// Error renders the sentinel, the attempted references, and the cause.
func (e *ChainExhaustedError) Error() string {
	return fmt.Sprintf("%s: attempted %v: %v", llm.ErrChainExhausted, e.Attempted, e.Cause)
}

// Is reports equivalence with llm.ErrChainExhausted so errors.Is matches the
// sentinel (task 3.5 / the design doc).
func (e *ChainExhaustedError) Is(target error) bool {
	return target == llm.ErrChainExhausted
}

// Unwrap exposes the last candidate's error.
func (e *ChainExhaustedError) Unwrap() error {
	return e.Cause
}

// proportionalShare computes a non-streaming attempt's budget: the outer
// deadline's remaining time divided by the candidates still untried (design
// D9). When ctx carries no deadline the flat AttemptTimeout applies.
func proportionalShare(ctx context.Context, untried int, fallback time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok || untried <= 0 {
		return fallback
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}

	return remaining / time.Duration(untried)
}

// cancelBudgetContext derives an attempt context from ctx with a
// disarmable budget timer (the streaming time-to-first-token path). The
// returned fired flag is set when (and only when) the budget timer — not the
// parent context — cancelled the attempt; stopTimer disarms the budget
// (first token/chunk arrived) without cancelling anything.
func cancelBudgetContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc, func() bool, func()) {
	if budget <= 0 {
		return ctx, func() {}, func() bool { return false }, func() {}
	}

	actx, cancel := context.WithCancel(ctx)
	var state atomic.Int32 // 0=armed, 1=fired, 2=disarmed

	timer := time.AfterFunc(budget, func() {
		if state.CompareAndSwap(0, 1) {
			cancel()
		}
	})

	stopTimer := sync.OnceFunc(func() {
		if state.CompareAndSwap(0, 2) {
			timer.Stop()
		}
	})

	return actx, cancel, func() bool { return state.Load() == 1 }, stopTimer
}

// ── Auxiliary consumers (formatter post-step, compaction summariser) ─────

// AuxModel returns a model.BaseChatModel whose Generate walks the candidate
// chain with the shared health policy and the proportional-share attempt
// budget (the design doc: both auxiliary consumers are non-streaming by nature).
// The formatter post-step call and the compaction summariser hold this
// model; their existing failure-degradation behaviour (llm-outage-delivery
// A5) is unchanged — an exhausted chain simply surfaces as an error.
func (o *Orchestrator) AuxModel() model.BaseChatModel {
	return &auxChatModel{o: o}
}

// auxChatModel adapts the orchestrator to model.BaseChatModel.
type auxChatModel struct {
	o *Orchestrator
}

// Guarantees auxChatModel satisfies model.BaseChatModel.
var _ model.BaseChatModel = (*auxChatModel)(nil)

// Generate walks the candidate chain for one auxiliary call. A call is
// atomic (no partial content, no tools), so every Advance-classified failure
// is retry-eligible on the next candidate.
//
// tui-usage-observability: the context is marked auxiliary so the model-layer
// usage recorder (internal/usage) counts these calls under AuxCalls instead
// of LLMCalls; the accumulator itself rides the caller's context (the turn
// context for formatter/classifier calls, absent for tests).
func (a *auxChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	o := a.o
	o.markTraffic()

	// Mark the call auxiliary for the usage recorder before the chain walk.
	ctx = usage.WithAux(ctx)

	ts := &turnState{order: o.constructedOrder(), attempted: make(map[int]bool)}

	var lastErr error

	for {
		i := o.nextCandidate(ts)
		if i < 0 {
			break
		}

		untried := o.countUntried(ts)
		ts.attempted[i] = true

		cand := o.candidateCopy(i)

		actx, cancel, budgetExpired := candidateCycleContext(ctx, true, untried, o.cfg.AttemptTimeout)

		msg, err := cand.Aux.Generate(actx, input, opts...)
		budgetHit := budgetExpired()
		cancel()

		if err == nil {
			o.recordSuccess(i, true, "aux")

			return msg, nil
		}

		// The auxiliary caller's own deadline/cancellation is not evidence that
		// the provider is unhealthy.
		if ctx.Err() != nil {
			return nil, err
		}

		if budgetHit {
			err = fmt.Errorf("%w: %w", llm.ErrAttemptBudgetExceeded, err)
		}

		lastErr = err

		cls := o.classifyFailure(cand, err)
		if cls.Demote {
			o.demote(i, cand, cls, "aux-failure")
		}

		if !cls.Advance || ctx.Err() != nil {
			return nil, err
		}
	}

	if lastErr == nil {
		lastErr = errors.New("no eligible LLM candidate") // wrapped into ErrChainExhausted below
	}

	o.logger.Warn().Err(lastErr).Msg("LLM candidate chain exhausted for auxiliary call")

	return nil, &ChainExhaustedError{Cause: lastErr}
}

// Stream serves a single chunk produced by Generate, then EOF. Neither
// auxiliary consumer needs incremental delivery; this exists only to satisfy
// model.BaseChatModel.
func (a *auxChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := a.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}

	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		if msg != nil {
			sw.Send(msg, nil)
		}
	}()

	return sr, nil
}

// ── Selection ─────────────────────────────────────────────────────────────

// constructedOrder returns the indexes of candidates whose construction
// succeeded, in priority order, under the lock.
func (o *Orchestrator) constructedOrder() []int {
	o.mu.Lock()
	defer o.mu.Unlock()

	order := make([]int, 0, len(o.cands))
	for i := range o.cands {
		if o.cands[i].cand.Runner != nil {
			order = append(order, i)
		}
	}

	return order
}

// nextCandidate selects the next candidate for a turn walk (task 3.2):
// the first unattempted candidate that is not within an unexpired cooldown,
// in declared order. When none qualifies, one forced attempt of the
// highest-priority unattempted candidate — through its cooldown — is
// allowed per turn; afterwards, or when every candidate has
// been attempted, selection is exhausted and returns -1.
func (o *Orchestrator) nextCandidate(ts *turnState) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := o.clock()

	for _, i := range ts.order {
		if ts.attempted[i] {
			continue
		}

		if o.cands[i].health.cooling && o.cands[i].health.cooldownUntil.After(now) {
			o.logger.Debug().
				Str("model", o.cands[i].cand.Ref).
				Time("cooldown_until", o.cands[i].health.cooldownUntil).
				Msg("LLM candidate skipped (cooling)")

			continue
		}

		return i
	}

	if ts.forced {
		return -1
	}

	for _, i := range ts.order {
		if !ts.attempted[i] {
			ts.forced = true
			o.logger.Warn().
				Str("model", o.cands[i].cand.Ref).
				Msg("LLM forced attempt: every candidate is cooling, attempting highest-priority candidate through its cooldown")

			return i
		}
	}

	return -1
}

// countUntried counts the candidates still untried in this turn's walk,
// including the one about to be attempted (spec: remaining deadline / untried
// candidates).
func (o *Orchestrator) countUntried(ts *turnState) int {
	count := 0
	for _, i := range ts.order {
		if !ts.attempted[i] {
			count++
		}
	}

	return count
}

// candidateCopy snapshots the immutable candidate at index i.
func (o *Orchestrator) candidateCopy(i int) Candidate {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.cands[i].cand
}

// candidateCooling reports whether another concurrent turn has demoted the
// candidate since this turn selected it. Local retries respect that global
// transition instead of bypassing a newly scheduled cooldown.
func (o *Orchestrator) candidateCooling(i int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.cands[i].health.cooling
}

// ── Health transitions ────────────────────────────────────────────────────

// markTraffic records agent traffic for the idle-probe window.
func (o *Orchestrator) markTraffic() {
	o.mu.Lock()
	o.lastTraffic = o.clock()
	o.mu.Unlock()
}

// recordSuccess resets the candidate's cooldown (backoff and verified flag
// included) and atomically promotes it (task 3.6): a candidate becomes the
// globally active reference only after one of its calls or probes succeeds.
// promote=false (a recovery probe of a lower-priority candidate than the
// current active) marks it healthy without changing the active reference —
// strict-priority recovery never demotes the active candidate.
func (o *Orchestrator) recordSuccess(i int, promote bool, reason string) {
	o.mu.Lock()

	prev := o.snapshotLocked()

	o.cands[i].health.cooling = false
	o.cands[i].health.demotions = 0
	o.cands[i].health.verified = true

	if promote && o.active != i {
		o.active = i
	}

	o.refreshStateLocked()
	activeChanged := o.active != prev.ActiveIndex
	o.mu.Unlock()

	// criticalities-remediation (M16): the promotion log and subscriber
	// transition reflect ONLY a real active-reference change. The old
	// `!wasActive` term logged a false "promoted to active" (and published a
	// no-op transition) for every non-promotional recovery-probe success of
	// a candidate that was never active.
	if activeChanged {
		o.logger.Info().
			Str("model", o.cands[i].cand.Ref).
			Str("api", o.cands[i].cand.API).
			Bool("verified", true).
			Str("reason", reason).
			Msg("LLM candidate promoted to active")

		o.emitTransition(prev, "promotion: "+reason)
	}
}

// demote marks candidates unhealthy and starts their cooldown (tasks 3.7,
// 3.8): the ladder's initial cooldown multiplied by the backoff factor once
// per successive demotion, bounded by the ladder's max, reset on success. A
// provider-scoped classification (quota exhaustion) demotes every candidate
// sharing the failing candidate's provider instance.
func (o *Orchestrator) demote(i int, cand Candidate, cls llm.Classification, reason string) {
	o.mu.Lock()

	prev := o.snapshotLocked()

	now := o.clock()

	targets := []int{i}
	if cls.ProviderScoped {
		targets = targets[:0]
		for j := range o.cands {
			if o.cands[j].cand.Provider == cand.Provider && o.cands[j].cand.Runner != nil {
				targets = append(targets, j)
			}
		}
	}

	for _, t := range targets {
		h := &o.cands[t].health
		h.demotions++
		h.cooling = true
		h.ladder = cls.Ladder

		cooldown := o.cooldownForLocked(cls.Ladder, h.demotions)
		h.cooldownUntil = now.Add(cooldown)

		o.logger.Warn().
			Str("model", o.cands[t].cand.Ref).
			Str("api", o.cands[t].cand.API).
			Str("ladder", cls.Ladder.String()).
			Dur("cooldown", cooldown).
			Time("cooldown_until", h.cooldownUntil).
			Int("consecutive_demotions", h.demotions).
			Bool("provider_scoped", cls.ProviderScoped).
			Bool("verified", h.verified).
			Str("reason", reason).
			Msg("LLM candidate demoted")
	}

	o.refreshStateLocked()
	stateChanged := prev.State != o.state
	o.mu.Unlock()

	if stateChanged {
		o.emitTransition(prev, "demotion: "+reason)
	}
}

// cooldownForLocked computes the cooldown for a demotion on the given
// ladder: initial * backoff^(demotions-1), bounded by the ladder max. The
// caller holds the lock.
func (o *Orchestrator) cooldownForLocked(ladder llm.Ladder, demotions int) time.Duration {
	var lc config.LadderConfig
	switch ladder {
	case llm.LadderSoft:
		lc = o.cfg.Recovery.Soft
	default:
		lc = o.cfg.Recovery.Hard
	}

	factor := o.cfg.Recovery.BackoffFactor
	if factor <= 1 {
		factor = 2
	}

	d := lc.InitialCooldown
	for k := 1; k < demotions; k++ {
		d = time.Duration(float64(d) * factor)
		if d >= lc.MaxCooldown {
			return lc.MaxCooldown
		}
	}

	if d > lc.MaxCooldown {
		d = lc.MaxCooldown
	}

	return d
}

// refreshStateLocked recomputes the global state: unavailable when no
// constructed candidate is attemptable (all cooling). The caller holds the
// lock.
func (o *Orchestrator) refreshStateLocked() {
	for i := range o.cands {
		if o.cands[i].cand.Runner != nil && !o.cands[i].health.cooling {
			o.state = StateAvailable

			return
		}
	}

	o.state = StateUnavailable
}

// ── Failure classification and logging ────────────────────────────────────

// classifyFailure logs the three-axis classification of a candidate failure
// (DEBUG, redaction-safe: no prompt or response text) and retains the
// diagnostic HTTP-400 WARN (task 2.6): a request-shape 400 is a bug in the
// request, not the candidate, so it neither advances nor demotes — but it is
// worth a distinct WARN naming the reference instead of silently repeating
// the broken shape every turn.
func (o *Orchestrator) classifyFailure(cand Candidate, err error) llm.Classification {
	cls := llm.ClassifyLLMError(err)

	o.logger.Debug().
		Str("model", cand.Ref).
		Str("api", cand.API).
		Bool("advance", cls.Advance).
		Bool("demote", cls.Demote).
		Bool("retry_same", cls.RetrySame).
		Str("ladder", cls.Ladder.String()).
		Bool("provider_scoped", cls.ProviderScoped).
		Err(err).
		Msg("LLM candidate failure classified")

	if !cls.Advance && !cls.Demote && llm.IsRequestShapeError(err) {
		o.logger.Warn().
			Str("model", cand.Ref).
			Err(err).
			Msg("LLM request rejected with HTTP 400 — likely a request-shape bug (e.g. an unsupported message role/order for this protocol), not a model failure; no failover advance performed")
	}

	return cls
}

// ── Background recovery ───────────────────────────────────────────────────

// recoveryTick is one pass of the background recovery worker (tasks 3.9,
// 3.10, 4.4): probes cooling candidates whose cooldown has expired, in
// strict configured order, and promotes the first healthy one — a recovered
// higher-priority candidate (including the default) replaces a lower
// priority active reference. Gated on agent traffic within the idle probe
// window. Unproven candidates are never probed here: they carry no cooldown
// and are verified by their first real use instead.
func (o *Orchestrator) recoveryTick(ctx context.Context) {
	if !o.trafficWithinIdleWindow() {
		return
	}

	for _, i := range o.coolingExpired() {
		// A cancelled context (shutdown) ends the tick without demoting
		// anyone — the probe never ran to completion.
		if ctx.Err() != nil {
			return
		}

		cand := o.candidateCopy(i)

		o.logger.Debug().
			Str("model", cand.Ref).
			Str("api", cand.API).
			Msg("LLM recovery probe starting")

		probeErr := o.probeCandidate(ctx, cand)
		if probeErr == nil {
			promote := o.shouldPromoteProbe(i)

			o.recordSuccess(i, promote, "recovery-probe")

			return // highest-priority healthy candidate restored; stop probing
		}

		o.logger.Warn().
			Str("model", cand.Ref).
			Str("api", cand.API).
			Err(probeErr).
			Msg("LLM recovery probe failed")

		// Classify the probe failure like a call failure where possible; a
		// readiness failure that matches no provider shape (e.g. the model
		// refused the synthetic tool call) is hard — the candidate does not
		// honour the tool-calling contract.
		cls := llm.ClassifyLLMError(probeErr)
		if !cls.Demote {
			cls = llm.Classification{Advance: true, Demote: true, Ladder: llm.LadderHard}
		}

		o.demote(i, cand, cls, "recovery-probe-failure")
	}
}

// trafficWithinIdleWindow reports whether agent traffic occurred within the
// idle probe window: an idle daemon never pays for probe round
// trips.
func (o *Orchestrator) trafficWithinIdleWindow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.lastTraffic.IsZero() {
		return false
	}

	return o.clock().Sub(o.lastTraffic) <= o.cfg.Recovery.IdleProbeWindow
}

// coolingExpired returns the indexes of cooling candidates whose cooldown
// has expired, in priority order, under the lock.
func (o *Orchestrator) coolingExpired() []int {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := o.clock()

	var expired []int
	for i := range o.cands {
		if o.cands[i].cand.Runner == nil {
			continue
		}
		if o.cands[i].health.cooling && !o.cands[i].health.cooldownUntil.After(now) {
			expired = append(expired, i)
		}
	}

	return expired
}

// shouldPromoteProbe reports whether a successful probe of candidate i
// should promote it to active: yes when it is higher priority than the
// current active, or when the orchestrator is unavailable (no healthy
// candidate to serve the next turn). Probing a lower-priority candidate
// than a healthy active never demotes the active reference.
func (o *Orchestrator) shouldPromoteProbe(i int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.state == StateUnavailable || i < o.active
}

// ── Snapshot and transitions ──────────────────────────────────────────────

// CandidateSnapshot is the observable state of one candidate.
type CandidateSnapshot struct {
	Ref           string           `json:"ref"`
	Provider      string           `json:"provider"`
	API           string           `json:"api"`
	Layout        llm.PromptLayout `json:"-"`
	Active        bool             `json:"active"`
	Cooling       bool             `json:"cooling"`
	CooldownUntil time.Time        `json:"cooldown_until"`
	Ladder        llm.Ladder       `json:"ladder"`
	Verified      bool             `json:"verified"`
	Unproven      bool             `json:"unproven"`
	ConstructErr  string           `json:"construct_err,omitempty"`
}

// Snapshot is the orchestrator's observable state (task 6.6): the global
// state, the active reference, and every candidate's health. Consumed by
// llm-status-visibility.
type Snapshot struct {
	State        State            `json:"state"`
	ActiveIndex  int              `json:"-"`
	ActiveRef    string           `json:"active_ref"`
	ActiveAPI    string           `json:"active_api"`
	ActiveLayout llm.PromptLayout `json:"-"`
	// ActiveContextWindow is the active candidate's configured context-window
	// size in tokens (0 = not configured) — the occupancy denominator for
	// /context (tui-usage-observability).
	ActiveContextWindow int                 `json:"-"`
	Candidates          []CandidateSnapshot `json:"candidates"`
}

// Transition is one observable state change, for subscribers.
type Transition struct {
	From   Snapshot `json:"from"`
	To     Snapshot `json:"to"`
	Reason string   `json:"reason"`
}

// Snapshot returns the current orchestrator state.
func (o *Orchestrator) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.snapshotLocked()
}

// snapshotLocked builds a Snapshot; the caller holds the lock.
func (o *Orchestrator) snapshotLocked() Snapshot {
	snap := Snapshot{
		State:       o.state,
		ActiveIndex: o.active,
	}

	if o.active >= 0 && o.active < len(o.cands) {
		snap.ActiveRef = o.cands[o.active].cand.Ref
		snap.ActiveAPI = o.cands[o.active].cand.API
		snap.ActiveLayout = o.cands[o.active].cand.Layout
		snap.ActiveContextWindow = o.cands[o.active].cand.Model.ContextWindow()
	}

	snap.Candidates = make([]CandidateSnapshot, len(o.cands))
	for i := range o.cands {
		c := &o.cands[i]
		cs := CandidateSnapshot{
			Ref:           c.cand.Ref,
			Provider:      c.cand.Provider,
			API:           c.cand.API,
			Layout:        c.cand.Layout,
			Active:        o.active == i,
			Cooling:       c.health.cooling,
			CooldownUntil: c.health.cooldownUntil,
			Ladder:        c.health.ladder,
			Verified:      c.health.verified,
			Unproven:      c.health.unproven() && c.cand.Runner != nil,
		}
		if c.cand.ConstructErr != nil {
			cs.ConstructErr = c.cand.ConstructErr.Error()
		}

		snap.Candidates[i] = cs
	}

	return snap
}

// ActiveLayout reports the prompt-layout capability of the active candidate
// (task 6.5), so callers that still need a single layout (logging, status)
// track promotions. Fails safe to LayoutHeadSafe when no candidate is
// active.
func (o *Orchestrator) ActiveLayout() llm.PromptLayout {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.active >= 0 && o.active < len(o.cands) {
		return o.cands[o.active].cand.Layout
	}

	return llm.LayoutHeadSafe
}

// Subscribe returns a channel of state transitions and a cancel function.
// The channel is buffered; overflow drops transitions rather than blocking
// the state machine. Cancel must be called to release the subscriber.
func (o *Orchestrator) Subscribe() (<-chan Transition, func()) {
	ch := make(chan Transition, subscriberBufferSize)

	o.mu.Lock()
	o.subs = append(o.subs, ch)
	o.mu.Unlock()

	cancel := sync.OnceFunc(func() {
		o.mu.Lock()
		for i, sub := range o.subs {
			if sub == ch {
				o.subs = append(o.subs[:i], o.subs[i+1:]...)

				break
			}
		}
		o.mu.Unlock()
	})

	return ch, cancel
}

// emitTransition delivers a transition to every subscriber. Called after the
// lock is released; delivery is non-blocking.
func (o *Orchestrator) emitTransition(prev Snapshot, reason string) {
	o.mu.Lock()
	to := o.snapshotLocked()
	subs := make([]chan Transition, len(o.subs))
	copy(subs, o.subs)
	o.mu.Unlock()

	tr := Transition{From: prev, To: to, Reason: reason}
	for _, ch := range subs {
		select {
		case ch <- tr:
		default: // drop rather than block the state machine
		}
	}
}

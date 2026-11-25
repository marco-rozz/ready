// subagent_runtime.go — the daemon wiring of the Subagent Runtime (openspec
// change subagent-runtime, tasks 7.3/3.2): the process-lifetime Runtime, its
// generation leases, the per-(generation, definition) child runner sets, the
// channel-owner construction from the tool context, the owner pre-turn
// handback-delivery hook, the passive notice state, and the five subagent_*
// tools.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/rs/zerolog"

	agentskillsSpec "github.com/flexigpt/agentskills-go/runtime/spec"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/injection"
	"github.com/marco-rozz/ready/internal/orchestrator"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/session"
	"github.com/marco-rozz/ready/internal/steer"
	"github.com/marco-rozz/ready/internal/subagent"
	"github.com/marco-rozz/ready/internal/usage"
)

// sessRootOf returns the sessions root of the shell's current boot config.
func sessRootOf(shell *serveShell) string {
	if shell == nil || shell.rt == nil {
		return ""
	}

	if c := shell.rt.currentBootConfig(); c != nil {
		return c.Sessions.Path
	}

	return ""
}

// subagentTypeList renders the public definition list for subagent_spawn's
// description (stable per generation, D3).
func subagentTypeList(defs *subagent.Definitions) string {
	pub := defs.PublicTypes()
	if len(pub) == 0 {
		return "none loaded"
	}

	parts := make([]string, 0, len(pub))
	for _, d := range pub {
		parts = append(parts, fmt.Sprintf("- %s: %s", d.Name, d.Description))
	}

	return strings.Join(parts, "\n")
}

// buildSubagentRuntime constructs the process-lifetime Runtime from the
// resolved configuration. Called once at boot, BEFORE the first generation:
// buildTools reads shell.subagents to register the subagent_* tools in the
// boot catalog. At this point the session runtime is a skeleton (the channel
// manager and the generation arrive later); every dependency that needs it
// is read lazily at dispatch time, except the job session root — falling
// back to the resolved config's sessions path.
func buildSubagentRuntime(shell *serveShell, cfg *config.SubagentsConfig, sessionsRoot string, logger zerolog.Logger) *subagent.Runtime {
	rt := shell.rt
	noticeState := newSubagentNoticeState()

	// One limiter serves every emission (change-driven and the Connection
	// attach snapshot) so the per-owner rate budget holds across both.
	lim := subagentFleetChanged(shell)
	rt.fleetLimiter = lim

	sessRoot := sessRootOf(shell)
	if sessRoot == "" {
		sessRoot = sessionsRoot
	}

	deps := subagent.Deps{
		Logger: logger,
		Limits: subagent.Limits{
			MaxDepth:          cfg.EffectiveMaxDepth(),
			MaxConcurrent:     cfg.MaxConcurrent,
			MaxConcurrentJobs: cfg.MaxConcurrentJobs,
			MailboxBuffer:     cfg.MailboxBuffer,
		},
		WaitTimeout:              cfg.WaitTimeout,
		HandbackMaxTokens:        cfg.HandbackMaxTokens,
		HandbackTurnBudgetTokens: cfg.HandbackTurnBudgetTokens,
		Enabled:                  cfg.EnabledOn(),
		Generation:               subagentGenerationEnv(rt),
		JobSessionFactory:        jobOwnerFactory(sessRoot, logger),
		Skills:                   &subagentSkillsService{rt: rt},
		ChildHistoryFactory:      subagentChildHistoryFactory(rt),
		ChildCompaction:          nil, // wired per generation (configureSessionRuntime)
		OwnerUsage: func(ownerSessionID string) *usage.Accumulator {
			return rt.usageStore.Accumulator(ownerSessionID)
		},
		Severity: func(err error) string {
			return string(errDigestClassifier().Classify(err))
		},
		Redact: func(text string) string {
			home := ""
			if c := rt.coord; c != nil {
				if g := c.currentGen(); g != nil && g.cfg != nil {
					home = g.cfg.RuntimeHome
				}
			}

			return privacy.FromRun(home, nil).Redact(text)
		},
		Notify: subagentPassiveNotice(rt, noticeState),
		// Nil only with a nil shell (never in production); the dep stays nil
		// then so a stray notify cannot dereference the limiter.
		FleetChanged: lim.notifyIf(),

		// The immediate handback notice: the connected TUI learns the
		// Subagent finished the moment the result is recorded (never a
		// delivery — the content lands at the head of the owner's next
		// turn).
		HandbackDelivered: func(ownerSessionID, name, status string, elapsedMS int64) {
			zmqCh := getZMQChannel(rt.mgr, zmqcli.ChannelName)
			if zmqCh == nil {
				return
			}

			if err := zmqCh.SendSubagentHandbackEvent(name, status, elapsedMS); err != nil {
				logger.Warn().Err(err).Str("owner_session", ownerSessionID).Str("subagent", name).
					Msg("failed to send the subagent handback event")
			}
		},
	}

	runtime := subagent.New(deps, subagent.NewDefinitions(nil))
	rt.subagents = runtime
	rt.subagentNotices = noticeState

	return runtime
}

// subagentGenerationEnv builds the D14 lease-backed GenEnv accessor: every
// child dispatch resolves its environment from the leased generation.
func subagentGenerationEnv(rt *sessionRuntime) func(ctx context.Context) (subagent.GenEnv, func()) {
	return func(ctx context.Context) (subagent.GenEnv, func()) {
		gen, release := rt.leases.Acquire(ctx)

		env := subagent.GenEnv{
			ChainRefs:       generationChainRefs(gen),
			OwnerTools:      gen.agentTools,
			BootstrapSkills: unionBootstrapSkills(gen),
			ExecuteTurn: func(ctx context.Context, req orchestrator.TurnRequest, onChunk func(string)) (agent.TurnOutcome, error) {
				return gen.agentReg.exec.ExecuteTurnRequest(ctx, req, onChunk)
			},
			RunnerSet: func(ctx context.Context, def subagent.Definition, pinnedModel string, tools []tool.BaseTool) (map[string]agent.Runner, error) {
				return gen.childRunnerSet(ctx, def, pinnedModel, tools, rt.logger)
			},
		}

		return env, release
	}
}

// generationChainRefs returns the generation's compiled chain references in
// priority order.
func generationChainRefs(gen *runtimeGeneration) []string {
	if gen == nil || gen.orch == nil {
		return nil
	}

	snap := gen.orch.Snapshot()
	refs := make([]string, 0, len(snap.Candidates))
	for _, c := range snap.Candidates {
		refs = append(refs, c.Ref)
	}

	return refs
}

// unionBootstrapSkills merges every channel's bootstrap skill list (the
// child prompt carries the owner path's bootstrap skills, D19).
func unionBootstrapSkills(gen *runtimeGeneration) []string {
	if gen == nil {
		return nil
	}

	seen := map[string]bool{}
	var out []string

	for _, list := range gen.bootstrapCfg {
		for _, s := range list {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}

	return out
}

// subagentSkillsService adapts the generation's skills machinery to the
// child prompt (D19). The generation is resolved fresh per call: the
// service is process-lifetime while skills managers are per generation.
type subagentSkillsService struct {
	rt *sessionRuntime
}

func (s *subagentSkillsService) gen() *runtimeGeneration {
	if s.rt != nil && s.rt.coord != nil {
		return s.rt.coord.currentGen()
	}

	return nil
}

func (s *subagentSkillsService) Session(ctx context.Context, childSessionID string) (string, error) {
	g := s.gen()
	if g == nil || g.bootstrapper == nil {
		return "", nil
	}

	sid, err := g.bootstrapper.GetOrCreateSession(ctx, childSessionID, "subagent")

	return string(sid), err
}

func (s *subagentSkillsService) MetadataPrompt(ctx context.Context, sid string) (string, error) {
	g := s.gen()
	if g == nil || g.skillsMgr == nil {
		return "", nil
	}

	return g.skillsMgr.GetMetadataPrompt(ctx, agentskillsSpec.SessionID(sid))
}

func (s *subagentSkillsService) LoadSkill(ctx context.Context, sid, name string) error {
	g := s.gen()
	if g == nil || g.skillsMgr == nil {
		return nil
	}

	_, err := g.skillsMgr.LoadSkill(ctx, agentskillsSpec.SessionID(sid), name)

	return err
}

func (s *subagentSkillsService) SkillBody(ctx context.Context, name string) (string, bool) {
	g := s.gen()
	if g == nil || g.skillsMgr == nil {
		return "", false
	}

	path, err := g.skillsMgr.SkillFilePath(name)
	if err != nil {
		return "", false
	}

	body, err := g.skillCache.Load(ctx, path)
	if err != nil {
		return "", false
	}

	return body, true
}

func (s *subagentSkillsService) Close(ctx context.Context, childSessionID string) error {
	g := s.gen()
	if g == nil || g.bootstrapper == nil {
		return nil
	}

	return g.bootstrapper.Close(ctx, childSessionID)
}

// subagentChildHistoryFactory builds child histories (0700/0600 through the
// SessionFile machinery, logger wired).
func subagentChildHistoryFactory(rt *sessionRuntime) func(ctx context.Context, path, childSessionID string) (*persistence.ConversationHistory, error) {
	return func(ctx context.Context, path, childSessionID string) (*persistence.ConversationHistory, error) {
		h := persistence.NewConversationHistory(childSessionID, persistence.NewSessionFile(path)).
			WithLogger(rt.logger.With().Str("component", "child-history").Str("child_session", childSessionID).Logger())

		if err := h.Restore(ctx); err != nil {
			rt.logger.Warn().Err(err).Str("child_session", childSessionID).Msg("failed to restore a child session history — continuing with what was recovered")
		}

		return h, nil
	}
}

// subagentNoticeState is the D6c latch: one fixed notice per idle period
// per owner Session, reset on consumption.
type subagentNoticeState struct {
	mu      sync.Mutex
	noticed map[string]bool
}

func newSubagentNoticeState() *subagentNoticeState {
	return &subagentNoticeState{noticed: map[string]bool{}}
}

// claim marks a notice sent (true when the caller should send it).
func (s *subagentNoticeState) claim(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.noticed[sessionID] {
		return false
	}

	s.noticed[sessionID] = true

	return true
}

// reset clears the latch (consumption).
func (s *subagentNoticeState) reset(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.noticed, sessionID)
}

// subagentPassiveNotice implements the D6c state machine. Only Telegram
// receives the fixed message; ZMQ carries the count in the snapshot; stdin
// CLI gets nothing.
func subagentPassiveNotice(rt *sessionRuntime, state *subagentNoticeState) func(ctx context.Context, owner subagent.Owner, pending int) {
	return func(ctx context.Context, owner subagent.Owner, pending int) {
		if pending <= 0 {
			return
		}

		if !state.claim(owner.SessionID) {
			return
		}

		channelName, meta := session.Route(session.Address(owner.Address))
		if channelName != telegram.ChannelName {
			return // ZMQ badge / stdin silence
		}

		rt.sendFixed(ctx, channel.Message{ChannelName: channelName, Metadata: meta}, msgSubagentResultsReady)
	}
}

// msgSubagentResultsReady is the fixed English notice of D6c.
const msgSubagentResultsReady = "Background results are ready — send any message to receive them."

// subagentsSnapshotInterval is the fleet-snapshot emitter's minimum
// interval per owner (at most four events per second).
const subagentsSnapshotInterval = 250 * time.Millisecond

// subagentFleetChanged wires the rate-limited `subagents` snapshot emission
// (D11, amended by subagent-panel-observability D2): the 250 ms budget is a
// MIN-INTERVAL rate limit with a GUARANTEED trailing state — a change
// arriving inside the window is never dropped: the owner is marked dirty
// and a one-shot timer flushes at last+interval, emitting a snapshot built
// from the registry's CURRENT state at flush time (a burst of N changes
// costs one immediate + one trailing emission carrying the final state;
// the failing leading-edge sequence was handback arrival (pending 1)
// followed within the window by its consumption (pending 0), which left
// the TUI badge stale forever — reproduced 2026-11-18).
// subagentFleetChanged returns the fleet-snapshot emitter for Deps.FleetChanged.
// The channel manager arrives AFTER the Subagent Runtime wiring (the runtime
// is wired before the first generation), so the nil check MUST be lazy —
// including on the trailing flush path — an eager check made FleetChanged
// nil for every fresh boot and the TUI panel never received a snapshot
// (reproduced 2026-11-16).
// subagentFleetChanged builds the fleet-change emitter shared by every
// change-driven notification AND the Connection attach snapshot (wire
// protocol, "Subagent fleet snapshot control event"): the attach emission
// runs through the same 250 ms limiter so an attach immediately followed by
// a fleet change coalesces correctly (an attach emission inside the window
// may serve as the flush). Returns the limiter so the connection-start hook
// can reach its notify; nil only without a shell (tests never are).
func subagentFleetChanged(shell *serveShell) *fleetLimiter {
	if shell == nil {
		return nil
	}

	return newFleetLimiter(func(ownerSessionID string) {
		// Lazy nil checks (see above): the channel manager may arrive after
		// this closure was wired, and the flush reads the registry's CURRENT
		// state at flush time.
		if rt := shell.rt; rt != nil && rt.mgr != nil {
			rt.emitSubagentsSnapshot(ownerSessionID)
		}
	})
}

// fleetLimiter is the min-interval rate limiter with guaranteed trailing
// state (D2), extracted from the shell wiring so its timing behaviour is
// unit-testable without a channel manager.
type fleetLimiter struct {
	emit func(ownerSessionID string)

	mu    sync.Mutex
	gates map[string]*ownerGate
}

type ownerGate struct {
	last  time.Time   // the last immediate-or-flush emission stamp
	timer *time.Timer // the armed trailing flush (nil when none)
}

func newFleetLimiter(emit func(ownerSessionID string)) *fleetLimiter {
	return &fleetLimiter{emit: emit, gates: map[string]*ownerGate{}}
}

// notifyIf returns the limiter's notify as a nil-safe dep value: nil limiter
// (a nil shell) yields a nil FleetChanged instead of a bound method that
// would dereference the nil receiver when fired.
func (l *fleetLimiter) notifyIf() func(ownerSessionID string) {
	if l == nil {
		return nil
	}

	return l.notify
}

// notify records a fleet change for one owner: an emission due outside the
// window goes out immediately; one inside the window arms (once) a trailing
// flush at last+interval that emits the CURRENT fleet state at flush time.
func (l *fleetLimiter) notify(ownerSessionID string) {
	l.mu.Lock()
	g, ok := l.gates[ownerSessionID]
	if !ok {
		g = &ownerGate{}
		l.gates[ownerSessionID] = g
	}

	now := time.Now()
	if !g.last.IsZero() && now.Sub(g.last) < subagentsSnapshotInterval {
		// Inside the window: mark the owner dirty and arm (once) the
		// trailing flush at last+interval.
		if g.timer == nil {
			deadline := g.last.Add(subagentsSnapshotInterval)
			g.timer = time.AfterFunc(time.Until(deadline), func() {
				l.mu.Lock()
				gNow, ok := l.gates[ownerSessionID]
				if !ok || gNow.timer == nil {
					// Disarmed (cancelled by an immediate emission) or
					// already flushed.
					l.mu.Unlock()

					return
				}
				gNow.timer = nil
				gNow.last = time.Now()
				l.mu.Unlock()

				l.emit(ownerSessionID)
			})
		}
		l.mu.Unlock()

		return
	}

	// Emission due and outside the window: emit now. A pending trailing
	// flush is subsumed (this emission already carries the current state),
	// so cancel it to keep emissions spaced ≥ interval even when the timer
	// goroutine is late.
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	g.last = now
	l.mu.Unlock()

	l.emit(ownerSessionID)
}

// maybeEmitAttachSnapshot emits the spec's attach snapshot: when a
// Connection attaches to a Session with Subagent history, exactly one
// snapshot goes out through the shared fleet limiter (so an attach +
// immediate change coalesce; the attach emission itself may serve as the
// flush). A pending reset pre-assigns a brand-new Session — no Subagent
// history by construction — and a history-less Session emits nothing
// (wire-protocol spec: a Session with no Subagent history never receives
// the event).
func (rt *sessionRuntime) maybeEmitAttachSnapshot(sess *session.Session) {
	if rt.subagents == nil || rt.fleetLimiter == nil || sess == nil {
		return
	}

	if !rt.sessionHasSubagentHistory(sess) {
		return
	}

	rt.fleetLimiter.notify(sess.ID)
}

// sessionHasSubagentHistory reports whether the owner Session carries
// Subagent history: the registry currently knows the owner (a live fleet
// or pending handbacks) or the Session folder has a subagents/ subtree —
// the durable trace of a fleet that already ended (boot restore). The
// owner dir comes from the existing resolver chain: the Session folder is
// the parent of the owner transcript file (session.Dir — the same
// directory ownerUsageWithSubtree derives from owner.History.Path()).
func (rt *sessionRuntime) sessionHasSubagentHistory(sess *session.Session) bool {
	if rt.subagents == nil {
		return false
	}

	if views, pending := rt.subagents.List(subagent.Owner{SessionID: sess.ID}); len(views) > 0 || pending > 0 {
		return true
	}

	info, err := os.Stat(filepath.Join(sess.Dir(), "subagents"))

	return err == nil && info.IsDir()
}

// emitSubagentsSnapshot pushes the `subagents` control event (fleet +
// pending count) to the connected ZMQ TUI, if any (D11).
func (rt *sessionRuntime) emitSubagentsSnapshot(ownerSessionID string) {
	if rt.subagents == nil {
		return
	}

	zmqCh := getZMQChannel(rt.mgr, zmqcli.ChannelName)
	if zmqCh == nil {
		return
	}

	views, pending := rt.subagents.List(subagent.Owner{SessionID: ownerSessionID})
	if err := zmqCh.SendSubagentsEvent(SubagentsInfoFromViews(views, pending)); err != nil {
		rt.logger.Warn().Err(err).Msg("failed to send the subagents snapshot event")
	}
}

// channelOwnerFromCtx builds the subagent Owner for a channel tool call:
// the current Session and its history (the durable handback queue).
func (rt *sessionRuntime) channelOwnerFromCtx(ctx context.Context) (subagent.Owner, error) {
	sessID, ok := ctx.Value(toolSessionKeyCtxKey{}).(string)
	if !ok || sessID == "" {
		return subagent.Owner{}, errors.New("no session in the tool context")
	}

	addr, _ := ctx.Value(toolSessionAddressCtxKey{}).(string)
	if addr == "" {
		addr = "cli"
	}

	cur := rt.sessions.Current(ctx, session.Address(addr))
	if cur == nil || cur.ID != sessID {
		return subagent.Owner{}, errors.New("the session is no longer current")
	}

	return subagent.Owner{
		SessionID: cur.ID,
		Address:   addr,
		Kind:      subagent.OwnerChannel,
		History:   rt.hist.For(ctx, cur),
		Delivery:  subagent.Delivery{Kind: subagent.DeliveryPassive},
	}, nil
}

// ── the owner pre-turn delivery hook (task 5.3, design D6) ──────────────

// deliverPendingHandbacks runs at the head of an owner Turn, BEFORE the
// user message is recorded (subagent-runtime task 5.3, design D6). The
// delivery logic itself lives in the Runtime (DeliverPending — tested
// there); this wrapper computes the per-Turn budget:
// min(handback_turn_budget_tokens, 25% of the smallest declared context
// window).
func (rt *sessionRuntime) deliverPendingHandbacks(ctx context.Context, owner subagent.Owner, history *persistence.ConversationHistory) {
	if rt.subagents == nil || history.Closed() {
		return
	}

	rt.subagents.DeliverPending(ctx, owner, history, rt.handbackBudgetTokens())
	rt.resetPendingNotice(owner.SessionID)
}

// handbackBudgetTokens computes the per-Turn delivery budget.
func (rt *sessionRuntime) handbackBudgetTokens() int {
	budget := 16384
	if rt.subagents != nil {
		budget = rt.subagents.Deps().HandbackTurnBudgetTokens
	}

	if orch := rt.liveOrch.Load(); orch != nil {
		if w := orch.SmallestContextWindow(); w > 0 {
			budget = min(budget, w/4)
		}
	}

	return budget
}

// ── jobs + boot restore (tasks 5b.1/5b.3/5.5) ────────────────────────────

// jobOwnerFactory builds a job owner Session at the reserved address
// `system` (subagent-runtime task 5b.1, D20): one Session per job, the
// child under its subagents/ subtree.
func jobOwnerFactory(sessRoot string, logger zerolog.Logger) func(_ context.Context, jobID string, now time.Time) (subagent.Owner, error) {
	return func(_ context.Context, jobID string, now time.Time) (subagent.Owner, error) {
		folder := now.Local().Format("2006-01-02T15-04-05") + "_" + jobID
		path := filepath.Join(sessRoot, subagent.AddressSystem, folder, "session.jsonl")

		h := persistence.NewConversationHistory(jobID, persistence.NewSessionFile(path)).
			WithLogger(logger.With().Str("component", "job-history").Str("job_session", jobID).Logger())

		return subagent.Owner{
			SessionID: jobID,
			Address:   subagent.AddressSystem,
			Kind:      subagent.OwnerJob,
			History:   h,
			// The delivery policy is completed by RunJob (the callback).
			Delivery: subagent.Delivery{Kind: subagent.DeliveryCallback},
		}, nil
	}
}

// closeStaleJobSessions closes every open `system` (job) Session tree at
// boot (task 5b.3): open tool-call groups closed with the fixed
// daemon-restart result, closing records with reason `daemon_restart` on
// every file of the job tree, NO callback (the launching feature
// re-triggers its job idempotently).
func closeStaleJobSessions(ctx context.Context, sessRoot string, logger zerolog.Logger) {
	sysDir := filepath.Join(sessRoot, subagent.AddressSystem)

	entries, err := os.ReadDir(sysDir)
	if err != nil {
		return // no jobs ever ran
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		folder := filepath.Join(sysDir, e.Name())

		// The owner file.
		closeOneJobFile(ctx, filepath.Join(folder, "session.jsonl"), logger)

		// The child tree (one dispatch, closed at boot like the owner).
		subDir := filepath.Join(folder, "subagents")
		if kids, kerr := os.ReadDir(subDir); kerr == nil {
			for _, kid := range kids {
				if kid.IsDir() {
					closeOneJobFile(ctx, filepath.Join(subDir, kid.Name(), "session.jsonl"), logger)
				}
			}
		}
	}
}

// closeOneJobFile closes one open job-tree file: interrupted results for an
// open group, then the closing record (reason daemon_restart).
func closeOneJobFile(ctx context.Context, path string, logger zerolog.Logger) {
	if path == "" {
		return
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}

	h := persistence.NewConversationHistory(filepath.Base(filepath.Dir(path)), persistence.NewSessionFile(path)).
		WithLogger(logger)

	if err := h.Restore(ctx); err != nil {
		logger.Warn().Err(err).Str("file", path).Msg("failed to read an open job file at boot")
	}

	if h.Closed() {
		return
	}

	if n := h.CloseOpenToolCallGroupWith(persistence.InterruptedRestartToolResult); n > 0 {
		logger.Warn().Str("file", path).Int("interrupted_results", n).Msg("an interrupted job left tool calls without results — interrupted results recorded")
	}

	now := time.Now()
	if err := h.AppendRecord(ctx, persistence.HistoryMessage{
		Kind:      persistence.KindSessionClosed,
		EndedAt:   &now,
		Reason:    "daemon_restart",
		Timestamp: now,
	}); err != nil {
		logger.Warn().Err(err).Str("file", path).Msg("failed to append a job tree closing record at boot")
	}
}

// restoreSubagentFleet runs the boot restore + reconciliation (task 5.5):
// for every still-current owner Session (enabled only), reconcile its child
// folders against the owner file and rebuild the pending set. No dispatch
// starts, no LLM traffic.
func restoreSubagentFleet(ctx context.Context, rt *sessionRuntime, sessRoot string, enabled bool, _ zerolog.Logger) {
	if rt.subagents == nil || !enabled || sessRoot == "" {
		return
	}

	addressDirs, err := os.ReadDir(sessRoot)
	if err != nil {
		return
	}

	for _, addrDir := range addressDirs {
		if !addrDir.IsDir() || addrDir.Name() == subagent.AddressSystem {
			continue
		}

		// The directory name IS the address for every realistic address
		// (cli, telegram-<chat id>): DirName's sanitisation is an identity
		// on them.
		addr := session.Address(addrDir.Name())

		cur := rt.sessions.Current(ctx, addr)
		if cur == nil {
			continue // a closed newest folder: nothing to reconcile
		}

		owner := subagent.Owner{
			SessionID: cur.ID,
			Address:   string(addr),
			Kind:      subagent.OwnerChannel,
			History:   rt.hist.For(ctx, cur),
			Delivery:  subagent.Delivery{Kind: subagent.DeliveryPassive},
		}

		rep := rt.subagents.ReconcileOwner(ctx, owner)

		// Usage attribution restore (task 4.4, D9): owner totals = the owner
		// file's records + the subtree walk (each child file exactly once).
		if rep.Children > 0 {
			if totals, uerr := rt.ownerUsageWithSubtree(ctx, owner); uerr == nil {
				rt.usageStore.Seed(cur.ID, totals)
			} else {
				rt.logger.Warn().Err(uerr).Str("session_id", cur.ID).Msg("failed the subagent usage walk at restore")
			}

			// The restored fleet's snapshot reaches an attaching TUI.
			rt.emitSubagentsSnapshot(cur.ID)
		}
	}
}

// ownerUsageWithSubtree sums the owner file's usage records with every
// child file's `turn_usage` + compaction usage exactly once (D9's restore
// walk; children run only after the owner's history is loaded — children
// are spawned by owner Turns).
func (rt *sessionRuntime) ownerUsageWithSubtree(ctx context.Context, owner subagent.Owner) (usage.Totals, error) {
	ownerPath := owner.History.Path()
	if ownerPath == "" {
		return usage.Totals{}, errors.New("owner history has no file")
	}

	total := owner.History.RestoredUsage()

	subDir := filepath.Join(filepath.Dir(ownerPath), "subagents")
	if folders, err := os.ReadDir(subDir); err == nil {
		for _, f := range folders {
			if !f.IsDir() {
				continue
			}

			childPath := filepath.Join(subDir, f.Name(), "session.jsonl")
			rec, rerr := persistence.ReadUsage(ctx, childPath)
			if rerr != nil {
				continue
			}

			total = total.Add(rec)
		}
	}

	return totalsFromRecord(total), nil
}

// SubagentsInfoFromViews maps the runtime's fleet view to the wire payload.
func SubagentsInfoFromViews(views []subagent.AgentView, pending int) protocol.SubagentsInfo {
	info := protocol.SubagentsInfo{
		Agents:           make([]protocol.SubagentsAgentView, 0, len(views)),
		PendingHandbacks: pending,
	}

	for _, v := range views {
		info.Agents = append(info.Agents, protocol.SubagentsAgentView{
			SubagentID: v.SubagentID,
			Name:       v.Name,
			Type:       v.Type,
			Status:     string(v.Status),
			StartedAt:  v.StartedAt.Format(time.RFC3339),
			QueuedMail: v.QueuedMail,
			Parent:     v.Parent,
		})
	}

	return info
}

// ── the five tools (task 7.1) ────────────────────────────────────────────

// subagentToolArgs are the shared tool arguments.
type subagentToolArgs struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Task    string   `json:"task"`
	Text    string   `json:"text"`
	Targets []string `json:"targets"`
	Timeout int      `json:"timeout"`
	Model   string   `json:"model"`
}

// newSubagentTools builds the five tools over the Runtime. They are
// registered under builtin_tools (deferred-loading eligible) and resolve
// their owner from the invocation context.
func newSubagentTools(runtime *subagent.Runtime, rt *sessionRuntime, logger zerolog.Logger) []tool.BaseTool {
	return []tool.BaseTool{
		newSubagentSpawnTool(runtime, logger),
		newSubagentSendTool(runtime, logger),
		newSubagentWaitTool(runtime, rt, logger),
		newSubagentListTool(runtime, logger),
		newSubagentInterruptTool(runtime, logger),
	}
}

// unmarshalToolArgs decodes a tool's JSON arguments.
func unmarshalToolArgs(args string, v any) error {
	if strings.TrimSpace(args) == "" {
		return nil
	}

	if err := json.Unmarshal([]byte(args), v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}

	return nil
}

// rtFromCtx returns the sessionRuntime stashed in the tool context by the
// dispatcher (wired in processMessage alongside the other tool context
// values).
func rtFromCtx(ctx context.Context) *sessionRuntime {
	if rt, ok := ctx.Value(subagentRuntimeCtxKey{}).(*sessionRuntime); ok {
		return rt
	}

	return nil
}

type subagentRuntimeCtxKey struct{}

// spawnTool implements subagent_spawn.
type spawnTool struct {
	runtime *subagent.Runtime
	logger  zerolog.Logger
}

func newSubagentSpawnTool(r *subagent.Runtime, logger zerolog.Logger) *spawnTool {
	return &spawnTool{runtime: r, logger: logger}
}

func (t *spawnTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "subagent_spawn",
		Desc: "Spawn a Subagent to work on a task in the background and return immediately. " +
			"The Subagent reports back with a structured result you receive via subagent_wait or at the start of your next turn.\n" +
			"Available types:\n" + subagentTypeList(t.runtime.Definitions()),
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"type":  {Type: schema.String, Desc: "Subagent type (see the list); empty = general-purpose"},
			"name":  {Type: schema.String, Desc: "Optional unique name ([a-z0-9-]); auto-generated when empty"},
			"task":  {Type: schema.String, Desc: "The complete, self-contained task for the Subagent", Required: true},
			"model": {Type: schema.String, Desc: "Optional model override (<provider>::<model> chain member)"},
		}),
	}, nil
}

func (t *spawnTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	owner, err := rtFromCtx(ctx).channelOwnerFromCtx(ctx)
	if err != nil {
		return "", err
	}

	var a subagentToolArgs
	if argErr := unmarshalToolArgs(args, &a); argErr != nil {
		return "", argErr
	}

	st, err := t.runtime.Spawn(ctx, owner, nil, subagent.SpawnRequest{
		Type:     a.Type,
		Name:     a.Name,
		Task:     a.Task,
		ModelPin: a.Model,
	})
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`{"subagent_id":%q,"name":%q,"status":%q}`, st.ID, st.Name, string(st.Status)), nil
}

// sendTool implements subagent_send.
type sendTool struct {
	runtime *subagent.Runtime
	logger  zerolog.Logger
}

func newSubagentSendTool(r *subagent.Runtime, logger zerolog.Logger) *sendTool {
	return &sendTool{runtime: r, logger: logger}
}

func (t *sendTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "subagent_send",
		Desc: "Send a follow-up message to a Subagent. A running Subagent queues it (dispatched after its current task); an idle one is reopened with its prior context intact.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"id":   {Type: schema.String, Desc: "Subagent id or name", Required: true},
			"text": {Type: schema.String, Desc: "The message", Required: true},
		}),
	}, nil
}

func (t *sendTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	owner, err := rtFromCtx(ctx).channelOwnerFromCtx(ctx)
	if err != nil {
		return "", err
	}

	var a subagentToolArgs
	if argErr := unmarshalToolArgs(args, &a); argErr != nil {
		return "", argErr
	}

	if err := t.runtime.Send(ctx, owner, nil, a.ID, a.Text); err != nil {
		return "", err
	}

	return "message queued for " + a.ID, nil
}

// waitTool implements subagent_wait.
type waitTool struct {
	runtime *subagent.Runtime
	rt      *sessionRuntime
	logger  zerolog.Logger
}

func newSubagentWaitTool(r *subagent.Runtime, rt *sessionRuntime, logger zerolog.Logger) *waitTool {
	return &waitTool{runtime: r, rt: rt, logger: logger}
}

func (t *waitTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "subagent_wait",
		Desc: "Wait for one or more Subagents. Returns every pending result of the targets immediately, or blocks until the first arrives or the timeout expires. A timeout does not stop the Subagents: their results arrive later. To show a result to the user before your final answer, pass its content (or a summary) to `say` when that tool is available.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"targets": {Type: schema.Array, Desc: "Subagent ids or names", Required: true, ElemInfo: &schema.ParameterInfo{Type: schema.String}},
			"timeout": {Type: schema.Integer, Desc: "Optional timeout in seconds (default 60)"},
		}),
	}, nil
}

func (t *waitTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	owner, err := rtFromCtx(ctx).channelOwnerFromCtx(ctx)
	if err != nil {
		return "", err
	}

	var a subagentToolArgs
	if argErr := unmarshalToolArgs(args, &a); argErr != nil {
		return "", argErr
	}

	// Turn-steering S12: the wait ends early on a candidate or a stop via the
	// per-Turn signal, on a derived context so the Turn's own context (shared
	// with MCP/HA calls) is never cancelled. Pending results stay pending.
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()

	turn := steer.FromContext(ctx)
	interrupted := make(chan struct{})

	if turn != nil {
		if sig := turn.Signal(); sig != nil {
			go func() {
				select {
				case <-sig:
					close(interrupted)
					cancelWait()
				case <-waitCtx.Done():
				}
			}()
		}
	}

	res, err := t.runtime.Wait(waitCtx, owner, a.Targets, time.Duration(a.Timeout)*time.Second)
	if err != nil {
		select {
		case <-interrupted:
			if turn.Stopping() {
				return steer.SubagentWaitInterruptedStop, nil
			}

			return steer.SubagentWaitInterruptedSteer, nil
		default:
		}

		return "", err
	}

	if res.TimedOut {
		return "No result yet — the Subagents keep running and their results will arrive later (at your next turn).", nil
	}

	// The wait door (D6): the handbacks are consumed; the tool result
	// references the ids so the recorded tool-call group IS the delivered
	// counterpart. The text is scan-mutated (delivery-time scan).
	var b strings.Builder
	for _, hb := range res.Handbacks {
		scanned := injection.Scan(hb.Report, injection.ModeFull)
		b.WriteString(injection.BuildHandbackEnvelope(injection.NewID(),
			fmt.Sprintf("Subagent %s finished (%s). handback_id: %s dispatch_id: %s\n\n%s",
				hb.Name, hb.Status, hb.HandbackID, hb.DispatchID, scanned.Text)))
		b.WriteString("\n\n")
	}

	// Consumption resets the passive-notice latch.
	if t.rt != nil {
		t.rt.resetPendingNotice(owner.SessionID)
	}

	return strings.TrimSpace(b.String()), nil
}

// listTool implements subagent_list.
type listTool struct {
	runtime *subagent.Runtime
	logger  zerolog.Logger
}

func newSubagentListTool(r *subagent.Runtime, logger zerolog.Logger) *listTool {
	return &listTool{runtime: r, logger: logger}
}

func (t *listTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name:        "subagent_list",
		Desc:        "List your Subagents with their status and the number of results pending.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
	}, nil
}

func (t *listTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	owner, err := rtFromCtx(ctx).channelOwnerFromCtx(ctx)
	if err != nil {
		return "", err
	}

	views, pending := t.runtime.List(owner)

	var b strings.Builder
	fmt.Fprintf(&b, "Pending results: %d\n", pending)
	for _, v := range views {
		fmt.Fprintf(&b, "- %s (%s) status=%s", v.Name, v.Type, v.Status)
		if v.QueuedMail > 0 {
			fmt.Fprintf(&b, " queued_mail=%d", v.QueuedMail)
		}

		b.WriteString("\n")
	}

	return b.String(), nil
}

// interruptTool implements subagent_interrupt.
type interruptTool struct {
	runtime *subagent.Runtime
	logger  zerolog.Logger
}

func newSubagentInterruptTool(r *subagent.Runtime, logger zerolog.Logger) *interruptTool {
	return &interruptTool{runtime: r, logger: logger}
}

func (t *interruptTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "subagent_interrupt",
		Desc: "Interrupt a running Subagent's current task (an interrupted result is produced; queued messages then start the next task).",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "Subagent id or name", Required: true},
		}),
	}, nil
}

func (t *interruptTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	owner, err := rtFromCtx(ctx).channelOwnerFromCtx(ctx)
	if err != nil {
		return "", err
	}

	var a subagentToolArgs
	if argErr := unmarshalToolArgs(args, &a); argErr != nil {
		return "", argErr
	}

	if err := t.runtime.Interrupt(ctx, owner, a.ID); err != nil {
		return "", err
	}

	return "interrupt requested for " + a.ID, nil
}

// resetPendingNotice clears the idle-period notice latch (called on
// consumption; nil-safe before wiring).
func (rt *sessionRuntime) resetPendingNotice(sessionID string) {
	if rt.subagentNotices != nil {
		rt.subagentNotices.reset(sessionID)
	}
}

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/agent"
	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/cli"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/llm"
	"github.com/marco-rozz/ready/internal/persistence"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/steer"
)

// transportZMQ is the metadata value the ZMQ channel stamps on its messages.
const transportZMQ = "zmq"

// Fixed English product strings of /stop (turn-steering S11).
const (
	msgStopped        = "Stopped. Completed actions are kept; nothing else will run."
	msgNothingToStop  = "Nothing to stop."
	msgStopDroppedFmt = " %d message(s) sent meanwhile were not processed."
)

// steeringPolicy is the per-generation steering configuration: the global
// limits plus the effective on/off of each logical channel. It is read per
// Turn under the generation read-lock, so a Runtime reload applies new limits
// from the next Turn (turn-steering S18). The zero value disables steering
// (MaxSteers 0), which keeps every runtime built without configuration — the
// tests — on the pre-steering behaviour.
type steeringPolicy struct {
	cfg      config.SteeringConfig
	channels map[string]bool
}

// steeringPolicyFrom derives the policy of a configuration. The logical
// channel "cli" is served by the stdin CLI or the ZMQ CLI (never both); its
// effective switch follows whichever is enabled.
func steeringPolicyFrom(cfg *config.Config) steeringPolicy {
	cliKey := "cli"
	if cfg.Channels.ZMQCLI.Enabled {
		cliKey = "zmqcli"
	}

	return steeringPolicy{
		cfg: cfg.Steering,
		channels: map[string]bool{
			cli.ChannelName:      cfg.EffectiveSteering(cliKey),
			telegram.ChannelName: cfg.EffectiveSteering("telegram"),
			"serial":             cfg.EffectiveSteering("serial"),
		},
	}
}

func (p steeringPolicy) enabledFor(channelName string) bool {
	on, ok := p.channels[channelName]
	if !ok {
		on = p.cfg.SteeringEnabled()
	}

	return on && p.cfg.MaxSteersPerTurn > 0
}

// turnObserver reports Turn starts and claims to the channel (ZMQ events,
// Telegram reaction). Every method is optional-safe: the runtime holds a nil
// observer until a channel wires one.
type turnObserver interface {
	// TurnStarted is called before any output of a Turn: opener is the Turn's
	// opening message, preloaded the deferred candidates it carries.
	TurnStarted(opener channel.Message, ev turnStartEvent)
	// SteerInjected is called when candidates were claimed into the Turn.
	SteerInjected(opener channel.Message, ev steerInjectedEvent)
}

type turnStartEvent struct {
	TurnID   string
	Deferred bool
	// Messages are the opener followed by the pre-loaded candidates.
	Messages []channel.Message
}

type steerInjectedEvent struct {
	TurnID     string
	Claimed    []channel.Message
	Superseded int
}

// turnControl is the per-Turn steering state of the worker: the inbox handle,
// the Turn ID and the limits read when the Turn opened.
type turnControl struct {
	turn        *steer.Turn
	id          string
	deferred    bool
	preload     []channel.Message
	minBudget   time.Duration
	maxRestarts int
	observer    turnObserver

	// superseded counts proposed tool calls closed by append since the last
	// claim; it is reported with the next steer_injected.
	superseded int

	// opener is the Turn's opening message; consumed lists every message the
	// Turn absorbed (opener, pre-loaded and claimed candidates) in
	// consumption order — the final reply answers all of them.
	opener   channel.Message
	consumed []channel.Message

	// preloadLeft counts the deferred candidates not yet claimed: they sit at
	// the head of the pending queue, so the first claims are theirs. They
	// were announced by turn_started and get no steer_injected / reaction.
	preloadLeft int
}

// openTurn registers the Turn on the address inbox. A nil inbox (tests and
// callers outside the dispatcher) yields a nil control: no steering, no
// /stop, exactly the pre-steering path.
func (rt *sessionRuntime) openTurn(in *steer.Inbox, w steer.Work) *turnControl {
	if in == nil {
		return nil
	}

	pol := rt.steering
	enabled := pol.enabledFor(w.Msg.ChannelName)
	steerable := enabled && w.Msg.Origin == channel.OriginHuman

	spec := steer.TurnSpec{
		ID:          newTurnID(),
		Steerable:   steerable,
		MaxRestarts: pol.cfg.MaxRestartsPerTurn,
		Preload:     w.Preload,
	}
	if steerable {
		spec.MaxSteers = pol.cfg.MaxSteersPerTurn
	}

	tc := &turnControl{
		turn:        in.OpenTurn(spec),
		id:          spec.ID,
		deferred:    w.Deferred,
		minBudget:   pol.cfg.RemainingBudget(),
		maxRestarts: pol.cfg.MaxRestartsPerTurn,
		observer:    rt.observer,
	}

	tc.opener = w.Msg
	tc.consumed = append(tc.consumed, w.Msg)

	// Pre-loaded candidates are consumed only when claimed (claim appends
	// them): one that stays unclaimed is deferred again, not answered here.
	for _, c := range w.Preload {
		tc.preload = append(tc.preload, c.Msg)
	}

	tc.preloadLeft = len(w.Preload)

	return tc
}

// newTurnID mints a Turn ID (UUIDv7), distinct from the formatter nonce and
// the thought-tap invocation id.
func newTurnID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}

	return id.String()
}

// close ends the steering window; unclaimed candidates are deferred.
func (tc *turnControl) close(logger zerolog.Logger) {
	if deferred := tc.turn.CloseTurn(); len(deferred) > 0 {
		logger.Info().Str("turn_id", tc.id).Int("deferred", len(deferred)).Msg("steering candidates deferred to the next turn")
	}
}

// arm carries the per-Turn handle (interruptible waits) and the preemption
// source (hooks, model-call preemption, backoff) in the run context — armed
// here only, so probes, aux consumers and Subagent dispatches never see them.
// A nil control (no steering) returns ctx unchanged.
func (tc *turnControl) arm(ctx context.Context) context.Context {
	if tc == nil {
		return ctx
	}

	ctx = steer.WithTurn(ctx, tc.turn)

	return agent.WithPreemptSource(ctx, tc.source(ctx))
}

// executeTurn runs the Turn through the registry's executor — wrapped by the
// turn-control decorator when the Turn has a steering control — and turns a
// /stop preemption into the fixed stop reply (not a failure; the Turn ends at
// its earliest safe point, turn-steering S11).
func (rt *sessionRuntime) executeTurn(
	ctx context.Context,
	tc *turnControl,
	history *persistence.ConversationHistory,
	sessionKey string,
	assemble func(llm.PromptLayout, string) []*schema.Message,
	resumeReady func() bool,
	onChunk func(string),
) (agent.TurnOutcome, string, error) {
	exec := rt.agentReg.exec
	if tc != nil {
		exec = &turnControlExec{turnExecutor: exec, tc: tc, history: history, logger: rt.logger}
	}

	outcome, runErr := exec.ExecuteTurn(ctx, sessionKey, assemble, resumeReady, onChunk)

	if pe, ok := agent.IsPreempt(runErr); ok && pe.Reason == agent.PreemptStop && tc != nil {
		return outcome, stoppedReply(len(tc.turn.DroppedByStop())), nil
	}

	return outcome, "", runErr
}

// source returns the preemption source of this Turn for runCtx's deadline.
func (tc *turnControl) source(runCtx context.Context) agent.PreemptSource {
	deadline, _ := runCtx.Deadline()

	return &turnSource{turn: tc.turn, deadline: deadline, minBudget: tc.minBudget}
}

// turnSource adapts the inbox's Turn handle to agent.PreemptSource.
type turnSource struct {
	turn      *steer.Turn
	deadline  time.Time
	minBudget time.Duration
}

var _ agent.PreemptSource = (*turnSource)(nil)

func (s *turnSource) PreemptReason() (agent.PreemptReason, bool) {
	switch s.turn.Preempt() {
	case steer.ReasonStop:
		return agent.PreemptStop, true
	case steer.ReasonSteer:
		if s.budgetOK() {
			return agent.PreemptSteer, true
		}
	}

	return "", false
}

func (s *turnSource) Signal() <-chan struct{} { return s.turn.Signal() }

// budgetOK reports whether enough of the Turn's outer deadline remains to
// claim and restart (steering limit min_remaining_budget).
func (s *turnSource) budgetOK() bool {
	return s.deadline.IsZero() || time.Until(s.deadline) >= s.minBudget
}

// claim moves the pending candidates into the transcript as `steer` records,
// in admission order, and reports the claim. It runs only at attempt
// assembly — the single claim point — so a steer can only land at a valid
// position (no open tool-call group). It returns the number claimed.
func (tc *turnControl) claim(ctx, runCtx context.Context, nonce string, history *persistence.ConversationHistory, logger zerolog.Logger) int {
	limit := 0
	if deadline, ok := runCtx.Deadline(); !ok || time.Until(deadline) >= tc.minBudget {
		limit = tc.turn.PendingCount()
	}

	claimed := tc.turn.Claim(limit)
	if len(claimed) == 0 {
		return 0
	}

	msgs := make([]channel.Message, 0, len(claimed))

	for _, c := range claimed {
		history.Add(persistence.HistoryMessage{
			Role:      persistence.RoleUser,
			Kind:      persistence.KindSteer,
			Content:   steer.RenderSteer(nonce, c.ReceivedAt, c.Msg.Content),
			Timestamp: time.Now(),
		})
		msgs = append(msgs, c.Msg)
		tc.consumed = append(tc.consumed, c.Msg)
	}

	if err := history.Save(ctx); err != nil {
		logger.Error().Err(err).Str("turn_id", tc.id).Msg("failed to save the session transcript after a steer claim")
	}

	superseded := tc.superseded
	tc.superseded = 0

	logger.Info().
		Str("turn_id", tc.id).
		Int("steers", len(claimed)).
		Int("superseded_tool_calls", superseded).
		Msg("steering candidates claimed")

	// Deferred (pre-loaded) candidates are answered as part of the Turn they
	// opened: no injection report and no reaction for them.
	skip := min(tc.preloadLeft, len(msgs))
	tc.preloadLeft -= skip
	msgs = msgs[skip:]

	if tc.observer != nil && len(msgs) > 0 {
		tc.observer.SteerInjected(msgs[0], steerInjectedEvent{TurnID: tc.id, Claimed: msgs, Superseded: superseded})
	}

	return len(claimed)
}

// stoppedReply builds the fixed /stop reply for a Turn.
func stoppedReply(dropped int) string {
	if dropped <= 0 {
		return msgStopped
	}

	return msgStopped + fmt.Sprintf(msgStopDroppedFmt, dropped)
}

// turnControlExec is the turn-control decorator around the turnExecutor: it
// turns a PreemptError into the next step of the Turn — a restart whose first
// assembly claims the candidates, or the end of a stopped Turn. With nothing
// pending it is a pass-through.
type turnControlExec struct {
	turnExecutor

	tc      *turnControl
	history *persistence.ConversationHistory
	logger  zerolog.Logger
}

var _ turnExecutor = (*turnControlExec)(nil)

// ExecuteTurn runs the inner executor, restarting on a steering preemption.
func (e *turnControlExec) ExecuteTurn(
	ctx context.Context,
	sessionKey string,
	assemble func(llm.PromptLayout, string) []*schema.Message,
	resumeReady func() bool,
	onChunk func(string),
) (agent.TurnOutcome, error) {
	var merged agent.TurnOutcome

	for {
		outcome, err := e.executor().ExecuteTurn(ctx, sessionKey, assemble, resumeReady, onChunk)
		merged = mergeTurnOutcome(merged, outcome)

		pe, preempted := agent.IsPreempt(err)
		if !preempted {
			var ok bool

			// final_undelivered: a complete answer that no channel received
			// (post-step formatting, onChunk == nil) is discarded unrecorded
			// and the Turn re-runs, instead of delivering a stale answer.
			if pe, ok = e.undeliveredFinal(err == nil, onChunk); !ok {
				return merged, err
			}
		}

		if pe.Point == agent.PointBeforeTools {
			e.closeProposedGroup(ctx, pe.Reason)
		}

		if pe.Reason == agent.PreemptStop {
			return merged, pe
		}

		e.tc.turn.NoteRestart()
		e.logger.Debug().Str("turn_id", e.tc.id).Str("point", string(pe.Point)).Int("restarts", e.tc.turn.Restarts()).Msg("turn preempted by steering; restarting")

		// A successful partial answer of the discarded attempt must not leak
		// into the merged outcome: the next attempt produces the real one.
		merged.Reply = ""
	}
}

func (e *turnControlExec) executor() turnExecutor { return e.turnExecutor }

// undeliveredFinal reports a final_undelivered preemption: the attempt
// succeeded, its content went nowhere yet, and a steer or stop is due.
func (e *turnControlExec) undeliveredFinal(succeeded bool, onChunk func(string)) (*agent.PreemptError, bool) {
	if !succeeded || onChunk != nil {
		return nil, false
	}

	switch e.tc.turn.Preempt() {
	case steer.ReasonStop:
		return &agent.PreemptError{Reason: agent.PreemptStop, Point: agent.PointFinalUndelivered}, true
	case steer.ReasonSteer:
		return &agent.PreemptError{Reason: agent.PreemptSteer, Point: agent.PointFinalUndelivered}, true
	}

	return nil, false
}

// closeProposedGroup closes the proposed-but-unexecuted tool-call group by
// append with the fixed Superseded (or Stopped) result and persists it
// immediately, before the next assembly.
func (e *turnControlExec) closeProposedGroup(ctx context.Context, reason agent.PreemptReason) {
	result := persistence.SupersededToolResult
	if reason == agent.PreemptStop {
		result = persistence.StoppedToolResult
	}

	n := e.history.CloseOpenToolCallGroupWith(result)
	if n == 0 {
		return
	}

	if reason == agent.PreemptSteer {
		e.tc.superseded += n
	}

	if err := e.history.Save(ctx); err != nil {
		e.logger.Error().Err(err).Str("turn_id", e.tc.id).Msg("failed to save superseded tool results")
	}
}

// mergeTurnOutcome accumulates the effect flags across the attempts of one
// Turn so terminal-message selection stays conservative.
func mergeTurnOutcome(acc, o agent.TurnOutcome) agent.TurnOutcome {
	acc.ToolCallStarted = acc.ToolCallStarted || o.ToolCallStarted
	acc.ToolCallCompleted = acc.ToolCallCompleted || o.ToolCallCompleted
	acc.ContentDelivered = acc.ContentDelivered || o.ContentDelivered

	if o.Reply != "" {
		acc.Reply = o.Reply
	}

	return acc
}

// handleStop handles an accepted /stop out of band, from the dispatcher's
// intake: with a Turn to stop the running Turn ends itself at its earliest
// safe point and its final reply is the stop reply; otherwise the user gets
// the fixed "nothing to stop" answer and nothing is written.
func (rt *sessionRuntime) handleStop(ctx context.Context, _ *steer.Inbox, msg channel.Message, adm steer.Admission) {
	rt.logger.Info().
		Str("channel", msg.ChannelName).
		Str("turn_id", adm.TurnID).
		Bool("stopping", adm.Stopping).
		Int("dropped", len(adm.Dropped)).
		Msg("/stop admitted")

	if adm.Stopping {
		return
	}

	rt.sendFixed(ctx, msg, msgNothingToStop)
}

// replyMetadata adds the Turn correlation to the final reply's metadata
// (zmqcli reads it): the Turn ID, every request the Turn consumed in
// consumption order — plus the accepted /stop requests for a stopped Turn —
// and the candidates a /stop dropped. Only requests of the opener's own
// Connection are listed; ids of other origins are opaque to this client.
func (tc *turnControl) replyMetadata(meta map[string]string, stopped bool) map[string]string {
	conn := meta[zmqcli.ConnectionID]
	if conn == "" {
		return withMetadata(meta, zmqcli.MetaTurnID, tc.id)
	}

	ids := requestIDsOf(tc.consumed, conn)

	var dropped []string

	if stopped {
		ids = append(ids, requestIDsOf(tc.turn.StopMessages(), conn)...)

		for _, c := range tc.turn.DroppedByStop() {
			if id := c.Msg.Metadata[zmqcli.RequestID]; id != "" && c.Msg.Metadata[zmqcli.ConnectionID] == conn {
				dropped = append(dropped, id)
			}
		}
	}

	out := withMetadata(meta, zmqcli.MetaTurnID, tc.id)
	out[zmqcli.MetaTurnRequestIDs] = zmqcli.EncodeIDs(ids)

	if len(dropped) > 0 {
		out[zmqcli.MetaTurnDroppedIDs] = zmqcli.EncodeIDs(dropped)
	}

	return out
}

// requestIDsOf lists the ZMQ request ids of msgs that belong to Connection
// conn, in order.
func requestIDsOf(msgs []channel.Message, conn string) []string {
	var ids []string

	for _, m := range msgs {
		if m.Metadata["transport"] != transportZMQ || m.Metadata[zmqcli.ConnectionID] != conn {
			continue
		}

		if id := m.Metadata[zmqcli.RequestID]; id != "" {
			ids = append(ids, id)
		}
	}

	return ids
}

// channelObserver reports Turn starts and claims to the channels that care:
// the ZMQ channel (turn_started / steer_injected events). The Telegram
// reaction joins in the Telegram change group.
type channelObserver struct {
	mgr    *channel.Manager
	logger zerolog.Logger
}

var _ turnObserver = (*channelObserver)(nil)

func (o *channelObserver) TurnStarted(opener channel.Message, ev turnStartEvent) {
	zch := getZMQChannel(o.mgr, opener.ChannelName)
	if zch == nil || opener.Metadata["transport"] != transportZMQ {
		return
	}

	conn := opener.Metadata[zmqcli.ConnectionID]
	zch.NotifyTurnStarted(conn, opener.Metadata[zmqcli.RequestID], ev.TurnID, requestIDsOf(ev.Messages, conn), ev.Deferred)
}

// steerReactionTimeout bounds the best-effort Telegram reaction.
const steerReactionTimeout = 5 * time.Second

func (o *channelObserver) SteerInjected(opener channel.Message, ev steerInjectedEvent) {
	o.reactTelegram(ev.Claimed)

	zch := getZMQChannel(o.mgr, opener.ChannelName)
	if zch == nil {
		return
	}

	// One event per Connection that owns claimed requests.
	seen := map[string]bool{}

	for _, m := range ev.Claimed {
		conn := m.Metadata[zmqcli.ConnectionID]
		if m.Metadata["transport"] != transportZMQ || conn == "" || seen[conn] {
			continue
		}

		seen[conn] = true
		zch.NotifySteerInjected(conn, opener.Metadata[zmqcli.RequestID], ev.TurnID, requestIDsOf(ev.Claimed, conn), ev.Superseded)
	}
}

// ackAdmission reports the admission of every ZMQ `message` request
// (message_accepted with the inbox's disposition). Other channels and `exec`
// requests are not acknowledged.
func (rt *sessionRuntime) ackAdmission(msg channel.Message, adm steer.Admission) {
	if msg.Metadata["transport"] != transportZMQ || msg.Metadata["request_type"] != string(protocol.RequestTypeMessage) {
		return
	}

	zch := getZMQChannel(rt.mgr, msg.ChannelName)
	if zch == nil {
		return
	}

	var disposition string

	switch adm.Disposition {
	case steer.Steer:
		disposition = protocol.DispositionSteer
	case steer.Stop:
		disposition = protocol.DispositionStop
	case steer.Queued:
		disposition = protocol.DispositionQueued
	default:
		return
	}

	zch.AcknowledgeMessage(msg.Metadata[zmqcli.ConnectionID], msg.Metadata[zmqcli.RequestID], disposition, adm.TurnID)
}

// reactTelegram sets the 👀 reaction on every injected Telegram message —
// best-effort and off the Turn's path. A deferred candidate is answered as a
// Turn of its own and gets no reaction.
func (o *channelObserver) reactTelegram(claimed []channel.Message) {
	for _, m := range claimed {
		if m.ChannelName != telegram.ChannelName {
			continue
		}

		tg, ok := o.mgr.Channel(telegram.ChannelName).(*telegram.Channel)
		if !ok {
			return
		}

		md := m.Metadata

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), steerReactionTimeout)
			defer cancel()

			tg.AcknowledgeSteer(ctx, md)
		}()
	}
}

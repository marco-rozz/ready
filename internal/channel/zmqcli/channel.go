// Package zmqcli implements the channel.Channel interface over a ZMQ ROUTER
// socket, providing the daemon-side half of the home-agent ZMQ CLI transport.
//
// A single Channel wraps a *zmq.Listener (ROUTER socket) and adds:
//   - HMAC-SHA256 challenge-response authentication (via transport/zmq.ChallengeStore)
//   - Single-session semantics: a second concurrent client receives "busy"
//   - Idle-timeout watchdog: releases the Connection after connection_idle_timeout of inactivity
//   - Gated DEBUG payload logging behind logging.zmq_payloads_enabled
//
// The Channel satisfies channel.Channel so it plugs into the existing Manager
// without any changes to the manager or the agent main loop.
package zmqcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	zmqtransport "github.com/marco-rozz/ready/internal/transport/zmq"
	"github.com/rs/zerolog"
)

// ChannelName is the logical channel identifier. It is intentionally the same
// as internal/channel/cli.ChannelName ("cli") so the agent registry, formatter
// registry, and session-key logic all continue to work without modification.
const (
	ChannelName  = "cli"
	Challenge    = "challenge"
	ConnectionID = "connection_id"
	RequestID    = "request_id"
	// ErrorCode is the channel.Message metadata key carrying a structured
	// error code for a failed turn's final reply (llm-status-visibility):
	// when present, Send emits Status=error with that ErrorCode instead of
	// Status=ok. Today the only producer is the dispatcher, setting it to
	// protocol.ErrorCodeLLMUnavailable for an exhausted LLM chain.
	ErrorCode = "error_code"
)

// errNoActiveSession is the error code sent when a request targets a session
// that is not active (unknown or already released). A named constant keeps
// the goconst literal count down across the handler set.
const errNoActiveSession = "no_active_session"

// Compile-time assertions: Channel is a connection-oriented channel.Channel.
var (
	_ channel.Channel            = (*Channel)(nil)
	_ channel.ConnectionOriented = (*Channel)(nil)
)

// sessionState holds the mutable per-session fields protected by a single mutex.
// Embedding the mutex inside the struct keeps locking local and avoids exported
// lock methods that callers could misuse.
type sessionState struct {
	mu           sync.Mutex
	connectionID string    // empty when no session is active
	identity     []byte    // ZMQ identity of the connected peer
	openedAt     time.Time // used for duration logging on close
	lastActivity time.Time // touched on every inbound message/exec
	lastPing     time.Time // last received heartbeat; zero = client never pinged (liveness exempt)
}

// tryAcquire attempts to open a new session. Returns true and records the
// identity + connectionID if no session is active; returns false otherwise.
// Callers must not hold ss.mu.
func (ss *sessionState) tryAcquire(connectionID string, identity []byte) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ss.connectionID != "" {
		return false
	}

	now := time.Now()
	ss.connectionID = connectionID
	ss.identity = identity
	ss.openedAt = now
	ss.lastActivity = now

	return true
}

// release closes the current session. Safe to call when no session is active.
// Callers must not hold ss.mu.
func (ss *sessionState) release() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.connectionID = ""
	ss.identity = nil
	ss.openedAt = time.Time{}
	ss.lastActivity = time.Time{}
	ss.lastPing = time.Time{}
}

// snapshot returns a consistent copy of all session fields under the lock.
// Callers must not hold ss.mu.
func (ss *sessionState) snapshot() (connectionID string, identity []byte, openedAt, lastActivity time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.connectionID, ss.identity, ss.openedAt, ss.lastActivity
}

// touchActivity updates the lastActivity timestamp. Callers must not hold ss.mu.
func (ss *sessionState) touchActivity() {
	ss.mu.Lock()
	ss.lastActivity = time.Now()
	ss.mu.Unlock()
}

// touchPing records a received heartbeat and counts it as session activity
// (a pinging client is never released by the idle watchdog — a live TUI left
// open stays connected). Callers must not hold ss.mu.
func (ss *sessionState) touchPing() {
	now := time.Now()

	ss.mu.Lock()
	ss.lastPing = now
	ss.lastActivity = now
	ss.mu.Unlock()
}

// lastPingTime returns the timestamp of the last heartbeat, or the zero time
// when the client has never pinged (the liveness watchdog is exempt for such
// sessions). Callers must not hold ss.mu.
func (ss *sessionState) lastPingTime() time.Time {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.lastPing
}

// Channel is the daemon-side ZMQ CLI channel. It embeds a *zmq.Listener for
// raw ROUTER-socket I/O and owns the session lock, the reply-pending map, and
// the internal goroutines (dispatch loop and idle watchdog).
//
// The Channel's public API (Name, Receive, Send, Close) is the only surface
// the channel.Manager sees — all ZMQ-protocol and auth details are hidden here.
// defaultReplyTimeout is the fallback used when cfg.ReplyTimeout is zero
// (e.g. tests that build config.ZMQCLIChannelConfig by hand without going
// through config.ApplyDefaults). It must stay comfortably above the
// daemon's real agent-run budget (agentRunTimeout + formatterTimeout in
// cmd/serve.go, currently 300s + 60s) or the watchdog fires while the agent
// is still legitimately computing, and the eventual reply gets dropped as
// "no pending entry". Keep in sync with config.ApplyDefaults's default.
const defaultReplyTimeout = 400 * time.Second

// defaultLivenessTimeout is the fallback used when cfg.LivenessTimeout is
// zero (e.g. tests that build config.ZMQCLIChannelConfig by hand without
// going through config.ApplyDefaults). The TUI pings every third of it
// (15s), so the release lands within ~45s of client death. Keep in sync with
// config.ApplyDefaults's default.
const defaultLivenessTimeout = 45 * time.Second

// pendingEntry holds the information needed to route replies and notifications
// back to the client for a given in-flight request.
type pendingEntry struct {
	identity  []byte
	requestID string
}

// Channel is the daemon-side ZMQ CLI channel. It embeds a *zmq.Listener for
// raw ROUTER-socket I/O and owns the session lock, the reply-pending map, and
// the internal goroutines (dispatch loop and idle watchdog).
//
// The Channel's public API (Name, Receive, Send, Close) is the only surface
// the channel.Manager sees — all ZMQ-protocol and auth details are hidden here.
type Channel struct {
	listener              *zmqtransport.Listener
	challenges            *zmqtransport.ChallengeStore
	connectionIdleTimeout time.Duration
	// replyTimeout is the maximum time a session may remain active while a
	// reply is pending (i.e. the agent is computing) before the watchdog
	// releases it regardless of connectionIdleTimeout. Sourced from cfg.ReplyTimeout
	// (falls back to defaultReplyTimeout if unset).
	replyTimeout time.Duration
	// livenessTimeout bounds how long a heartbeating session may stop
	// sending pings before the watchdog releases it with reason
	// client_unresponsive (change zmq-session-liveness). Sourced from
	// cfg.LivenessTimeout (falls back to defaultLivenessTimeout if unset).
	// Sessions that never pinged are exempt (see checkIdleTimeout).
	livenessTimeout time.Duration

	logger          zerolog.Logger
	payloadsEnabled bool

	// modelMu guards the model-status fields below (handshake reporting vs
	// the WatchModelStatus subscription goroutine).
	modelMu     sync.Mutex
	modelStatus ModelStatusEvent
	modelKnown  bool

	// usageMu guards the usage-source hook below (wired once at boot by
	// cmd/serve.go, read on every reply and handshake — tui-usage-
	// observability).
	usageMu sync.Mutex
	// usageSource returns the session key's cumulative totals for the
	// wire's Usage block, or nil when the session has no usage yet (fresh
	// daemon). Attached to every final reply and to the authenticated
	// handshake as the reconnect seed; never attached to progress
	// notifications.
	usageSource func() *protocol.UsageInfo

	// controlHandler answers TUI-reserved control requests (session_info,
	// context_usage) with the markdown content for the reply. Wired once at
	// boot by cmd/serve.go; nil in tests that never set it.
	control ControlRequestHandler

	// connStart is the Connection start hook (session-lifecycle D6): it
	// evaluates the Session reset rule at every successful handshake and
	// returns the effective Session ID. Guarded by usageMu like the other
	// boot-time hooks; nil in tests that never set it.
	connStart channel.ConnectionStartHook

	// sess holds the single active session state.
	sess sessionState

	// pending maps connection_id → pendingEntry so Send can route replies.
	// Entries are added on message/exec receipt and removed on successful send
	// of the final reply (ResponseType:"reply") or on session release.
	pendingMu sync.Mutex
	pending   map[string]pendingEntry

	// incoming queues channel.Messages ready for the Manager's receive loop.
	incoming chan channel.Message

	// ctx / cancel govern the lifetime of internal goroutines.
	ctx    context.Context
	cancel context.CancelFunc

	// closeOnce makes Close idempotent.
	closeOnce sync.Once
}

// NewChannel creates a Channel bound to the given config. It constructs the
// ROUTER socket via zmq.NewListener, starts the dispatch goroutine and
// the idle watchdog, and is ready to use immediately after return.
//
// secret is the plaintext HMAC key resolved by ZMQCLIChannelConfig.ResolveAuthToken.
// payloadsEnabled mirrors cfg.Logging.ZMQPayloadsEnabled — passed separately
// because it lives in LoggingConfig rather than ZMQCLIChannelConfig.
// logger should be pre-scoped to component=zmq-listener by the caller so that
// the transport-level log lines carry the right component tag.
func NewChannel(
	parentCtx context.Context,
	cfg config.ZMQCLIChannelConfig,
	secret string,
	payloadsEnabled bool,
	logger zerolog.Logger,
) (*Channel, error) {
	listenerLogger := logger.With().Str("component", "zmq-listener").Logger()
	sessionLogger := logger.With().Str("component", "zmq-session").Logger()

	listener, err := zmqtransport.NewListener(parentCtx, cfg.Bind, listenerLogger)
	if err != nil {
		return nil, fmt.Errorf("zmqcli: create listener: %w", err)
	}

	// The nonce TTL is 2 × connection_idle_timeout.
	nonceTTL := 2 * cfg.ConnectionIdleTimeout
	challenges := zmqtransport.NewChallengeStore(secret, nonceTTL, payloadsEnabled, sessionLogger)

	// cancel is stored in ch.cancel and called by ch.Close() — not leaked.
	ctx, cancel := context.WithCancel(parentCtx) // G118: cancel function is stored in ch.cancel and invoked by ch.Close()

	replyTimeout := cfg.ReplyTimeout
	if replyTimeout == 0 {
		replyTimeout = defaultReplyTimeout
	}

	livenessTimeout := cfg.LivenessTimeout
	if livenessTimeout == 0 {
		livenessTimeout = defaultLivenessTimeout
	}

	ch := &Channel{
		listener:              listener,
		challenges:            challenges,
		connectionIdleTimeout: cfg.ConnectionIdleTimeout,
		replyTimeout:          replyTimeout,
		livenessTimeout:       livenessTimeout,
		logger:                sessionLogger,
		payloadsEnabled:       payloadsEnabled,
		pending:               make(map[string]pendingEntry),
		incoming:              make(chan channel.Message, 16),
		ctx:                   ctx,
		cancel:                cancel,
	}

	go ch.dispatchLoop()
	go ch.watchdogLoop()

	return ch, nil
}

// Name returns the logical channel name. Always "cli" so the agent registry
// and formatter registry continue to work without modification.
func (c *Channel) Name() string {
	return ChannelName
}

// ControlRequestHandler answers a TUI-reserved control request (session_info,
// context_usage) with the markdown content to deliver as the reply.
// reqType is one of protocol.RequestTypeSessionInfo / RequestTypeContextUsage;
// connectionID is the requesting ZMQ session's ID (for /session display). The
// handler MUST NOT invoke the LLM, mutate conversation history, or touch any
// channel formatter.
type ControlRequestHandler func(reqType protocol.RequestType, connectionID string) string

// SetUsageSource wires the hook that produces the session's cumulative usage
// totals for reply attachment and the handshake seed (tui-usage-
// observability). Nil (never wired) keeps pre-usage payloads valid.
func (c *Channel) SetUsageSource(source func() *protocol.UsageInfo) {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()

	c.usageSource = source
}

// SetControlRequestHandler wires the daemon-side handler for the
// session_info / context_usage request types.
// criticalities-remediation (M1): the write is mutex-guarded like its sibling
// SetUsageSource — dispatchLoop may already be running (started in NewChannel)
// and reads the same field, so an unguarded write is a real data race in the
// boot-time wiring window.
func (c *Channel) SetControlRequestHandler(h ControlRequestHandler) {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()

	c.control = h
}

// OnConnectionStart wires the Connection start hook (channel.ConnectionOriented).
func (c *Channel) OnConnectionStart(hook channel.ConnectionStartHook) {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()

	c.connStart = hook
}

// connectionStart runs the wired hook for a just-acquired Connection; with
// no hook wired the handshake carries no Session ID and the usage seed as
// before.
func (c *Channel) connectionStart(connectionID string) (channel.ConnectionInfo, bool) {
	c.usageMu.Lock()
	hook := c.connStart
	c.usageMu.Unlock()

	if hook == nil {
		return channel.ConnectionInfo{}, false
	}

	return hook(c.ctx, channel.Message{
		ChannelName: ChannelName,
		Metadata:    map[string]string{ConnectionID: connectionID},
	}), true
}

// SendSessionEvent pushes one unsolicited Session lifecycle control event
// (protocol.ControlSessionChanged or protocol.ControlCompacted) to the active
// Connection without touching the pending reply slot. With no Connection
// active the event is dropped: the next handshake carries the current
// Session ID.
func (c *Channel) SendSessionEvent(kind, sessionID string, compaction *protocol.CompactionInfo) error {
	connectionID, identity, _, _ := c.sess.snapshot()
	if connectionID == "" || len(identity) == 0 {
		return nil
	}

	resp, err := protocol.Marshal(protocol.Response{
		ConnectionID: connectionID,
		SessionID:    sessionID,
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      kind,
		Compaction:   compaction,
	})
	if err != nil {
		return fmt.Errorf("zmqcli: marshal %s event: %w", kind, err)
	}

	if sendErr := c.listener.Send(identity, resp); sendErr != nil {
		c.logger.Warn().Err(sendErr).Str(ConnectionID, connectionID).Str("event", kind).Msg("failed to send session event")

		return fmt.Errorf("zmqcli: send %s event: %w", kind, sendErr)
	}

	return nil
}

// currentUsage returns the usage snapshot via the wired source, or nil.
func (c *Channel) currentUsage() *protocol.UsageInfo {
	c.usageMu.Lock()
	source := c.usageSource
	c.usageMu.Unlock()

	if source == nil {
		return nil
	}

	return source()
}

// Receive blocks until a message is available from an authenticated client, or
// until ctx is cancelled. The Manager calls this in its receiveLoop goroutine.
//
// All ZMQ protocol traffic (session_start, session_end, auth challenges, busy
// rejections) is handled inside the dispatchLoop goroutine without touching
// Receive — only message/exec requests that survive auth and session checks
// reach the incoming queue and surface here.
func (c *Channel) Receive(ctx context.Context) (channel.Message, error) {
	select {
	case msg, ok := <-c.incoming:
		if !ok {
			// Channel closed — signal clean exit to the Manager.
			return channel.Message{}, context.Canceled
		}

		return msg, nil
	case <-ctx.Done():
		return channel.Message{}, ctx.Err()
	}
}

// Send delivers a reply to the client that originally sent the message
// identified by msg.Metadata[ConnectionID]. It looks up the ZMQ identity in
// the pending map, sends the JSON-encoded Response with ResponseType "reply",
// and removes the entry on success.
//
// Delivery outcomes (criticalities-remediation LOW zmq): a missing pending
// entry (unknown session, or a duplicate Send after the exactly-once slot
// was consumed) drops the reply WITHOUT retry and returns an error — never a
// silent nil success. A network send failure releases the session so the
// next client is not locked out by the idle watchdog; the entry is NOT
// restored in that case (retrying to a vanished peer only delays the
// failure), and the returned error names the drop.
func (c *Channel) Send(_ context.Context, msg channel.Message) error {
	connectionID := msg.Metadata[ConnectionID]
	if connectionID == "" {
		c.logger.Warn().Msg("Send called with no connection_id in metadata — dropping reply")

		// criticalities-remediation (LOW zmq): an undeliverable reply is an
		// error, never a silent success.
		return fmt.Errorf("zmqcli: reply dropped — no connection_id in metadata")
	}

	c.pendingMu.Lock()
	entry, ok := c.pending[connectionID]
	if ok {
		delete(c.pending, connectionID)
	}
	c.pendingMu.Unlock()

	if !ok {
		c.logger.Warn().Str(ConnectionID, connectionID).Msg("no pending entry for reply — dropping")

		// criticalities-remediation (LOW zmq): the drop is propagated so the
		// dispatcher's error path observes it instead of reading success.
		return fmt.Errorf("zmqcli: reply dropped — no pending entry for connection %q", connectionID)
	}

	resp, err := protocol.Marshal(protocol.Response{
		ConnectionID: connectionID,
		Status:       replyStatusFor(msg.Metadata),
		RequestID:    entry.requestID,
		ResponseType: protocol.ResponseTypeReply,
		Content:      msg.Content,
		ErrorCode:    msg.Metadata[ErrorCode],
		// tui-usage-observability: every final reply carries the session's
		// cumulative totals (success AND failure — a failed attempt still
		// consumed tokens); nil keeps pre-usage payloads valid.
		Usage: c.currentUsage(),
	})
	if err != nil {
		// Marshal failure is a bug — put the identity back and surface the error.
		c.pendingMu.Lock()
		c.pending[connectionID] = entry
		c.pendingMu.Unlock()

		return fmt.Errorf("zmqcli: marshal response: %w", err)
	}

	if c.payloadsEnabled {
		c.logger.Debug().
			Str(ConnectionID, connectionID).
			Str("identity", shortIdentity(entry.identity)).
			Str("response_type", "reply").
			Msg("response_payload")
	}

	if sendErr := c.listener.Send(entry.identity, resp); sendErr != nil {
		// The peer is unreachable — the client disconnected (timeout, kill,
		// network loss). There is no point in retrying: the reply is lost.
		// Release the session immediately so the next client can connect
		// without waiting for the idle watchdog.
		activeConnectionID, activeIdentity, openedAt, _ := c.sess.snapshot()
		if activeConnectionID == connectionID && bytes.Equal(activeIdentity, entry.identity) {
			c.sess.release()

			duration := time.Since(openedAt)
			c.logger.Info().
				Err(sendErr).
				Str(ConnectionID, connectionID).
				Str("reason", "send_failure").
				Dur("duration", duration).
				Msg("session_closed")
		} else {
			c.logger.Warn().Err(sendErr).Str(ConnectionID, connectionID).Msg("failed to send reply — session already released")
		}

		return fmt.Errorf("zmqcli: send reply: %w", sendErr)
	}

	// Reset the idle watchdog: the session is active as long as the client
	// is receiving replies, not just sending messages.
	c.sess.touchActivity()

	return nil
}

// SendNotification sends an intermediate progress notification to the active
// session client without consuming the pending entry. The client DEALER receives
// this as ResponseType "progress" and can display it inline. If no pending
// entry exists (e.g., the reply was already sent), the notification is dropped.
func (c *Channel) SendNotification(_ context.Context, connectionID, content string) error {
	c.pendingMu.Lock()
	entry, ok := c.pending[connectionID]
	c.pendingMu.Unlock()

	if !ok {
		c.logger.Debug().Str(ConnectionID, connectionID).Msg("no pending entry for notification — dropping")

		return nil
	}

	resp, err := protocol.Marshal(protocol.Response{
		ConnectionID: connectionID,
		Status:       protocol.ResponseStatusOK,
		RequestID:    entry.requestID,
		ResponseType: protocol.ResponseTypeProgress,
		Content:      content,
		// Live usage totals (tui-usage-observability): the TUI footer's
		// LLM-call counter advances mid-turn — one tick per completed ReAct
		// iteration. Nil source (fresh session) serialises as omitted.
		Usage: c.currentUsage(),
	})
	if err != nil {
		return fmt.Errorf("zmqcli: marshal notification: %w", err)
	}

	if c.payloadsEnabled {
		c.logger.Debug().
			Str(ConnectionID, connectionID).
			Str("identity", shortIdentity(entry.identity)).
			Str("response_type", "progress").
			Msg("notification_payload")
	}

	if sendErr := c.listener.Send(entry.identity, resp); sendErr != nil {
		c.logger.Warn().Err(sendErr).Str(ConnectionID, connectionID).Msg("failed to send notification")

		return fmt.Errorf("zmqcli: send notification: %w", sendErr)
	}

	// A successful notification proves the agent is actively producing
	// output (e.g. mid-stream LLM tokens or tool-call progress), not stuck.
	// Reset the idle watchdog so a long-but-alive stream isn't killed by
	// reply_timeout partway through — otherwise the client sees chunks
	// arrive (looking like a complete answer) but the terminating "reply"
	// gets silently dropped once idle (measured from turn start) crosses
	// reply_timeout, and the client hangs until its own timeout fires.
	c.sess.touchActivity()

	return nil
}

// Close performs the graceful shutdown handshake described in the design doc:
//  1. If a session is active, send Response{Status:"shutdown"} best-effort.
//  2. Cancel internal goroutines.
//  3. Close the ROUTER socket.
//
// Idempotent: multiple calls return nil after the first.
func (c *Channel) Close() error {
	var closeErr error

	c.closeOnce.Do(func() {
		// 1. Best-effort shutdown notification.
		connectionID, identity, _, _ := c.sess.snapshot()
		if len(identity) > 0 {
			resp, marshalErr := protocol.Marshal(protocol.Response{
				ConnectionID: connectionID,
				Status:       protocol.ResponseStatusShutdown,
				ResponseType: protocol.ResponseTypeControl,
				Content:      "daemon shutting down",
			})
			if marshalErr == nil {
				_ = c.listener.Send(identity, resp)
			}
		}

		// 2. Stop internal goroutines.
		c.cancel()

		// 3. Close listener (this also unblocks any pending Recv).
		closeErr = c.listener.Close()
	})

	return closeErr
}

// ---------------------------------------------------------------------------
// Internal goroutines
// ---------------------------------------------------------------------------

// dispatchLoop is the main ZMQ receive goroutine. It reads frames from the
// ROUTER socket, parses the Request, and dispatches by type. It runs until
// c.ctx is cancelled or the listener is closed.
func (c *Channel) dispatchLoop() {
	for {
		identity, payload, err := c.listener.Recv()
		if err != nil {
			if errors.Is(err, zmqtransport.ErrContextDone) ||
				errors.Is(err, zmqtransport.ErrListenerClosed) {
				return
			}
			c.logger.Warn().Err(err).Msg("recv error — continuing")

			continue
		}

		req, parseErr := protocol.UnmarshalRequest(payload)
		if parseErr != nil {
			c.logger.Warn().Err(parseErr).Str("identity", shortIdentity(identity)).Msg("malformed request — sending error response")
			resp, _ := protocol.Marshal(protocol.Response{
				Status:    protocol.ResponseStatusError,
				ErrorCode: "malformed_request",
			})
			_ = c.listener.Send(identity, resp)

			continue
		}

		if c.payloadsEnabled {
			c.logger.Debug().
				Str("identity", shortIdentity(identity)).
				Str("type", string(req.Type)).
				Str(ConnectionID, req.ConnectionID).
				Msg("request_payload")
		}

		switch req.Type {
		case protocol.RequestTypeSessionStart:
			c.handleSessionStart(identity, req)
		case protocol.RequestTypeMessage, protocol.RequestTypeExec:
			c.handleMessage(identity, req)
		case protocol.RequestTypeSessionEnd:
			c.handleSessionEnd(identity, req)
		case protocol.RequestTypePing:
			c.handlePing(identity, req)
		case protocol.RequestTypeSessionInfo, protocol.RequestTypeContextUsage:
			c.handleControlRequest(identity, req)
		}
	}
}

// handleSessionStart handles the two-round HMAC challenge-response handshake.
//
// Round 1 (probe): AuthToken is empty → issue nonce challenge.
// Round 2 (verify): AuthToken is non-empty → verify HMAC and try to acquire session.
func (c *Channel) handleSessionStart(identity []byte, req protocol.Request) {
	if req.AuthToken == "" {
		// Round 1: challenge probe. Issue a nonce and reply with it.
		nonceHex, err := c.challenges.IssueChallenge(identity)
		if err != nil {
			c.logger.Error().Err(err).Msg("failed to issue challenge")
			resp, _ := protocol.Marshal(protocol.Response{
				ConnectionID: req.ConnectionID,
				Status:       protocol.ResponseStatusError,
				RequestID:    req.RequestID,
				ResponseType: protocol.ResponseTypeControl,
				ErrorCode:    "internal_error",
			})
			_ = c.listener.Send(identity, resp)

			return
		}

		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusOK,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
			Content:      nonceHex,
			ErrorCode:    Challenge,
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	// Round 2: verify HMAC token.
	if err := c.challenges.VerifyAuthToken(identity, req.AuthToken); err != nil {
		c.logger.Warn().Str("reason", "hmac_mismatch").Msg("auth_failed")
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusAuthFail,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	// Auth OK — attempt to acquire the single-session lock.
	if !c.sess.tryAcquire(req.ConnectionID, identity) {
		c.logger.Info().
			Str(ConnectionID, req.ConnectionID).
			Msg("session_busy_rejected")
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusBusy,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	c.logger.Info().Str(ConnectionID, req.ConnectionID).Msg("session_opened")

	// llm-status-visibility: the successful authenticated handshake carries
	// the current model status so a fresh session starts correct without
	// polling. Omitted entirely when no status source is wired (tests,
	// pre-change shape).
	okResp := protocol.Response{
		ConnectionID: req.ConnectionID,
		Status:       protocol.ResponseStatusOK,
		RequestID:    req.RequestID,
		ResponseType: protocol.ResponseTypeControl,
	}
	if ev, known := c.currentModelStatus(); known {
		okResp.ModelRef = ev.Ref
		okResp.ModelState = ev.State
	}
	// session-lifecycle D6: the Session reset rule is evaluated here, at the
	// Connection start, without waiting for the address's worker; the
	// response carries the effective Session ID.
	info, hooked := c.connectionStart(req.ConnectionID)
	okResp.SessionID = info.SessionID
	// tui-usage-observability: the handshake seeds a reconnecting TUI with
	// the current Session's live totals (nil — omitted — when fresh, and
	// always omitted when a reset was just decided).
	if !hooked || !info.ResetPending {
		okResp.Usage = c.currentUsage()
	}
	// zmq-session-liveness: the client derives its ping interval from the
	// daemon's liveness timeout (interval = timeout / 3) — one knob, no
	// client-side configuration.
	okResp.LivenessTimeout = c.livenessTimeout.String()
	resp, marshalErr := protocol.Marshal(okResp)
	if marshalErr != nil {
		// Bug-level failure: release the just-acquired session so the next
		// client is not locked out until the watchdog.
		c.sess.release()
		c.logger.Error().Err(marshalErr).Msg("failed to marshal handshake OK reply")

		return
	}

	// criticalities-remediation (M7): a failed send on the acquire path
	// releases the session — same semantics as every other acquire path. A
	// fire-and-forget send left the session locked until the idle watchdog
	// when the network send failed.
	if sendErr := c.listener.Send(identity, resp); sendErr != nil {
		c.sess.release()
		c.logger.Error().Err(sendErr).
			Str(ConnectionID, req.ConnectionID).
			Msg("failed to send handshake OK reply — session released")
	}
}

// handleMessage processes a message or exec request. It verifies that the
// sender matches the active session, records the reply-routing entry, and
// forwards the message to the incoming queue for the agent.
func (c *Channel) handleMessage(identity []byte, req protocol.Request) {
	activeConnectionID, activeIdentity, _, _ := c.sess.snapshot()

	if activeConnectionID != req.ConnectionID || !bytes.Equal(activeIdentity, identity) {
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusError,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
			ErrorCode:    "no_active_session",
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	// Update last-activity and register the pending reply entry.
	c.sess.touchActivity()

	// criticalities-remediation (M8): a second request while another is
	// still in flight is rejected structurally instead of silently
	// overwriting the pending slot (which mis-routed or dropped the first
	// request's reply).
	c.pendingMu.Lock()
	existing, inFlight := c.pending[req.ConnectionID]
	c.pendingMu.Unlock()

	if inFlight && existing.requestID != req.RequestID {
		c.logger.Warn().Str(ConnectionID, req.ConnectionID).Msg("request_in_flight_rejected")
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusError,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
			ErrorCode:    "request_in_flight",
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	c.pendingMu.Lock()
	c.pending[req.ConnectionID] = pendingEntry{
		identity:  identity,
		requestID: req.RequestID,
	}
	c.pendingMu.Unlock()

	msg := channel.Message{
		Content:     req.Content,
		ChannelName: ChannelName,
		Metadata: map[string]string{
			ConnectionID:   req.ConnectionID,
			RequestID:      req.RequestID,
			"request_type": string(req.Type),
		},
	}

	select {
	case c.incoming <- msg:
	case <-c.ctx.Done():
	}
}

// handleControlRequest answers the TUI-reserved control request types
// (session_info, context_usage) without touching the agent, conversation
// history, or any channel formatter: verify the session, register the reply
// slot, ask the injected handler for the markdown content, and send the
// reply — consuming the pending entry exactly once.
func (c *Channel) handleControlRequest(identity []byte, req protocol.Request) {
	activeConnectionID, activeIdentity, _, _ := c.sess.snapshot()

	if activeConnectionID != req.ConnectionID || !bytes.Equal(activeIdentity, identity) {
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusError,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
			ErrorCode:    "no_active_session",
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	c.sess.touchActivity()

	// criticalities-remediation (M1): read the handler under the same mutex
	// SetControlRequestHandler writes through.
	c.usageMu.Lock()
	control := c.control
	c.usageMu.Unlock()

	content := ""
	if control != nil {
		content = control(req.Type, req.ConnectionID)
	}

	resp, err := protocol.Marshal(protocol.Response{
		ConnectionID: req.ConnectionID,
		Status:       protocol.ResponseStatusOK,
		RequestID:    req.RequestID,
		ResponseType: protocol.ResponseTypeReply,
		Content:      content,
		Usage:        c.currentUsage(),
	})
	if err != nil {
		c.logger.Error().Err(err).Str("request_type", string(req.Type)).Msg("failed to marshal control request reply")

		return
	}

	if c.payloadsEnabled {
		c.logger.Debug().
			Str(ConnectionID, req.ConnectionID).
			Str("request_type", string(req.Type)).
			Msg("control_request_reply")
	}

	if sendErr := c.listener.Send(identity, resp); sendErr != nil {
		c.logger.Warn().Err(sendErr).Str(ConnectionID, req.ConnectionID).Msg("failed to send control request reply")
	}

	// Reset the idle watchdog: a served control request is client activity.
	c.sess.touchActivity()
}

// handleSessionEnd handles a session_end request. It verifies the session_id
// matches the active session and releases the lock. The ZMQ identity is NOT
// checked (change zmq-session-liveness): the client sends its
// cleanup from a fresh DEALER socket (<connectionID>-end identity) on exit paths
// where the session socket is already closed, so the identity cannot match.
// Ownership is authenticated by the session_id — a UUID v4 knowable only after
// a successful HMAC handshake — and the identity is socket routing, not a
// secret. (Reverting the 3ba4a1c identity check, which broke every cleanup
// path and left the lock held until the idle watchdog.)
func (c *Channel) handleSessionEnd(identity []byte, req protocol.Request) {
	activeConnectionID, _, openedAt, _ := c.sess.snapshot()

	if activeConnectionID != req.ConnectionID {
		// Not the active session — still reply so chatSendSessionEnd DEALER Recv completes.
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusOK,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	c.sess.release()

	c.pendingMu.Lock()
	delete(c.pending, req.ConnectionID)
	c.pendingMu.Unlock()

	duration := time.Since(openedAt)
	c.logger.Info().
		Str(ConnectionID, req.ConnectionID).
		Str("reason", "end").
		Dur("duration", duration).
		Msg("session_closed")

	resp, _ := protocol.Marshal(protocol.Response{
		ConnectionID: req.ConnectionID,
		Status:       protocol.ResponseStatusOK,
		RequestID:    req.RequestID,
		ResponseType: protocol.ResponseTypeControl,
	})
	_ = c.listener.Send(identity, resp)
}

// handlePing answers a client liveness heartbeat (change zmq-session-
// liveness): verify the sender is the active session, record the heartbeat,
// and reply ok. It never touches the pending map, the agent, or the
// conversation history — the ping is pure liveness, counted as session
// activity so a live TUI left open is never released by the idle watchdog.
func (c *Channel) handlePing(identity []byte, req protocol.Request) {
	activeConnectionID, activeIdentity, _, _ := c.sess.snapshot()

	if activeConnectionID != req.ConnectionID || !bytes.Equal(activeIdentity, identity) {
		resp, _ := protocol.Marshal(protocol.Response{
			ConnectionID: req.ConnectionID,
			Status:       protocol.ResponseStatusError,
			RequestID:    req.RequestID,
			ResponseType: protocol.ResponseTypeControl,
			ErrorCode:    errNoActiveSession,
		})
		_ = c.listener.Send(identity, resp)

		return
	}

	c.sess.touchPing()

	resp, _ := protocol.Marshal(protocol.Response{
		ConnectionID: req.ConnectionID,
		Status:       protocol.ResponseStatusOK,
		RequestID:    req.RequestID,
		ResponseType: protocol.ResponseTypeControl,
	})
	_ = c.listener.Send(identity, resp)
}

// watchdogLoop runs every 30 s and releases sessions that have exceeded
// connection_idle_timeout without activity. The release is silent — no unsolicited
// message is pushed to the client (see releaseTimedOut doc).
func (c *Channel) watchdogLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.checkIdleTimeout()
		}
	}
}

// checkIdleTimeout is called by watchdogLoop. Factored out so tests can drive
// it directly without a timer.
func (c *Channel) checkIdleTimeout() {
	connectionID, identity, openedAt, lastActivity := c.sess.snapshot()
	if connectionID == "" {
		return // no active session
	}

	// Liveness path (change zmq-session-liveness): a session that has pinged
	// before but stopped within the liveness window is a dead client (killed
	// process, closed terminal window, laptop sleep). Released well before
	// connectionIdleTimeout so a new client can connect. Sessions that never pinged
	// (older clients, --exec) have a zero lastPing and stay exempt — the
	// idle/reply watchdogs below cover them as before.
	lastPing := c.sess.lastPingTime()
	if !lastPing.IsZero() && time.Since(lastPing) > c.livenessTimeout {
		c.releaseTimedOut(connectionID, identity, openedAt, "client_unresponsive")

		return
	}

	idle := time.Since(lastActivity)

	// Fast path: check the normal idle timeout.
	if idle > c.connectionIdleTimeout {
		c.releaseTimedOut(connectionID, identity, openedAt, "idle_timeout")

		return
	}

	// Secondary check: if a reply is pending (agent is running) and the session
	// has been inactive for longer than the agent run timeout + buffer, the
	// client is likely dead and the agent either hung or already tried to Send
	// (which would have released via send_failure). This is a safety net.
	c.pendingMu.Lock()
	_, hasPending := c.pending[connectionID]
	c.pendingMu.Unlock()

	if hasPending && idle > c.replyTimeout {
		c.releaseTimedOut(connectionID, identity, openedAt, "reply_timeout")
	}
}

// releaseTimedOut releases the session silently. It does NOT send a
// notification to the peer because in REQ/ROUTER an unsolicited message from
// ROUTER poisons the REQ socket's recv buffer — the next Recv() would pick up
// the stale timeout instead of the real reply to the next request. The client
// has its own per-reply timeout; if it sends a message after session release,
// handleMessage returns "no_active_session" through the normal request/reply
// cycle. reason is one of "idle_timeout" or "reply_timeout".
func (c *Channel) releaseTimedOut(connectionID string, _ []byte, openedAt time.Time, reason string) {
	c.sess.release()

	c.pendingMu.Lock()
	delete(c.pending, connectionID)
	c.pendingMu.Unlock()

	duration := time.Since(openedAt)
	c.logger.Info().
		Str(ConnectionID, connectionID).
		Str("reason", reason).
		Dur("duration", duration).
		Msg("session_closed")
}

// replyStatusFor selects the final reply's ResponseStatus from the
// message metadata: a structured error code (an exhausted LLM chain —
// llm-status-visibility) makes the reply Status=error so machine consumers
// can distinguish a completed-but-failed request from a dead session; every
// other reply stays Status=ok.
func replyStatusFor(metadata map[string]string) protocol.ResponseStatus {
	if metadata[ErrorCode] != "" {
		return protocol.ResponseStatusError
	}

	return protocol.ResponseStatusOK
}

// shortIdentity returns the first 8 hex chars of identity for log fields.
// ZMQ auto-assigned identities are typically 5 bytes; user-set identities may
// be longer. Truncating to 8 chars avoids long strings in the log output while
// still being useful for correlation.
func shortIdentity(identity []byte) string {
	const maxHexChars = 8

	if len(identity) == 0 {
		return "<empty>"
	}

	// Fast path: identity is short enough that hex-encoding fits in maxHexChars.
	hex := fmt.Sprintf("%x", identity)
	if len(hex) <= maxHexChars {
		return hex
	}

	return hex[:maxHexChars]
}

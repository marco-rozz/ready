package tui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/go-zeromq/zmq4"
	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/mdrender"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// renderTickInterval is the coalesced progressive-render interval (D5). It is
// a code constant, not YAML config.
const renderTickInterval = 50 * time.Millisecond

// inputMinLines / inputCapPct govern the dynamic input-editor height (D11,
// amended by tui-usage-observability: Pi Agent mechanics — the growth cap is
// max(5, 30% of terminal rows); amended again: the base height is a single
// line (like Pi Agent's editor), growing with the typed content).
const (
	inputMinLines = 1
	inputCapPct   = 30 // 30% of terminal height
	inputCapFloor = 5  // the cap never goes below 5 lines
)

// userMessageInset is the horizontal inset (in cells) of the user echo text
// from the left/right edges of the transcript, mirroring Pi Agent's
// UserMessageComponent Box (outputPad = 1). The markdown is rendered at
// width - 2*inset so the inset block still spans the full viewport width.
const userMessageInset = 1

// userMessageBgDark / userMessageBgLight are the user-echo highlight
// background colours, taken verbatim from Pi Agent's dark.json/light.json
// themes ("userMsgBg").
const (
	userMessageBgDark  = "#343541"
	userMessageBgLight = "#e8e8e8"
)

// echoPromptPrefix is the dim marker shown before the first line of the
// user echo in the transcript — the same glyph as the input editor prompt.
const echoPromptPrefix = "❯❯ "

// spinnerFrame returns the braille working-spinner frame at idx (the design doc,
// Pi Agent's embedWorkingStatus shape). A function (not a global slice) keeps
// gochecknoglobals happy.
func spinnerFrame(idx int) string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	return frames[idx%len(frames)]
}

// reservedCommand maps the TUI-reserved slash commands (/session, /context)
// to their wire request types: intercepted at send, never
// forwarded to the model.
func reservedCommand(text string) (protocol.RequestType, bool) {
	switch text {
	case "/session":
		return protocol.RequestTypeSessionInfo, true
	case "/context":
		return protocol.RequestTypeContextUsage, true
	default:
		return "", false
	}
}

// turnState is the status-line turn state, derived from wire events (D17).
type turnState int

const (
	stateIdle turnState = iota
	stateThinking
	stateStreaming
	stateError
)

func (s turnState) Label() string {
	switch s {
	case stateThinking:
		return "thinking…"
	case stateStreaming:
		return "streaming…"
	case stateError:
		return "error"
	default:
		return "idle"
	}
}

// completedTurn stores the raw (un-redacted) markdown of a finalised turn:
// the user echo and the assistant reply. Redaction + glamour are applied at
// render time so a resize can re-render the whole transcript at the new width
// (D3 / D18). The "completed-turns render cache" is completedRenderd; it is
// invalidated (completedDirty) on finalise and on resize.
type completedTurn struct {
	echo  string
	reply string
	// info is a one-line informational transcript entry (session-lifecycle:
	// `── new session ──`, `── context compacted · … ──`) — faint styled
	// text, never markdown, never a turn.
	info string
}

// Options configures a TUI Run.
type Options struct {
	ConnectionID string
	// SessionID is the conversation Session ID (handshake), shown by the
	// info block; session_changed replaces it.
	SessionID    string
	ReplyTimeout time.Duration
	Sanitizer    *privacy.Sanitizer
	// ModelRef / ModelState seed the status line's model region from the
	// authenticated handshake (llm-status-visibility). ModelState is the
	// empty string when the daemon reported nothing — the region is then
	// omitted until a control event arrives.
	ModelRef   string
	ModelState protocol.ModelState
	// Usage seeds the status line's usage segment from the authenticated
	// handshake (tui-usage-observability): nil until the daemon has served
	// turns for the session key.
	Usage *protocol.UsageInfo
	// LivenessTimeout is the daemon's heartbeat window from the handshake
	// (zmq-session-liveness); the TUI derives the ping interval from it.
	// Zero keeps the built-in default interval (old daemon, metadata absent).
	LivenessTimeout time.Duration
	// LogoMode / DaemonAddress / ClientVersion configure the static head
	// block (chat-logo-modes): glyph-banner + info block, or info block alone.
	LogoMode      LogoMode
	DaemonAddress string
	ClientVersion string
}

// Model is the Bubble Tea model for the `ready chat` interactive TUI.
type Model struct {
	dealer       dealer
	sanitizer    *privacy.Sanitizer
	connectionID string
	timeout      time.Duration
	prompt       string

	width  int
	height int

	vp viewport.Model
	ta textarea.Model

	style         mdrender.Style
	styleResolved bool
	renderer      *mdrender.Renderer
	echoRenderer  *mdrender.Renderer // Markdown renderer for the inset user echo (width - 2*inset)

	state    turnState
	buffer   strings.Builder
	dirty    bool
	waiting  bool
	tickOn   bool
	quitting bool
	turnSeq  int // bumped per sent turn; timeoutMsg tags carry it (stale-timer fix)
	// timeoutSeq is bumped on every arm/re-arm of the per-turn reply timeout
	// (tui-progress-rearm-timeout). timeoutMsg.armSeq captures it at arm
	// time: since Bubble Tea commands cannot be cancelled, a turn that keeps
	// receiving progress accumulates multiple armed timers with the SAME
	// turnSeq — only the latest arm may end the turn (sliding deadline).
	timeoutSeq int

	exitReason ExitReason
	exitSet    bool

	completed        []completedTurn
	completedRenderd string
	completedDirty   bool

	curEcho     string // raw markdown of the current turn's user echo
	livePreview string // rendered current-turn preview (streamed or final)
	newContent  bool   // status-line "new content" indicator

	modelRef   string              // active model reference from the handshake / control events
	modelState protocol.ModelState // default | fallback | unavailable; "" until first report

	// usage is the last received usage totals (handshake seed + every final
	// reply, tui-usage-observability). nil until the first data arrives —
	// the usage segment stays hidden before that.
	usage *protocol.UsageInfo

	// Static head block (chat-logo-modes D3): the glyph banner (glyph mode
	// only) and the info block, rendered once at construction from
	// Options-carried facts. Stored separately from completedRenderd — they
	// are never conversation history, never re-rendered as markdown, and
	// never persisted; buildContent re-emits them as a fixed prefix.
	glyphBanner string
	infoBlock   string

	// Info-block facts kept for rebuilding it when the conversation Session
	// changes (session-lifecycle).
	sessionID     string
	daemonAddress string
	clientVersion string

	// Liveness heartbeat (zmq-session-liveness): pingInterval derives from
	// the handshake's liveness_timeout (timeout / 3, built-in default when
	// absent); pendingPings tracks in-flight ping request IDs so their
	// replies (and failures) stay inert — never disturbing turn state.
	pingInterval time.Duration
	pendingPings map[string]struct{}

	// pendingCtrl maps the request IDs of in-flight reserved slash commands
	// (/session, /context) to their request type, so their replies render as
	// transcript blocks even while a model turn is in flight.
	pendingCtrl map[string]protocol.RequestType

	// pending is the steering queue (Amendment 3): prompts submitted while a
	// turn is in flight. FIFO: the first entry is sent automatically when
	// the in-flight turn completes (startNextQueuedTurn), each as a normal
	// turn with a fresh reply timeout. The daemon never sees concurrent
	// message requests.
	pending []string

	spinnerIdx int // working-spinner frame index, advanced by the render tick

	// Mouse ownership (Amendment 3, mouse always captured): content is the
	// last plain (un-highlighted) viewport content built by buildContent;
	// a drag renders an in-app selection over it (selection.go). copiedMsg
	// is the transient status-line copy confirmation.
	content   string
	selActive bool
	selAnchor selCell
	selHead   selCell
	copiedMsg string
}

// NewModel builds the initial TUI model. width/height default to 80/24 until
// the first tea.WindowSizeMsg arrives (D18 fallback).
func NewModel(d dealer, opts Options) *Model {
	m := &Model{
		dealer:       d,
		sanitizer:    opts.Sanitizer,
		connectionID: opts.ConnectionID,
		timeout:      opts.ReplyTimeout,
		prompt:       echoPromptPrefix,
		width:        80,
		height:       24,
		style:        mdrender.StyleDark, // dark until BackgroundColorMsg arrives (D6)
		state:        stateIdle,
		modelRef:     opts.ModelRef,
		modelState:   opts.ModelState,
		usage:        opts.Usage,
		pendingCtrl:  make(map[string]protocol.RequestType),

		sessionID:     opts.SessionID,
		daemonAddress: opts.DaemonAddress,
		clientVersion: opts.ClientVersion,
	}

	// Static head block (chat-logo-modes D3): rendered once at construction
	// from the handshake-carried facts. Width-independent by design (short
	// labelled lines), so resize re-renders re-emit it byte-identical. The
	// model reference is NOT part of it — the status line is its only home.
	m.infoBlock = buildInfoBlock(opts.DaemonAddress, opts.SessionID, opts.ClientVersion)
	if opts.LogoMode == LogoGlyph {
		m.glyphBanner = renderGlyphBanner()
	}
	if m.timeout <= 0 {
		m.timeout = 120 * time.Second
	}

	// Liveness heartbeat cadence (zmq-session-liveness): a third of the
	// daemon's release window, so three missed pings trigger the release.
	m.pingInterval = defaultPingInterval
	if opts.LivenessTimeout > 0 {
		m.pingInterval = opts.LivenessTimeout / 3
	}
	m.pendingPings = make(map[string]struct{})

	m.vp = viewport.New()
	m.vp.MouseWheelEnabled = true
	m.vp.MouseWheelDelta = 3

	m.ta = textarea.New()
	// The bubbles default paints the cursor line with a solid background
	// (black in dark mode), which reads as a darker inner band inside the
	// input area. Strip it so the editor keeps the terminal's own background.
	m.ta.SetStyles(textareaStyles(true)) // dark until BackgroundColorMsg resolves (D6)
	// shift+enter reaches us either in band (kitty keyboard protocol or xterm
	// modifyOtherKeys, both negotiated by Bubble Tea) or, on terminals that
	// implement neither, through the out-of-band OS modifier probe in
	// shiftenter.go. Neither path is guaranteed on every terminal, so ctrl+j —
	// a plain control byte (LF, 0x0A) every terminal parses unambiguously —
	// stays the binding advertised as always reliable.
	m.ta.Placeholder = "type a message…  (enter: send · ctrl+j/shift+enter: newline · ctrl+d: quit)"
	m.ta.Prompt = ""             // prompt is rendered as a lipgloss prefix outside the buffer
	m.ta.ShowLineNumbers = false // cleaner chat input
	_ = m.ta.Focus()             // focus so keystrokes insert; cursor-blink cmd ignored pre-Run

	return m
}

// Guarantees at compile time that *Model implements tea.Model.
var _ tea.Model = (*Model)(nil)

// textareaStyles returns the textarea styles for the given background
// darkness with the cursor-line background removed, so the input area keeps
// the terminal's own background instead of the bubbles default band (black
// on dark, near-white on light). Applied at construction and re-applied on
// every tea.BackgroundColorMsg so light terminals get the light variants.
func textareaStyles(isDark bool) textarea.Styles {
	s := textarea.DefaultStyles(isDark)
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Blurred.CursorLine = lipgloss.NewStyle()

	return s
}

// Init starts the DEALER receive command and requests the terminal background
// color. The render tick starts on the first send (see sendInput).
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.recvCmd(),
		m.pingTickCmd(),
		tea.RequestBackgroundColor,
	)
}

// Update handles tea messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.invalidateRenderers()
		m.completedDirty = true
		m.applyLayout()
		m.rebuildViewport()

		return m, nil

	case tea.BackgroundColorMsg:
		m.style = mdrender.StyleForBackground(msg.IsDark())
		m.styleResolved = true
		m.invalidateRenderers()
		m.ta.SetStyles(textareaStyles(msg.IsDark()))
		m.completedDirty = true
		m.rebuildViewport()

		return m, nil

	case tea.InterruptMsg:
		return m.quit(exitSIGINT, "")

	case tickMsg:
		return m.handleTick()

	case progressChunk:
		return m.handleProgressChunk(msg)

	case replyFinal:
		if m.quitting {
			return m, nil
		}

		return m.handleReply(msg.Resp)

	case controlAfter:
		if m.quitting {
			return m, nil
		}

		return m.handleControl(msg.Resp)

	case pingTickMsg:
		if m.quitting || m.pingInterval <= 0 {
			return m, nil
		}

		// Best-effort heartbeat (zmq-session-liveness D5): send failures are
		// swallowed inside the command — a dying client is the case the
		// mechanism exists to detect, not an error to surface.
		reqID := uuid.New().String()
		m.pendingPings[reqID] = struct{}{}

		return m, tea.Batch(m.sendPingCmd(reqID), m.pingTickCmd())

	case timeoutMsg:
		if m.quitting || !m.waiting || msg.turnSeq != m.turnSeq || msg.armSeq != m.timeoutSeq {
			// Two tags, one filter: the turn sequence kills a timer left over
			// from a PREVIOUS turn; the arm sequence kills a timer superseded
			// by a re-arm of the CURRENT turn (re-arming adds timers because
			// Bubble Tea commands cannot be cancelled — without the arm tag
			// the earliest send-armed timer would fire first and end a turn
			// that kept producing progress).
			return m, nil
		}

		return m.quit(exitTimeout, "timeout")

	case sentMsg:
		return m, nil

	case errMsg:
		if m.quitting {
			return m, nil
		}

		return m.quit(exitErr, msg.Error())

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseWheelMsg, tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, copyStatusExpireMsg:
		model, cmd, _ := m.handleMouseEvent(msg)

		return model, cmd

	case tea.PasteMsg:
		return m.handlePaste(msg)

	case tea.QuitMsg:
		return m, nil

	default:
		return m, nil
	}
}

// handleKey implements the keybinding matrix.
func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.Key().Keystroke() {
	case "enter":
		return m.sendInput()

	case "shift+enter", "alt+enter", "ctrl+j":
		m.ta.InsertString("\n")
		m.applyLayout()

		return m, nil

	case "ctrl+c":
		return m.quit(exitSIGINT, "")

	case "ctrl+d":
		if m.ta.Value() == "" {
			return m.quit(exitOK, "")
		}

		// Non-empty: keep the textarea default forward-delete.
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(tea.KeyPressMsg(msg.Key()))

		return m, cmd

	case "ctrl+g":
		// Jump-to-bottom key (vim convention is "G", but a bare "G" must type
		// into the input editor; Ctrl+G is an unambiguous, non-conflicting
		// binding satisfying the "jump-to-bottom key" requirement).
		m.vp.GotoBottom()
		m.newContent = false

		return m, nil

	case "pgup", "pgdown":
		var vcmd tea.Cmd
		m.vp, vcmd = m.vp.Update(tea.KeyPressMsg(msg.Key()))
		_ = vcmd
		m.syncAutoFollow()

		return m, nil

	default:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(tea.KeyPressMsg(msg.Key()))
		m.applyLayout()

		return m, cmd
	}
}

// sendInput handles RETURN: reserved slash commands (/session, /context) are
// intercepted at send and transmitted as their control request
// types — no user-turn echo, no history entry, no thinking state; the reply
// renders as a transcript block when it arrives. Everything else is echoed,
// the editor is cleared, the message is sent, and the per-turn timeout +
// render tick armed (D13 / D17).
//
// Steering (Amendment 3): while a turn is in flight the prompt is QUEUED
// locally instead of sent — the daemon structurally rejects concurrent
// message requests (request_in_flight), and an unguarded second send used to
// corrupt the client itself (echo overwritten, live stream wiped, orphaned
// timeout). Queued prompts appear as pending transcript blocks and are sent
// FIFO on turn completion; the reply timeout is armed only on an actual
// send, so exactly one timeout per sent turn.
func (m *Model) sendInput() (tea.Model, tea.Cmd) {
	text := m.ta.Value()
	if text == "" {
		return m, nil
	}

	if m.waiting {
		// Steering: queue the prompt, clear the editor, send nothing.
		m.pending = append(m.pending, text)
		m.ta.Reset()
		m.applyLayout()
		m.rebuildViewport()

		return m, nil
	}

	// Reserved slash commands: intercept and forward as control requests.
	if reqType, ok := reservedCommand(text); ok {
		reqID := uuid.New().String()
		m.pendingCtrl[reqID] = reqType

		return m, m.sendCmdWithID(reqType, "", reqID)
	}

	// Echo at send (raw markdown stored; rendered at draw time).
	m.curEcho = text
	m.ta.Reset()
	m.state = stateThinking
	m.buffer.Reset()
	m.livePreview = ""
	m.dirty = false
	m.waiting = true
	m.tickOn = true
	m.turnSeq++
	m.timeoutSeq++
	// applyLayout already sized the viewport for the editor's new (empty)
	// content. Rebuild now: the user echo is visible at send time, before any
	// progress chunk or final reply arrives.
	m.applyLayout()
	m.rebuildViewport()

	return m, tea.Batch(
		m.sendCmd(protocol.RequestTypeMessage, text),
		m.timeoutCmd(),
		m.tickCmd(),
	)
}

// sendCmd builds and queues a protocol request with a fresh request ID.
func (m *Model) sendCmd(reqType protocol.RequestType, content string) tea.Cmd {
	return m.sendCmdWithID(reqType, content, uuid.New().String())
}

// sendCmdWithID builds and queues a protocol request carrying a caller-chosen
// request ID (reserved commands track theirs for reply matching).
func (m *Model) sendCmdWithID(reqType protocol.RequestType, content, reqID string) tea.Cmd {
	payload, _ := protocol.Marshal(protocol.Request{
		ConnectionID: m.connectionID,
		Type:         reqType,
		RequestID:    reqID,
		Content:      content,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	d := m.dealer

	return func() tea.Msg {
		if err := d.Send(zmq4.NewMsg(payload)); err != nil {
			return errMsg{err}
		}

		return sentMsg{}
	}
}

// handleReply finalises a turn from the authoritative reply (D3 / task 3.7).
func (m *Model) handleReply(resp protocol.Response) (tea.Model, tea.Cmd) {
	// tui-usage-observability: every received reply carrying usage refreshes
	// the footer's usage segment.
	if resp.Usage != nil {
		m.usage = resp.Usage
	}

	// Reserved-command reply (/session, /context): render as a transcript
	// block, whatever the turn state is — it never belongs to the in-flight
	// model turn.
	if _, isCmd := m.pendingCtrl[resp.RequestID]; isCmd {
		delete(m.pendingCtrl, resp.RequestID)

		switch resp.Status {
		case protocol.ResponseStatusShutdown:
			return m.quit(exitOK, "daemon shut down")
		case protocol.ResponseStatusError:
			m.completeCommandBlock("**Error:** `" + resp.ErrorCode + "`")
		default:
			m.completeCommandBlock(resp.Content)
		}

		return m, m.recvCmd()
	}

	switch resp.Status {
	case protocol.ResponseStatusShutdown:
		return m.quit(exitOK, "daemon shut down")

	case protocol.ResponseStatusError:
		// llm-status-visibility: an exhausted LLM chain is a COMPLETED
		// request, not a session failure — render the terminal message as the
		// turn's completion and keep the session open. Every other error
		// stays on the quit path (no_active_session / session_timeout remain
		// terminal, exit 1).
		if resp.ErrorCode == protocol.ErrorCodeLLMUnavailable {
			return m.handleLLMUnavailable(resp)
		}

		return m.handleServerError(resp, false)

	default:
		// ok — render the reply as the turn's final content, then drain the
		// steering queue (the first pending prompt becomes the live turn).
		drain := m.completeTurn(resp.Content)

		return m, tea.Batch(m.recvCmd(), drain)
	}
}

// handleLLMUnavailable finalises a turn whose LLM chain was exhausted
// (llm-status-visibility): the supplied terminal message is rendered as the
// completed assistant turn and the TUI returns to idle with the session
// still open. Any partial stream content delivered before the failure is
// retained — the transcript shows the partial chunks followed by the
// warning, never a replacement or silent drop.
func (m *Model) handleLLMUnavailable(resp protocol.Response) (tea.Model, tea.Cmd) {
	reply := resp.Content
	if partial := m.buffer.String(); partial != "" {
		reply = partial + "\n\n" + reply
	}

	drain := m.completeTurn(reply)

	return m, tea.Batch(m.recvCmd(), drain)
}

// appendInfoLine adds a one-line informational entry to the transcript
// (never a turn, never an echo).
func (m *Model) appendInfoLine(line string) {
	m.completed = append(m.completed, completedTurn{info: line})
	m.completedDirty = true
	m.rebuildViewport()
}

// completeCommandBlock appends a reserved-command reply (/session, /context)
// to the transcript as a standalone block: no user echo, no turn state
// change, never touching an in-flight turn's live preview.
func (m *Model) completeCommandBlock(content string) {
	m.completed = append(m.completed, completedTurn{echo: "", reply: content})
	m.completedDirty = true
	m.rebuildViewport()
}

// completeTurn finalises the current turn with the given reply content:
// archives the echo + reply as a completed turn, clears the live preview,
// and stops the render tick. Shared by the success path and the
// llm_unavailable path. Returns the command that sends the first queued
// steering prompt (nil when the queue is empty).
func (m *Model) completeTurn(reply string) tea.Cmd {
	m.completed = append(m.completed, completedTurn{echo: m.curEcho, reply: reply})
	m.curEcho = ""
	m.buffer.Reset()
	m.livePreview = ""
	m.state = stateIdle
	m.waiting = false
	m.tickOn = false
	m.completedDirty = true
	m.rebuildViewport()

	return m.startNextQueuedTurn()
}

// startNextQueuedTurn promotes the first queued steering prompt to the live
// turn: echo it, clear the preview state, arm a FRESH reply timeout (one per
// sent turn — a queued prompt never inherits the previous turn's), and send.
// nil when the queue is empty.
func (m *Model) startNextQueuedTurn() tea.Cmd {
	if len(m.pending) == 0 {
		return nil
	}

	text := m.pending[0]
	m.pending = m.pending[1:]
	m.curEcho = text
	m.buffer.Reset()
	m.livePreview = ""
	m.dirty = false
	m.state = stateThinking
	m.waiting = true
	m.tickOn = true
	m.turnSeq++
	m.timeoutSeq++
	m.applyLayout()
	m.rebuildViewport()

	return tea.Batch(
		m.sendCmd(protocol.RequestTypeMessage, text),
		m.timeoutCmd(),
		m.tickCmd(),
	)
}

// handleControl consumes a post-handshake control message (D10 / task 3.4).
// Model-status events (llm-status-visibility) update only the model
// presentation — turn state and transcript content are untouched and the
// TUI keeps listening.
func (m *Model) handleControl(resp protocol.Response) (tea.Model, tea.Cmd) {
	switch resp.Status {
	case protocol.ResponseStatusShutdown:
		return m.quit(exitOK, "daemon shut down")

	case protocol.ResponseStatusError:
		// zmq-session-liveness: a heartbeat reply that errors is inert — an
		// old daemon rejects the unknown request type during strict decode
		// (malformed_request, no request id echoed), and a session already
		// released daemon-side is surfaced at the next real interaction
		// A failed ping never quits the TUI.
		if m.isPingReply(resp) {
			return m, m.recvCmd()
		}

		return m.handleServerError(resp, false)

	default:
		// Model-status event: refresh the status line's model region without
		// disturbing anything else.
		if resp.ModelState != "" {
			m.modelRef = resp.ModelRef
			m.modelState = resp.ModelState
		}

		// Session lifecycle events (session-lifecycle): informational
		// transcript lines only — the turn state is untouched.
		switch resp.Content {
		case protocol.ControlSessionChanged:
			m.usage = nil // the new Session starts from zero; hidden until data arrives
			m.sessionID = resp.SessionID
			m.infoBlock = buildInfoBlock(m.daemonAddress, m.sessionID, m.clientVersion)
			m.appendInfoLine("── new session ──")
		case protocol.ControlCompacted:
			if c := resp.Compaction; c != nil {
				m.appendInfoLine(fmt.Sprintf("── context compacted · %s → %s tokens ──",
					roundTokens(c.TokensBefore), roundTokens(c.TokensAfter)))
			}
		}

		// Consume the ping bookkeeping on success (request IDs are one-shot).
		delete(m.pendingPings, resp.RequestID)

		// Unknown control: ignore and keep listening.
		return m, m.recvCmd()
	}
}

// isPingReply reports whether an error response belongs to a liveness
// heartbeat: either it matches an in-flight ping request id, or it is an old
// daemon's malformed_request rejection (strict codec rejects the unknown
// "ping" type before any request-id echo is possible).
func (m *Model) isPingReply(resp protocol.Response) bool {
	if resp.RequestID != "" {
		if _, ok := m.pendingPings[resp.RequestID]; ok {
			delete(m.pendingPings, resp.RequestID)

			return true
		}

		return false
	}

	return resp.ErrorCode == "malformed_request"
}

// handleServerError sets the turn state to error and maps the error code to an
// exit reason. A `no_active_session` (or defensively `session_timeout`) error
// surfaces "session expired"; any other error surfaces the raw ErrorCode.
func (m *Model) handleServerError(resp protocol.Response, _ bool) (tea.Model, tea.Cmd) {
	// Belt-and-braces (Amendment 3 steering): with the local queue the
	// client never sends a second request while one is in flight, so this
	// daemon rejection must never arrive. If it ever does (older binary,
	// out-of-band writer), the live turn is untouched — keep waiting instead
	// of tearing the session down.
	if resp.ErrorCode == "request_in_flight" {
		return m, m.recvCmd()
	}

	m.state = stateError
	m.waiting = false
	m.tickOn = false

	msg := "error: " + resp.ErrorCode
	if resp.ErrorCode == "no_active_session" || resp.ErrorCode == "session_timeout" {
		msg = "session expired"
	}

	return m.quit(exitErr, msg)
}

// quit sets the exit reason (once), marks the model quitting, and returns a
// command that closes the dealer (unblocking the in-flight recv) and quits the
// program (D10). The TUI never calls os.Exit.
func (m *Model) quit(code int, msg string) (tea.Model, tea.Cmd) {
	if !m.exitSet {
		m.exitReason = ExitReason{Code: code, Message: msg}
		m.exitSet = true
	}

	m.quitting = true
	m.tickOn = false
	m.waiting = false

	return m, func() tea.Msg {
		_ = m.dealer.Close()

		return tea.Quit()
	}
}

// ExitReason returns the model's final exit reason (zero value until quit).
func (m *Model) ExitReason() ExitReason { return m.exitReason }

// recvCmd blocks on dealer.Recv, parses the Response, and returns a typed msg.
// Empty/unparseable frames are skipped by re-issuing the command without
// surfacing a message (D9 / task 3.4).
func (m *Model) recvCmd() tea.Cmd {
	d := m.dealer

	return func() tea.Msg {
		for {
			zmsg, err := d.Recv()
			if err != nil {
				return errMsg{err}
			}

			if len(zmsg.Frames) == 0 {
				continue
			}

			var resp protocol.Response
			if parseErr := decodeResponse(zmsg.Frames[len(zmsg.Frames)-1], &resp); parseErr != nil {
				continue // malformed frame: skip, keep listening
			}

			switch resp.ResponseType {
			case protocol.ResponseTypeProgress:
				return progressChunk{Content: resp.Content, Usage: resp.Usage}
			case protocol.ResponseTypeReply:
				return replyFinal{Resp: resp}
			case protocol.ResponseTypeControl:
				return controlAfter{Resp: resp}
			default:
				// Legacy: messages without response_type are treated as replies.
				return replyFinal{Resp: resp}
			}
		}
	}
}

// handlePaste inserts bracketed-paste content without sending.
func (m *Model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	m.ta.InsertString(msg.Content)
	m.applyLayout()

	return m, nil
}

// handleTick advances the working spinner (task 6.4), flushes the live
// preview when dirty, and re-issues the render tick while active.
func (m *Model) handleTick() (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}

	if m.working() {
		m.spinnerIdx++
	}

	if m.dirty {
		m.livePreview = m.renderMarkdown(m.buffer.String())
		m.dirty = false
		m.rebuildViewport()
	}

	if m.tickOn {
		return m, m.tickCmd()
	}

	return m, nil
}

// handleProgressChunk applies one streamed progress notification: usage
// totals, live buffer, turn state and the reply-timeout re-arm
// (tui-progress-rearm-timeout). Each progress chunk is proof of liveness, so
// the effective deadline becomes time-since-last-progress, not
// time-since-send: timeoutSeq is bumped first so the new arm supersedes every
// earlier one; the stale timers stay behind (commands cannot be cancelled)
// and are filtered out in the timeoutMsg handler via the arm-sequence tag.
func (m *Model) handleProgressChunk(msg progressChunk) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}

	// Live usage totals ride the progress frame (tui-usage-
	// observability): update the footer counters mid-turn. Nil keeps the
	// last known totals (fresh sessions, legacy daemons).
	if msg.Usage != nil {
		m.usage = msg.Usage
	}

	if m.quitting {
		return m, nil
	}

	// Live usage totals ride the progress frame (tui-usage-
	// observability): update the footer counters mid-turn. Nil keeps the
	// last known totals (fresh sessions, legacy daemons).
	if msg.Usage != nil {
		m.usage = msg.Usage
	}

	m.buffer.WriteString(msg.Content)
	m.dirty = true
	if m.state == stateThinking {
		m.state = stateStreaming
	}

	m.timeoutSeq++

	return m, tea.Batch(m.recvCmd(), m.timeoutCmd())
}

// timeoutCmd sleeps for the per-turn reply timeout and returns timeoutMsg.
// The message is tagged with the turn sequence current at arm time: a late
// timeout is neutralised by the waiting flag (D10) and by the turn-sequence
// comparison in the Update handler — a timer left over from an older turn
// must never kill a newer turn still in flight.
//
// The timeout is an inactivity deadline, not a fixed turnaround budget: it is
// armed at send and re-armed by every progress chunk of the in-flight turn
// (tui-progress-rearm-timeout), so a chain of re-armed timers makes the
// effective deadline time-since-last-progress, not time-since-send. Each arm
// bumps timeoutSeq BEFORE the command is issued; a fired timer is acted on
// only when BOTH its tags match, so superseded timers never end a turn that
// kept producing progress.
func (m *Model) timeoutCmd() tea.Cmd {
	seq := m.turnSeq
	arm := m.timeoutSeq // captured at arm time: latest arm always wins
	t := m.timeout

	return func() tea.Msg {
		time.Sleep(t)

		return timeoutMsg{turnSeq: seq, armSeq: arm}
	}
}

// pingTickCmd schedules the next liveness heartbeat (zmq-session-liveness).
// Re-issued from the pingTickMsg handler until the TUI quits.
func (m *Model) pingTickCmd() tea.Cmd {
	interval := m.pingInterval

	return tea.Tick(interval, func(time.Time) tea.Msg { return pingTickMsg{} })
}

// sendPingCmd fires one heartbeat request, swallowing send errors (design
// D5): if the dealer is closed or the network is gone, the daemon's liveness
// watchdog does the release — the TUI is dying or dead by definition.
func (m *Model) sendPingCmd(reqID string) tea.Cmd {
	payload, _ := protocol.Marshal(protocol.Request{
		ConnectionID: m.connectionID,
		Type:         protocol.RequestTypePing,
		RequestID:    reqID,
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	})
	d := m.dealer

	return func() tea.Msg {
		_ = d.Send(zmq4.NewMsg(payload))

		return nil
	}
}

// defaultPingInterval is the heartbeat cadence when the daemon did not
// negotiate a liveness_timeout in the handshake (old daemon): 15s, the
// interval implied by the daemon-side default of 45s.
const defaultPingInterval = 15 * time.Second

// tickCmd is the coalesced 50 ms render tick (D5), re-issued after every
// firing while tickOn is true.
func (m *Model) tickCmd() tea.Cmd {
	return tea.Tick(renderTickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// renderMarkdown redacts + sanitises + glamour-renders raw markdown using the
// current renderer (D4 / D6). glamour pads its output with a leading and
// trailing newline; both are trimmed so callers joining blocks don't inherit
// spurious blank rows at every block boundary.
func (m *Model) renderMarkdown(raw string) string {
	return strings.Trim(m.ensureRenderer().Render(raw), "\n")
}

// ensureRenderer lazily constructs the renderer if it has not been built yet
// (e.g. before the first WindowSizeMsg, using the 80-column fallback — D18).
func (m *Model) ensureRenderer() *mdrender.Renderer {
	if m.renderer == nil {
		m.renderer = mdrender.New(m.width, m.style, m.sanitizer)
	}

	return m.renderer
}

// invalidateRenderers drops the cached renderers so the next render rebuilds
// them at the current width/style.
func (m *Model) invalidateRenderers() {
	m.renderer = nil
	m.echoRenderer = nil
}

// renderEcho renders a user-turn echo block: the dim `❯❯` prefix on the
// first content line, followed by the rendered markdown, all wrapped in the
// Pi Agent user-message highlight band (D13).
func (m *Model) renderEcho(raw string) string {
	// Trim glamour's leading/trailing newlines before prepending the prefix,
	// so `❯❯` lands on the first content line instead of its own band row.
	rendered := strings.Trim(m.ensureEchoRenderer().Render(raw), "\n")

	prefix := lipgloss.NewStyle().Faint(true).Render(echoPromptPrefix)

	return userMessageBlock(prefix+rendered, m.width, m.style == mdrender.StyleDark)
}

// ensureEchoRenderer lazily constructs the user-echo renderer at the inset
// width (viewport width minus the two one-cell side insets and the `❯❯`
// prefix, which occupies band space on the first line), mirroring
// ensureRenderer's 80-column fallback before the first WindowSizeMsg.
func (m *Model) ensureEchoRenderer() *mdrender.Renderer {
	if m.echoRenderer == nil {
		w := m.width - 2*userMessageInset - lipgloss.Width(echoPromptPrefix)
		if w < 1 {
			w = 1
		}
		m.echoRenderer = mdrender.New(w, m.style, m.sanitizer)
	}

	return m.echoRenderer
}

// userMessageBlock wraps already-rendered markdown in the Pi Agent user
// message shape (UserMessageComponent → Box): the text sits one cell from
// the left and right edges, every line is highlighted on a full-width
// background band, and one background-filled blank line separates the block
// from the surrounding transcript (Box paddingY = 1).
//
// glamour terminates every styled span with an SGR reset, which would also
// drop the background mid-line — the row would keep the band only up to the
// first styled span. Each reset inside the line is therefore rewritten to
// "full reset + re-paint of the band colour", so attributes are still cleared
// but the highlight survives to the end of the row.
func userMessageBlock(rendered string, width int, dark bool) string {
	bg := lipgloss.NewStyle().Background(lipgloss.Color(userMessageBgDark))
	if !dark {
		bg = lipgloss.NewStyle().Background(lipgloss.Color(userMessageBgLight))
	}

	// bgSeq is the SGR sequence lipgloss emits for the block background,
	// recovered from a probe render instead of hardcoding truecolor so a
	// reduced colour profile degrades everywhere consistently.
	resetWithBg := "\x1b[0m"
	if probe := bg.Render("x"); strings.Contains(probe, "x") {
		resetWithBg += strings.SplitN(probe, "x", 2)[0]
	}
	// One simultaneous pass: resetWithBg itself contains \x1b[0m, and a
	// sequential ReplaceAll chain would re-match its own output.
	deReset := strings.NewReplacer(
		"\x1b[m", resetWithBg,
		"\x1b[0m", resetWithBg,
		"\x1b[49m", resetWithBg,
	)

	if width < 1 {
		width = 1
	}

	rendered = strings.Trim(rendered, "\n")
	if rendered == "" {
		return ""
	}

	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines)+2)
	out = append(out, bg.Render(strings.Repeat(" ", width))) // top padding band
	for _, line := range lines {
		line = deReset.Replace(line)
		line = " " + line + " " // one-cell inset on both edges
		if pad := width - lipgloss.Width(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		out = append(out, bg.Render(line))
	}
	out = append(out, bg.Render(strings.Repeat(" ", width))) // bottom padding band

	return strings.Join(out, "\n")
}

// renderCompleted re-renders all completed turns at the current width. One
// blank row separates the echo block from its reply, and one blank line
// separates consecutive turns.
func (m *Model) renderCompleted() string {
	blocks := make([]string, 0, len(m.completed))
	for _, t := range m.completed {
		if t.info != "" {
			blocks = append(blocks, infoLabelStyle().Render(t.info))

			continue
		}

		var b strings.Builder
		if t.echo != "" {
			b.WriteString(m.renderEcho(t.echo))
		}
		if t.reply != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(m.renderMarkdown(t.reply))
		}
		blocks = append(blocks, b.String())
	}

	return strings.Join(blocks, "\n\n")
}

// buildContent assembles the viewport content: the static head block (glyph
// banner + info block, or info block alone — chat-logo-modes), cached
// completed turns, current user echo, and the live (streamed or final)
// assistant preview (D3). The head block is a fixed prefix, identical across
// re-renders: it is styled text, never re-processed as markdown.
func (m *Model) buildContent() string {
	head := m.headBlock()

	var parts []string
	if head != "" {
		parts = append(parts, head)
		if m.completedRenderd != "" || m.curEcho != "" || m.livePreview != "" {
			parts = append(parts, "") // blank row between the head block and the transcript
		}
	}
	if m.completedRenderd != "" {
		parts = append(parts, m.completedRenderd)
	}
	if m.curEcho != "" {
		parts = append(parts, m.renderEcho(m.curEcho))
	}
	if m.livePreview != "" {
		if len(parts) > 0 {
			parts = append(parts, "") // blank row between echo block and live reply
		}
		parts = append(parts, m.livePreview)
	}

	// Steering queue (Amendment 3): prompts submitted while a turn is in
	// flight render as pending echo blocks below the live reply — visible
	// ahead of their turn, sent FIFO on completion.
	for _, text := range m.pending {
		if len(parts) > 0 {
			parts = append(parts, "")
		}
		parts = append(parts, m.renderEcho(text))
	}

	return strings.Join(parts, "\n")
}

// headBlock composes the static viewport prefix: the glyph banner (glyph
// mode only, always above the info block) followed by the info block. Empty
// when neither is present (never the case with the built-in info block).
func (m *Model) headBlock() string {
	switch {
	case m.glyphBanner != "" && m.infoBlock != "":
		return m.glyphBanner + "\n\n" + m.infoBlock
	case m.glyphBanner != "":
		return m.glyphBanner
	default:
		return m.infoBlock
	}
}

// rebuildViewport refreshes the viewport content and applies smart auto-follow
// (D14): GotoBottom only when already at bottom; otherwise set the new-content
// indicator. Any transcript content or layout change cancels an in-progress
// selection (Amendment 3: no mobile anchors — a selection never survives
// content moving under it).
func (m *Model) rebuildViewport() {
	m.cancelSelection()

	if m.completedDirty {
		m.completedRenderd = m.renderCompleted()
		m.completedDirty = false
	}

	atBottom := m.vp.AtBottom()
	m.content = m.buildContent()
	m.vp.SetContent(m.content)
	if atBottom {
		m.vp.GotoBottom()
		m.newContent = false
	} else {
		m.newContent = true
	}
}

// refreshSelection re-applies the selection overlay to the stored plain
// content without touching the transcript or auto-follow: the click/motion
// mouse path re-renders only the highlight, never the underlying content.
func (m *Model) refreshSelection() {
	if m.selActive {
		m.vp.SetContent(m.applySelection(m.content))

		return
	}

	m.vp.SetContent(m.content)
}

// finishSelection finalises the drag: extract the rendered text (ANSI
// stripped), copy it to the system clipboard, confirm on the status line,
// and restore the plain content. A plain click (empty selection) just
// clears the highlight.
func (m *Model) finishSelection() (tea.Model, tea.Cmd) {
	text := m.extractSelection()
	m.cancelSelection()
	m.refreshSelection()

	if text == "" {
		return m, nil
	}

	m.copiedMsg = fmt.Sprintf("copied %d chars", len(text))

	return m, tea.Batch(m.copyToClipboard(text), clearCopyStatusCmd())
}

// syncAutoFollow clears the new-content indicator once the user scrolls back
// to the bottom (D14).
func (m *Model) syncAutoFollow() {
	if m.vp.AtBottom() {
		m.newContent = false
	}
}

// applyLayout sizes the viewport, textarea, and prompt for the current
// terminal dimensions and input content (D11).
func (m *Model) applyLayout() {
	if m.height <= 0 || m.width <= 0 {
		return
	}

	inH := m.computeInputHeight()
	promptW := lipgloss.Width(m.prompt)

	m.ta.SetHeight(inH)
	if w := m.width - promptW; w > 0 {
		m.ta.SetWidth(w)
	}

	m.vp.SetWidth(m.width)
	vpH := m.height - inH - 3 // -1 status line, -2 input bars (task 6.1)
	if vpH < 1 {
		vpH = 1
	}
	m.vp.SetHeight(vpH)
}

// computeInputHeight returns the dynamic input-editor height: grows with the
// number of typed lines, bounded by [inputMinLines, max(5, 30% of terminal
// height)] (tui-usage-observability task 6.2: the Pi Agent cap). The editor
// scrolls internally past the cap (bubbles textarea cursor-following
// viewport); the bars carry the hidden-line indicators.
func (m *Model) computeInputHeight() int {
	lines := strings.Count(m.ta.Value(), "\n") + 1
	maxH := m.height * inputCapPct / 100
	if maxH < inputCapFloor {
		maxH = inputCapFloor
	}
	h := lines
	if h < inputMinLines {
		h = inputMinLines
	}
	if h > maxH {
		h = maxH
	}

	return h
}

// View renders the three regions with lipgloss. The session runs in the
// ALTERNATE SCREEN (banner-in-transcript); mouse capture is conditional on
// turn-in-flight: at rest the mouse belongs to the terminal, so native
// selection over the rendered transcript works without modifiers, and —
// because the alt buffer has no scrollback — an at-rest wheel can no longer
// escape into the pre-session content (the regression the permanent-capture
// attempt tried to fix). During a turn the capture comes back on so the
// wheel scrolls the internal viewport.
func (m *Model) View() tea.View {
	m.applyLayout()

	transcript := m.vp.View()
	prompt := lipgloss.NewStyle().Faint(true).Render(m.prompt)
	input := lipgloss.JoinHorizontal(lipgloss.Top, prompt, m.ta.View())

	topBar, bottomBar := m.inputBars()
	status := m.statusLine()

	body := lipgloss.JoinVertical(lipgloss.Left, transcript, topBar, input, bottomBar, status)

	v := tea.NewView(body)
	// banner-in-transcript: alt-screen for the whole session; mouse capture
	// is PERMANENT (Amendment 3, mouse ownership). The client never hands
	// the mouse back to the terminal: the wheel always scrolls the internal
	// viewport, and selection + copy are TUI-managed (selection.go) — drag
	// → reverse-video highlight, release → copy the rendered text to the
	// system clipboard (OSC52 / background pbcopy). Native terminal
	// selection is never relied upon, so terminal mouse-reporting quirks
	// (macOS Terminal.app has no selection override while reporting is
	// active) are irrelevant by construction. Both prior policies —
	// permanent capture WITHOUT managed selection (Amendment 1) and
	// conditional capture handing the mouse back at rest (Amendment 2) —
	// each delivered only half of scroll+select+copy in every state.
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	return v
}

// statusLine renders the persistent status line (D15 / task 3.9, extended
// by llm-status-visibility, then amended by tui-usage-observability): turn
// state label (always present in the bottom-left corner, including thinking
// and streaming), live model state, the usage
// segment, and a "new content" indicator when scrolled up during streaming.
// The session ID and CWD are NOT in the footer — they moved to the /session
// command. The model region is coloured by priority, not identity: green
// for the configured default, yellow for an active fallback, red for
// unavailable. It is omitted until the daemon first reports a
// model status.
func (m *Model) statusLine() string {
	parts := make([]string, 0, 5)
	// The turn-state label is always present (bottom-left corner); while a
	// turn is in flight the working spinner is additionally embedded in the
	// input's top border.
	parts = append(parts, m.state.Label())
	if region := m.modelRegion(); region != "" {
		parts = append(parts, region)
	}
	if seg := m.usageSegment(); seg != "" {
		parts = append(parts, seg)
	}
	if m.newContent {
		parts = append(parts, "↓ new")
	}
	if m.copiedMsg != "" {
		parts = append(parts, m.copiedMsg)
	}

	return lipgloss.NewStyle().Faint(true).Render(strings.Join(parts, " · "))
}

// modelRegion renders the status line's model region. Colour encoding:
// green = default active, yellow = fallback active, red = no candidate
// healthy. A known reference without a reported state (handshake carried
// model_ref but no state, e.g. a status source not yet wired) still shows
// the reference, unstyled-faint like the rest of the line — the model is
// always visible once known, never dropped for lack of a state event.
func (m *Model) modelRegion() string {
	switch m.modelState {
	case protocol.ModelStateDefault:
		return modelStatusStyle(lipgloss.Color("2")).Render(m.modelRef)
	case protocol.ModelStateFallback:
		return modelStatusStyle(lipgloss.Color("3")).Render(m.modelRef)
	case protocol.ModelStateUnavailable:
		return modelStatusStyle(lipgloss.Color("1")).Render("LLM unavailable")
	default:
		if m.modelRef != "" {
			return m.modelRef // known ref, state not yet reported — still visible
		}

		return "" // nothing known at all — omit the region
	}
}

// modelStatusStyle builds the style for a model-region colour.
func modelStatusStyle(color color.Color) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(color)
}

// abbreviateHome is retired: the CWD left the footer for the /session
// command (tui-usage-observability).

// decodeResponse decodes a raw frame into a protocol.Response, returning an
// error wrapping protocol.ErrMalformedResponse on failure.
func decodeResponse(raw []byte, resp *protocol.Response) error {
	got, err := protocol.UnmarshalResponse(raw)
	if err != nil {
		return err
	}

	*resp = got

	return nil
}

// ExitCode returns the numeric exit code (convenience for the dispatcher).
func (m *Model) ExitCode() int { return m.exitReason.Code }

// String helps debugging; not used in production output.
func (m *Model) String() string {
	return fmt.Sprintf("tui.Model(state=%d exit=%d)", m.state, m.exitReason.Code)
}

// working reports whether a model turn is in flight (thinking or streaming):
// the working spinner is embedded in the input's top border.
func (m *Model) working() bool {
	return m.state == stateThinking || m.state == stateStreaming
}

// usageSegment renders the status line's usage segment (tui-usage-
// observability the design doc): compact Read / Cache% / Written tokens, the
// context-occupancy percentage when the daemon sent one, and the pair
// "user turns · LLM calls" in square brackets. LLMCalls counts every
// main-agent model call, one per ReAct iteration (thought → tool →
// observation → next iteration), including failover retries — the two
// figures diverge exactly when the agent used tools. The whole segment is
// hidden until the first usage data arrives; a sub-segment whose data is
// absent is omitted rather than rendered as zero (no fake 0% cache for
// Ollama).
func (m *Model) usageSegment() string {
	u := m.usage
	if u == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("R" + compactTokens(u.ReadTokens))
	if u.CachedTokens > 0 && u.ReadTokens > 0 {
		_, _ = fmt.Fprintf(&b, " C%.1f%%", 100*float64(u.CachedTokens)/float64(u.ReadTokens))
	}
	b.WriteString(" O" + compactTokens(u.WrittenTokens))

	if u.ContextOccupancyPct > 0 {
		pct := fmt.Sprintf("ctx %d%%", int(u.ContextOccupancyPct+0.5))
		if c := ctxOccupancyColor(u.ContextOccupancyPct); c != "" {
			b.WriteString(" · " + modelStatusStyle(lipgloss.Color(c)).Render(pct))
		} else {
			b.WriteString(" · " + pct)
		}
	}

	_, _ = fmt.Fprintf(&b, " [%d»%d]", u.TurnCount, u.LLMCalls)

	return b.String()
}

// roundTokens abbreviates like compactTokens without a trailing ".0":
// 142000 → "142k", 31500 → "31.5k", 940 → "940" (compaction line).
func roundTokens(n int) string {
	return strings.Replace(compactTokens(n), ".0k", "k", 1)
}

// compactTokens renders a token count compactly: 68000 → "68.0k", 5400 → "5.4k",
// 940 → "940".
func compactTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}

	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// inputBars renders the full-width horizontal bars above and below the input
// editor (task 6.1, reworked to Pi Agent mechanics). Idle: plain rules, with
// the centered `↑/↓ N more` indicator when lines are hidden past the
// textarea's internal scroll window. During a turn the top bar embeds the
// working spinner Pi-style (workingTopBorder).
func (m *Model) inputBars() (top, bottom string) {
	hiddenAbove, hiddenBelow := m.hiddenInputLines()

	switch {
	case m.working():
		top = m.workingTopBorder(hiddenAbove)
	case hiddenAbove > 0:
		top = scrollRule(m.width, "↑", hiddenAbove)
	default:
		top = strings.Repeat("─", max(m.width, 0))
	}

	if hiddenBelow > 0 {
		bottom = scrollRule(m.width, "↓", hiddenBelow)
	} else {
		bottom = strings.Repeat("─", max(m.width, 0))
	}

	return top, bottom
}

// workingTopBorder ports Pi Agent's CustomEditor.renderTopBorder verbatim
// (dist/modes/interactive/components/custom-editor.js): the spinner +
// "READY." label sit left-aligned after a `── ` prefix; the hidden-lines
// indicator is centered in the remaining space when both fit — otherwise the
// label degrades to the lone spinner frame and the indicator appears only if
// it then fits beside it (Pi drops it entirely when it does not); on very
// narrow terminals even the label is dropped in favour of a short dash
// prefix followed by the frame.
func (m *Model) workingTopBorder(hiddenAbove int) string {
	status := m.spinnerLabel(false)
	statusWidth := lipgloss.Width(status)

	overflow := ""
	if hiddenAbove > 0 {
		overflow = fmt.Sprintf(" ↑ %d more ", hiddenAbove)
	}
	overflowWidth := lipgloss.Width(overflow)
	overflowStart := (m.width - overflowWidth) / 2
	canFitOverflow := func() bool {
		return overflow != "" && overflowWidth+2 <= m.width &&
			overflowStart-(3+statusWidth+1) >= 1
	}

	if overflow != "" && !canFitOverflow() {
		status = spinnerFrame(m.spinnerIdx) // compact: give the indicator a chance
		statusWidth = lipgloss.Width(status)
	}

	if canFitOverflow() {
		leftBlockWidth := 3 + statusWidth + 1

		return "── " + status + " " +
			strings.Repeat("─", overflowStart-leftBlockWidth) +
			overflow +
			strings.Repeat("─", max(0, m.width-overflowStart-overflowWidth))
	}
	if m.width >= statusWidth+5 {
		return "── " + status + " " + strings.Repeat("─", max(0, m.width-statusWidth-4))
	}

	// Too narrow even for the full label — spinner only, short dash prefix.
	status = spinnerFrame(m.spinnerIdx)
	statusWidth = lipgloss.Width(status)
	prefix := min(3, max(0, m.width-statusWidth))

	return strings.Repeat("─", prefix) + status + strings.Repeat("─", max(0, m.width-prefix-statusWidth))
}

// scrollRule ports Pi Agent's createScrollBorder (pi-tui editor.js): the
// hidden-lines indicator is centered with one padding space on each side; on
// narrow terminals it degrades to a `─── ` prefix, then to a truncation
// ellipsis.
func scrollRule(width int, direction string, hidden int) string {
	label := fmt.Sprintf(" %s %d more ", direction, hidden)
	labelWidth := lipgloss.Width(label)
	if labelWidth+2 <= width {
		left := (width - labelWidth) / 2

		return strings.Repeat("─", left) + label + strings.Repeat("─", width-left-labelWidth)
	}

	indicator := fmt.Sprintf("─── %s %d more ", direction, hidden)
	if remaining := width - lipgloss.Width(indicator); remaining >= 0 {
		return indicator + strings.Repeat("─", remaining)
	}

	return truncateAnsi(indicator, max(0, width-3)) + "..."
}

// spinnerLabel renders the embedded working spinner: braille frame + "READY."
// (border colour), or the compact lone frame when mustCompact.
func (m *Model) spinnerLabel(mustCompact bool) string {
	frame := spinnerFrame(m.spinnerIdx)
	if mustCompact {
		return frame
	}

	return frame + " READY."
}

// hiddenInputLines computes the lines hidden above/below the textarea's
// internal scroll window. The bubbles textarea keeps the cursor visible in
// its own viewport (repositionView); ScrollYOffset is the top visible visual
// row. Counts are visual-row based (soft-wrapped long lines included via the
// hard-wrap approximation), matching what the bars claim.
func (m *Model) hiddenInputLines() (above, below int) {
	total := m.inputVisualLines()
	visible := m.computeInputHeight()
	if total <= visible {
		return 0, 0
	}

	offset := m.ta.ScrollYOffset()
	if offset < 0 {
		offset = 0
	}
	if offset > total-visible {
		offset = total - visible
	}

	below = total - offset - visible
	if below < 0 {
		below = 0
	}

	return offset, below
}

// inputVisualLines counts the display rows the textarea needs, approximating
// the textarea's own soft wrap: each logical line occupies
// ceil(displayWidth / textareaWidth) rows.
func (m *Model) inputVisualLines() int {
	w := m.ta.Width()
	if w <= 0 {
		w = 1
	}

	total := 0
	for line := range strings.SplitSeq(m.ta.Value(), "\n") {
		lw := lipgloss.Width(line)
		if lw <= 0 {
			total++

			continue
		}
		total += (lw + w - 1) / w
	}

	return total
}

// truncateAnsi is a lipgloss.Width-aware truncation fallback for labels wider
// than the bar (edge case: very narrow terminals).
func truncateAnsi(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}

	var out strings.Builder
	w := 0
	for _, c := range s {
		cw := lipgloss.Width(string(c))
		if w+cw > width {
			break
		}
		out.WriteRune(c)
		w += cw
	}

	return out.String()
}

// ctxOccupancyColor picks the occupancy tint: red at ≥90%, yellow at ≥70%,
// no tint below (tui-usage-observability task 5.3).
func ctxOccupancyColor(pct float64) string {
	switch {
	case pct >= 90:
		return "1"
	case pct >= 70:
		return "3"
	default:
		return ""
	}
}

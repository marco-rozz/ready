package tui

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/marco-rozz/ready/internal/mdrender"
	"github.com/marco-rozz/ready/internal/protocol"
)

// Turn steering in the TUI (protocol 7). RETURN always sends; the daemon
// decides whether a message steers the running Turn, runs next, or stops it.
// The client never guesses a placement: every block moves only on a daemon
// event (message_accepted, turn_started, steer_injected, the final reply).

// blockStage is where a user message currently stands.
type blockStage int

const (
	// stOpener is an ordinary Turn-opening message (no label).
	stOpener blockStage = iota
	// stSending: sent, not yet acknowledged by the daemon.
	stSending
	// stSteerPending: acknowledged as a steering candidate, not yet claimed.
	stSteerPending
	// stQueued: acknowledged as queued behind the running Turn or a command.
	stQueued
	// stHeld: held locally while the client cannot send (reloading).
	stHeld
	// stInjected: claimed into the Turn (steer_injected or pre-loaded).
	stInjected
	// stDeferred: opened the next Turn after the previous reply.
	stDeferred
	// stDropped: dropped by /stop.
	stDropped
	// stStopRequest: the /stop command itself.
	stStopRequest
)

// Label glyphs: single-cell and not East-Asian-ambiguous (the squares ■ and ◆
// are ambiguous and were rejected).
const (
	glyphPending  = "⋯"
	glyphInjected = "↳"
	glyphDeferred = "↷"
	glyphDropped  = "⊘"
)

// labelStopRequested is the label of the /stop echo.
const labelStopRequested = glyphDropped + " stop requested"

// echoBlock is one echoed user message.
type echoBlock struct {
	reqID string
	text  string
	stage blockStage
	// superseded is the number of proposed tool calls the preemption that
	// injected this steer superseded (first block of a batch only).
	superseded int
}

// label returns the steering label line of the block ("" = none).
func (b echoBlock) label() string {
	switch b.stage {
	case stSteerPending:
		return glyphPending + " steer pending"
	case stInjected:
		switch b.superseded {
		case 0:
			return glyphInjected + " steer"
		case 1:
			return glyphInjected + " steer · 1 action superseded"
		default:
			return fmt.Sprintf("%s steer · %d actions superseded", glyphInjected, b.superseded)
		}
	case stDeferred:
		return glyphDeferred + " arrived after the reply · new turn"
	case stQueued:
		return glyphPending + " queued"
	case stHeld:
		return glyphPending + " held"
	case stDropped:
		return glyphDropped + " not processed · stopped"
	case stStopRequest:
		return labelStopRequested
	default:
		return ""
	}
}

// quoteText turns the typed text into one markdown blockquote: every line
// prefixed with "> ", blank lines "> "-less ">" so the side bar is
// continuous. Redaction is applied later by the renderer.
func quoteText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")

	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ">"

			continue
		}

		lines[i] = "> " + l
	}

	return strings.Join(lines, "\n")
}

// renderEchoBlock renders one echoed user message: the optional label line
// (terminal styling, not markdown) above the quoted message, inside the user
// band. The echo carries no ❯❯ prefix — that stays the editor prompt.
func (m *Model) renderEchoBlock(b echoBlock) string {
	quote := trimBlankRows(m.ensureEchoRenderer().Render(quoteText(b.text)))

	if label := b.label(); label != "" {
		quote = infoLabelStyle().Render(label) + "\n" + quote
	}

	return userMessageBlock(quote, m.width, m.style == mdrender.StyleDark)
}

var sgrSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// trimBlankRows drops the visually blank rows glamour puts around a block
// (document and blockquote margins), keeping exactly the content rows: the
// user band then carries one padding row above and below, as before.
func trimBlankRows(rendered string) string {
	lines := strings.Split(rendered, "\n")

	blank := func(l string) bool { return strings.TrimSpace(sgrSeq.ReplaceAllString(l, "")) == "" }

	start, end := 0, len(lines)
	for start < end && blank(lines[start]) {
		start++
	}

	for end > start && blank(lines[end-1]) {
		end--
	}

	return strings.Join(lines[start:end], "\n")
}

// syncEcho mirrors the opener's raw text into curEcho (kept for the status and
// legacy call sites that only need "the live turn's first message").
func (m *Model) syncEcho() {
	if len(m.live) > 0 {
		m.curEcho = m.live[0].text
	} else {
		m.curEcho = ""
	}
}

// takeBlock removes and returns the block with the given request id from the
// live head or the tail.
func (m *Model) takeBlock(id string) (echoBlock, bool) {
	if id == "" {
		return echoBlock{}, false
	}

	for i, b := range m.live {
		if b.reqID == id {
			m.live = append(m.live[:i:i], m.live[i+1:]...)
			m.syncEcho()

			return b, true
		}
	}

	for i, b := range m.tail {
		if b.reqID == id {
			m.tail = append(m.tail[:i:i], m.tail[i+1:]...)

			return b, true
		}
	}

	return echoBlock{}, false
}

// findBlock returns a pointer to the block with the given id (live or tail).
func (m *Model) findBlock(id string) *echoBlock {
	for i := range m.live {
		if m.live[i].reqID == id {
			return &m.live[i]
		}
	}

	for i := range m.tail {
		if m.tail[i].reqID == id {
			return &m.tail[i]
		}
	}

	return nil
}

// holdApplies reports whether RETURN on chat text must be held locally: only
// while the client cannot send — never merely because a turn is in flight.
func (m *Model) holdApplies() bool {
	return m.reloading
}

// awaitingAnswer reports whether the client still waits for the answer of a
// message it sent, or holds prompts locally.
func (m *Model) awaitingAnswer() bool {
	return len(m.sentIDs) > 0 || len(m.hold) > 0
}

// dispatchChat sends one chat message: the daemon decides what it is. While no
// turn is in flight it is the provisional opener of the live turn (the
// transcript echoes it at once, the thinking state and reply timeout start);
// while a turn runs it appears as a `sending` block at the live tail and
// changes neither the turn state nor the timeout until acknowledged.
func (m *Model) dispatchChat(text string) tea.Cmd {
	id := uuid.New().String()
	b := echoBlock{reqID: id, text: text, stage: stSending}
	m.sentIDs[id] = true

	if !m.waiting {
		b.stage = stOpener
		m.live = []echoBlock{b}
		m.body = nil
		m.syncEcho()
		m.beginTurnState()
		m.activityRequestID = id
		m.activitySeq = 0
		m.activityTool = ""
		m.activityAdditional = 0

		return tea.Batch(m.sendCmdWithID(protocol.RequestTypeMessage, text, id), m.timeoutCmd(), m.tickCmd())
	}

	if strings.TrimSpace(text) == "/stop" {
		b.stage = stStopRequest
	}

	m.tail = append(m.tail, b)

	return m.sendCmdWithID(protocol.RequestTypeMessage, text, id)
}

// beginTurnState enters the thinking state for a Turn start: the turn
// sequence advances exactly once per Turn (own idle send, or a turn_started
// the client did not open) and the reply timeout is armed afresh.
func (m *Model) beginTurnState() {
	m.state = stateThinking
	m.buffer.Reset()
	m.livePreview = ""
	m.dirty = false
	m.waiting = true
	m.tickOn = true
	m.turnSeq++
	m.timeoutSeq++
	m.turnCounted = true
}

// drainHold sends the locally held prompts in FIFO order once sending is
// possible again; each then follows the normal mid-turn rules.
func (m *Model) drainHold() tea.Cmd {
	if len(m.hold) == 0 || m.holdApplies() {
		return nil
	}

	held := m.hold
	m.hold = nil

	cmds := make([]tea.Cmd, 0, len(held))
	for _, b := range held {
		cmds = append(cmds, m.dispatchChat(b.text))
	}

	m.applyLayout()
	m.rebuildViewport()

	return tea.Batch(cmds...)
}

// handleTurnEvent routes the turn-steering control events (protocol 7); ok is
// false for any other control message.
func (m *Model) handleTurnEvent(resp protocol.Response) (tea.Model, tea.Cmd, bool) {
	if resp.ResponseType != protocol.ResponseTypeControl || resp.Status != protocol.ResponseStatusOK || resp.ErrorCode != "" {
		return m, nil, false
	}

	switch resp.Content {
	case protocol.ControlMessageAccepted:
		model, cmd := m.handleMessageAccepted(resp)

		return model, cmd, true
	case protocol.ControlTurnStarted:
		model, cmd := m.handleTurnStarted(resp)

		return model, cmd, true
	case protocol.ControlSteerInjected:
		model, cmd := m.handleSteerInjected(resp)

		return model, cmd, true
	case protocol.ControlNotice:
		model, cmd := m.handleNotice(resp)

		return model, cmd, true
	default:
		return m, nil, false
	}
}

// handleMessageAccepted places the block of an acknowledged message according
// to the daemon's disposition.
func (m *Model) handleMessageAccepted(resp protocol.Response) (tea.Model, tea.Cmd) {
	if resp.Turn == nil {
		return m, m.recvCmd()
	}

	b := m.findBlock(resp.RequestID)
	if b != nil {
		switch resp.Turn.Disposition {
		case protocol.DispositionSteer:
			// A provisional opener the daemon turned into a steering candidate
			// (a Turn was already running) moves to the live tail.
			moved, _ := m.takeBlock(resp.RequestID)
			moved.stage = stSteerPending
			m.tail = append(m.tail, moved)
		case protocol.DispositionQueued:
			if b.stage == stSending {
				b.stage = stQueued
			}
		case protocol.DispositionStop:
			b.stage = stStopRequest
		}

		m.rebuildViewport()
	}

	// An acknowledgement concerning the in-flight turn is activity: re-arm.
	if m.waiting {
		m.timeoutSeq++

		return m, tea.Batch(m.recvCmd(), m.timeoutCmd())
	}

	return m, m.recvCmd()
}

// handleTurnStarted makes the announced Turn the live one: the opener (ours or
// not) and its pre-loaded candidates head the Turn, and a Turn the client did
// not open enters thinking.
func (m *Model) handleTurnStarted(resp protocol.Response) (tea.Model, tea.Cmd) {
	t := resp.Turn
	if t == nil {
		return m, m.recvCmd()
	}

	var cmds []tea.Cmd

	m.turnID = t.TurnID

	if !m.turnCounted {
		m.beginTurnState()
		cmds = append(cmds, m.timeoutCmd(), m.tickCmd())
	}

	m.activityRequestID = resp.RequestID
	m.activitySeq = 0
	m.activityTool = ""
	m.activityAdditional = 0

	// Blocks that were the provisional live head but do not open this Turn go
	// back to the tail (the daemon is authoritative).
	keep := map[string]bool{}
	for _, id := range t.RequestIDs {
		keep[id] = true
	}

	var stay []echoBlock

	for _, b := range m.live {
		if keep[b.reqID] {
			stay = append(stay, b)

			continue
		}

		b.stage = stSteerPending
		m.tail = append(m.tail, b)
	}

	m.live = stay

	var head []echoBlock

	for i, id := range t.RequestIDs {
		b, ok := m.takeBlock(id)
		if !ok {
			continue
		}

		switch {
		case i == 0 && t.Deferred:
			b.stage = stDeferred
		case i == 0:
			b.stage = stOpener
		default:
			b.stage = stInjected
		}

		// The opener heads the turn; pre-loaded candidates open its body.
		if len(head) == 0 {
			head = append(head, b)

			continue
		}

		m.body = append(m.body, bodyItem{kind: bodySteer, block: b})
	}

	m.live = head

	m.syncEcho()
	m.rebuildViewport()

	cmds = append(cmds, m.recvCmd())

	return m, tea.Batch(cmds...)
}

// handleSteerInjected moves the claimed candidates into the Turn head after
// the opener and earlier injected steers, labelled with the superseded count.
func (m *Model) handleSteerInjected(resp protocol.Response) (tea.Model, tea.Cmd) {
	t := resp.Turn
	if t == nil {
		return m, m.recvCmd()
	}

	superseded := 0
	if t.SupersededToolCalls != nil {
		superseded = *t.SupersededToolCalls
	}

	for i, id := range t.RequestIDs {
		b, ok := m.takeBlock(id)
		if !ok {
			continue
		}

		b.stage = stInjected
		if i == 0 {
			b.superseded = superseded
		}

		m.body = append(m.body, bodyItem{kind: bodySteer, block: b})
	}

	m.rebuildViewport()

	if m.waiting {
		m.timeoutSeq++

		return m, tea.Batch(m.recvCmd(), m.timeoutCmd())
	}

	return m, m.recvCmd()
}

// finishTurn archives the Turn answered by a final reply: the head shows every
// user message the reply consumed, in consumption order, before the answer;
// candidates a /stop dropped stay visible as not processed; the client stays
// busy while a candidate it sent is neither consumed nor dropped, until the
// deferred Turn's turn_started.
func (m *Model) finishTurn(resp protocol.Response, reply string) tea.Cmd {
	m.activityTool = ""
	m.activityRequestID = ""

	var consumed []string

	switch {
	case resp.Turn != nil && len(resp.Turn.RequestIDs) > 0:
		consumed = resp.Turn.RequestIDs
	case resp.RequestID != "":
		consumed = []string{resp.RequestID}
	case len(m.live) > 0:
		consumed = []string{m.live[0].reqID}
	}

	// The opener heads the turn; steers injected earlier already sit in the
	// body (in arrival order). A consumed message still at the tail or the
	// live head — never claimed by an event — joins the body last.
	var heads []echoBlock

	body := m.body

	for i, id := range consumed {
		delete(m.sentIDs, id)

		b, ok := m.takeBlock(id)
		if !ok {
			continue
		}

		if i == 0 && len(heads) == 0 {
			heads = append(heads, b)

			continue
		}

		if b.stage != stStopRequest && b.stage != stInjected {
			b.stage = stInjected
		}

		body = append(body, bodyItem{kind: bodySteer, block: b})
	}

	// The live head's remaining blocks (not consumed) were not part of this
	// reply; they go back to the tail rather than being lost.
	for _, b := range m.live {
		b.stage = stSteerPending
		m.tail = append(m.tail, b)
	}

	m.live = nil

	// Legacy fallback: a live turn recorded only as raw echo text.
	if len(heads) == 0 && m.curEcho != "" {
		heads = append(heads, echoBlock{text: m.curEcho})
	}

	m.completed = append(m.completed, completedTurn{heads: heads, reply: reply, body: body})
	m.body = nil

	for _, text := range m.standalone {
		m.completed = append(m.completed, completedTurn{notice: text})
	}

	m.standalone = nil

	if resp.Turn != nil {
		for _, id := range resp.Turn.DroppedRequestIDs {
			if b, ok := m.takeBlock(id); ok {
				b.stage = stDropped
				delete(m.sentIDs, id)
				m.completed = append(m.completed, completedTurn{heads: []echoBlock{b}})
			}
		}
	}

	m.curEcho = ""
	m.buffer.Reset()
	m.livePreview = ""
	m.turnID = ""
	m.turnCounted = false
	m.completedDirty = true

	// A candidate the daemon accepted as a steer that this reply neither
	// consumed nor dropped is deferred: it opens the next Turn immediately.
	for _, b := range m.tail {
		if b.stage == stSteerPending {
			m.state = stateThinking
			m.waiting = true
			m.tickOn = true
			m.turnSeq++
			m.timeoutSeq++
			m.turnCounted = true
			m.rebuildViewport()

			return tea.Batch(m.timeoutCmd(), m.tickCmd())
		}
	}

	m.state = stateIdle
	m.waiting = false
	m.tickOn = false
	m.rebuildViewport()

	return nil
}

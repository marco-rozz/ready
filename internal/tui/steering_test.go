package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

const foreignReq = "foreign"

// steeringTestModel is a Model with a recording dealer and the window
// bootstrapped, ready to drive turn flows.
func steeringTestModel(t *testing.T) (*Model, *fakeDealer) {
	t.Helper()

	d := &fakeDealer{}
	m := NewModel(d, Options{
		ConnectionID: testSessionID,
		ReplyTimeout: 0, // zero → NewModel clamps to 120s
		Sanitizer:    privacy.New(nil),
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	return m, d
}

// sendTurn types text into the editor and pushes RETURN through the real
// sendInput path, executing only the frame-sending command (the others are
// sleeping ticks/timeouts — never run those in a test).
func sendTurn(m *Model, text string) {
	m.ta.InsertString(text)
	tm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm, ok := tm.(*Model)
	if !ok {
		return
	}
	*m = *mm
	if cmd == nil {
		return
	}

	// An idle send is a Batch [send, timeout, tick]; a mid-turn send is the
	// bare send command (running it performs the send).
	if bm, ok := cmd().(tea.BatchMsg); ok && len(bm) > 0 {
		_ = bm[0]()
	}
}

func control(m *Model, resp protocol.Response) {
	resp.Status = protocol.ResponseStatusOK
	resp.ResponseType = protocol.ResponseTypeControl
	tm, _ := m.Update(controlAfter{Resp: resp})
	if mm, ok := tm.(*Model); ok {
		*m = *mm
	}
}

func finalReply(m *Model, resp protocol.Response) {
	resp.Status = protocol.ResponseStatusOK
	resp.ResponseType = protocol.ResponseTypeReply
	tm, _ := m.Update(replyFinal{Resp: resp})
	if mm, ok := tm.(*Model); ok {
		*m = *mm
	}
}

func lastSent(t *testing.T, d *fakeDealer) protocol.Request {
	t.Helper()

	d.mu.Lock()
	defer d.mu.Unlock()

	req, err := protocol.UnmarshalRequest(d.sends[len(d.sends)-1])
	if err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	return req
}

func sendCount(d *fakeDealer) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return len(d.sends)
}

var ansiStrip = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func plainContent(m *Model) string { return ansiStrip.ReplaceAllString(m.buildContent(), "") }

// RETURN while a turn runs always sends (14.1): no local queue, the live
// turn's echo is untouched, a `sending` block appears at the live tail, the
// turn state and the reply timeout do not change until acknowledged.
func TestSteer_ReturnWhileWorkingSendsImmediately(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)

	sendTurn(m, "prima")
	if !m.waiting || m.curEcho != "prima" || sendCount(d) != 1 {
		t.Fatalf("precondition: live turn missing (waiting=%v echo=%q sends=%d)", m.waiting, m.curEcho, sendCount(d))
	}

	turnSeq, armSeq, state := m.turnSeq, m.timeoutSeq, m.state

	sendTurn(m, "correzione")

	if sendCount(d) != 2 {
		t.Fatalf("a mid-turn message must be sent at once, sends=%d", sendCount(d))
	}

	req := lastSent(t, d)
	if req.Type != protocol.RequestTypeMessage || req.Content != "correzione" || req.RequestID == "" {
		t.Fatalf("request = %+v", req)
	}

	if m.curEcho != "prima" || len(m.live) != 1 {
		t.Fatalf("the live turn's echo was clobbered: %q live=%d", m.curEcho, len(m.live))
	}

	if len(m.tail) != 1 || m.tail[0].stage != stSending || m.tail[0].text != "correzione" {
		t.Fatalf("tail = %+v, want one sending block", m.tail)
	}

	if m.turnSeq != turnSeq || m.timeoutSeq != armSeq || m.state != state {
		t.Fatal("a mid-turn send must change neither the turn state nor the timeout")
	}
}

// message_accepted places the block per the daemon's disposition; the client
// never infers a placement the daemon did not report.
func TestSteer_AcceptedDispositionsPlaceBlocks(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	sendTurn(m, "prima")
	sendTurn(m, "steer me")
	steerID := lastSent(t, d).RequestID
	sendTurn(m, "queue me")
	queueID := lastSent(t, d).RequestID
	sendTurn(m, "/stop")
	stopID := lastSent(t, d).RequestID

	armBefore := m.timeoutSeq

	control(m, protocol.Response{RequestID: steerID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})
	control(m, protocol.Response{RequestID: queueID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionQueued}})
	control(m, protocol.Response{RequestID: stopID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionStop}})

	got := map[string]blockStage{}
	for _, b := range m.tail {
		got[b.reqID] = b.stage
	}

	if got[steerID] != stSteerPending || got[queueID] != stQueued || got[stopID] != stStopRequest {
		t.Fatalf("stages = %v", got)
	}

	if m.timeoutSeq != armBefore+3 {
		t.Fatalf("each acknowledgement of the in-flight turn must re-arm the timeout: %d → %d", armBefore, m.timeoutSeq)
	}

	view := plainContent(m)
	for _, want := range []string{"⋯ steer pending", "⋯ queued", labelStopRequested} {
		if !strings.Contains(view, want) {
			t.Errorf("transcript missing label %q:\n%s", want, view)
		}
	}
}

// steer_injected moves the block into the turn head after the opener, with
// the superseded count on the first block of the batch.
func TestSteer_InjectedMovesIntoTurnHeadWithSupersededCount(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	sendTurn(m, "accendi le luci")
	openerID := m.live[0].reqID

	sendTurn(m, "no, quelle di sopra")
	steerID := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: steerID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})

	two := 2
	control(m, protocol.Response{RequestID: openerID, Content: protocol.ControlSteerInjected, Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{steerID}, SupersededToolCalls: &two}})

	if len(m.tail) != 0 || len(m.live) != 1 || len(m.body) != 1 || m.body[0].kind != bodySteer || m.body[0].block.stage != stInjected || m.body[0].block.superseded != 2 {
		t.Fatalf("live=%+v body=%+v tail=%+v", m.live, m.body, m.tail)
	}

	if view := plainContent(m); !strings.Contains(view, "↳ steer · 2 actions superseded") {
		t.Fatalf("label missing:\n%s", view)
	}

	if label := (echoBlock{stage: stInjected, superseded: 1}).label(); label != "↳ steer · 1 action superseded" {
		t.Fatalf("singular label = %q", label)
	}

	if label := (echoBlock{stage: stInjected}).label(); label != "↳ steer" {
		t.Fatalf("zero-superseded label = %q", label)
	}
}

// The final reply archives every consumed message, in consumption order,
// before the answer, and clears the client's unanswered set.
func TestSteer_FinalReplyConsumesInOrder(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	sendTurn(m, "uno")
	openerID := m.live[0].reqID
	sendTurn(m, "due")
	twoID := lastSent(t, d).RequestID

	control(m, protocol.Response{RequestID: twoID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})
	zero := 0
	control(m, protocol.Response{RequestID: openerID, Content: protocol.ControlSteerInjected, Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{twoID}, SupersededToolCalls: &zero}})

	finalReply(m, protocol.Response{RequestID: openerID, Content: "risposta", Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID, twoID}}})

	if m.state != stateIdle || m.waiting || len(m.sentIDs) != 0 || len(m.live) != 0 || len(m.tail) != 0 {
		t.Fatalf("state=%v waiting=%v sent=%v live=%d tail=%d", m.state, m.waiting, m.sentIDs, len(m.live), len(m.tail))
	}

	if len(m.completed) != 1 || len(m.completed[0].heads) != 1 || m.completed[0].heads[0].text != "uno" ||
		len(m.completed[0].body) != 1 || m.completed[0].body[0].block.text != "due" {
		t.Fatalf("completed = %+v", m.completed)
	}

	view := plainContent(m)
	if strings.Index(view, "uno") > strings.Index(view, "due") || strings.Index(view, "due") > strings.Index(view, "risposta") {
		t.Fatalf("head must show consumed messages in order before the answer:\n%s", view)
	}
}

// A candidate the reply neither consumed nor dropped keeps the client busy;
// the deferred turn_started starts the next turn in thinking and advances the
// turn sequence exactly once.
func TestSteer_DeferredCandidateKeepsClientBusy(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	sendTurn(m, "uno")
	openerID := m.live[0].reqID
	sendTurn(m, "tardi")
	lateID := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: lateID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})

	finalReply(m, protocol.Response{RequestID: openerID, Content: "a1", Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID}}})

	if !m.waiting || m.state != stateThinking {
		t.Fatalf("a deferred candidate must keep the client busy: waiting=%v state=%v", m.waiting, m.state)
	}

	if len(m.tail) != 1 || m.tail[0].reqID != lateID {
		t.Fatalf("tail = %+v", m.tail)
	}

	seq := m.turnSeq

	control(m, protocol.Response{RequestID: lateID, Content: protocol.ControlTurnStarted, Turn: &protocol.TurnInfo{TurnID: "t2", RequestIDs: []string{lateID}, Deferred: true}})

	if m.turnSeq != seq || m.turnID != "t2" {
		t.Fatalf("turn sequence must advance once per turn (armed at the reply): %d → %d, turnID=%q", seq, m.turnSeq, m.turnID)
	}

	if len(m.live) != 1 || m.live[0].stage != stDeferred || len(m.tail) != 0 {
		t.Fatalf("live=%+v tail=%+v", m.live, m.tail)
	}

	if view := plainContent(m); !strings.Contains(view, "↷ arrived after the reply · new turn") {
		t.Fatalf("deferred label missing:\n%s", view)
	}

	finalReply(m, protocol.Response{RequestID: lateID, Content: "a2", Turn: &protocol.TurnInfo{TurnID: "t2", RequestIDs: []string{lateID}}})

	if m.state != stateIdle || m.waiting {
		t.Fatal("the deferred turn's reply must return the client to idle")
	}
}

// Dropped candidates (and the /stop request) are shown as such; the client
// returns to idle (nothing deferred).
func TestSteer_StoppedTurnDropsCandidatesVisibly(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	sendTurn(m, "uno")
	openerID := m.live[0].reqID
	sendTurn(m, "due")
	dropID := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: dropID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})
	sendTurn(m, "/stop")
	stopID := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: stopID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionStop}})

	finalReply(m, protocol.Response{
		RequestID: openerID, Content: "Stopped. Completed actions are kept; nothing else will run.",
		Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID, stopID}, DroppedRequestIDs: []string{dropID}},
	})

	if m.state != stateIdle || m.waiting || len(m.sentIDs) != 0 || len(m.tail) != 0 {
		t.Fatalf("state=%v waiting=%v sent=%v tail=%d", m.state, m.waiting, m.sentIDs, len(m.tail))
	}

	view := plainContent(m)
	for _, want := range []string{labelStopRequested, "⊘ not processed · stopped", "due"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
}

// A turn_started for a turn the client did not open enters thinking and
// advances the turn sequence; an own idle send already counted its turn.
func TestSteer_TurnStartedOfAForeignTurnEntersThinking(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)

	if m.waiting {
		t.Fatal("precondition: idle")
	}

	seq, arm := m.turnSeq, m.timeoutSeq
	control(m, protocol.Response{RequestID: foreignReq, Content: protocol.ControlTurnStarted, Turn: &protocol.TurnInfo{TurnID: "tx", RequestIDs: []string{foreignReq}}})

	if !m.waiting || m.state != stateThinking || m.turnSeq != seq+1 || m.timeoutSeq != arm+1 || m.activityRequestID != foreignReq {
		t.Fatalf("waiting=%v state=%v turnSeq=%d→%d timeoutSeq=%d→%d activity=%q", m.waiting, m.state, seq, m.turnSeq, arm, m.timeoutSeq, m.activityRequestID)
	}

	finalReply(m, protocol.Response{RequestID: foreignReq, Content: "ok", Turn: &protocol.TurnInfo{TurnID: "tx", RequestIDs: []string{foreignReq}}})

	if m.state != stateIdle || m.waiting {
		t.Fatal("the foreign turn's reply must return to idle")
	}
}

// An own idle send is not counted twice by its own turn_started.
func TestSteer_OwnIdleSendCountsOneTurn(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	sendTurn(m, "ciao")
	seq := m.turnSeq
	openerID := m.live[0].reqID

	control(m, protocol.Response{RequestID: openerID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionQueued}})
	control(m, protocol.Response{RequestID: openerID, Content: protocol.ControlTurnStarted, Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID}}})

	if m.turnSeq != seq {
		t.Fatalf("turn sequence advanced twice for one turn: %d → %d", seq, m.turnSeq)
	}

	if len(m.live) != 1 || m.live[0].stage != stOpener || m.live[0].label() != "" {
		t.Fatalf("live = %+v, want a plain opener without label", m.live)
	}
}

// A mid-turn message acknowledged as `steer` for what the client believed to
// be an idle send (a turn was already running) moves to the live tail.
func TestSteer_ProvisionalOpenerBecomesSteerWhenDaemonSaysSo(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	sendTurn(m, "ciao")
	id := m.live[0].reqID

	control(m, protocol.Response{RequestID: id, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "other"}})

	if len(m.live) != 0 || len(m.tail) != 1 || m.tail[0].stage != stSteerPending {
		t.Fatalf("live=%+v tail=%+v", m.live, m.tail)
	}
}

// Local prompt hold: held while reloading as `⋯ held`, sent FIFO afterwards.
func TestSteer_PromptHoldDuringReloadDrainsFIFO(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	m.reloading = true

	sendTurn(m, "uno")
	sendTurn(m, "due")

	if sendCount(d) != 0 || len(m.hold) != 2 || m.waiting {
		t.Fatalf("held prompts must not be sent: sends=%d hold=%d waiting=%v", sendCount(d), len(m.hold), m.waiting)
	}

	if view := plainContent(m); !strings.Contains(view, "⋯ held") {
		t.Fatalf("held label missing:\n%s", view)
	}

	m.reloading = false

	if cmd := m.drainHold(); cmd != nil {
		if bm, ok := cmd().(tea.BatchMsg); ok {
			for _, c := range bm {
				if c == nil {
					continue
				}
				// Run only frame-sending sub-commands (skip sleeping ticks).
				if msg := c(); msg != nil {
					if inner, ok := msg.(tea.BatchMsg); ok && len(inner) > 0 {
						_ = inner[0]()
					}
				}
			}
		}
	}

	if len(m.hold) != 0 || len(m.sentIDs) != 2 || !m.waiting {
		t.Fatalf("hold=%d sent=%d waiting=%v", len(m.hold), len(m.sentIDs), m.waiting)
	}
}

// ── Quoted echo (14.3) ──────────────────────────────────────────────────

func TestQuoteText(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"one line":                 "> one line",
		"a\nb":                     "> a\n> b",
		"a\n\nb":                   "> a\n>\n> b",
		"a\r\nb":                   "> a\n> b",
		"trailing\n":               "> trailing\n>",
		"```go\nunclosed":          "> ```go\n> unclosed",
		"> already quoted\nnormal": "> > already quoted\n> normal",
		"  \n":                     ">\n>",
	}

	for in, want := range cases {
		if got := quoteText(in); got != want {
			t.Errorf("quoteText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEchoRendersAsPreservedQuoteWithoutPrompt(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	out := ansiStrip.ReplaceAllString(m.renderEchoBlock(echoBlock{text: "accendi le luci\ne abbassa le tapparelle\n\nfine"}), "")

	if strings.Contains(out, "❯❯") {
		t.Fatalf("the echo must not carry the editor prompt:\n%s", out)
	}

	lines := nonEmptyTrimmed(out)
	var quoted []string

	for _, l := range lines {
		if strings.HasPrefix(l, "│") || strings.HasPrefix(l, "|") {
			quoted = append(quoted, l)
		}
	}

	joined := strings.Join(quoted, "\n")
	for _, want := range []string{"accendi le luci", "e abbassa le tapparelle", "fine"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("quote missing %q:\n%s", want, out)
		}
	}

	// Two typed lines stay two rendered lines (preserved newlines).
	if strings.Contains(joined, "accendi le luci e abbassa") {
		t.Fatalf("typed line breaks collapsed:\n%s", out)
	}
}

func TestUnclosedFenceStaysInsideTheQuote(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	echo := m.renderEchoBlock(echoBlock{text: "look:\n```go\nfmt.Println(1)"})
	reply := m.renderMarkdown("**normal reply**")

	if !strings.Contains(ansiStrip.ReplaceAllString(echo, ""), "fmt.Println(1)") {
		t.Fatal("fenced content missing from the echo")
	}

	// The following content renders as markdown (bold has SGR), not as code.
	if !strings.Contains(reply, "\x1b[") {
		t.Fatal("the reply after an unclosed fence must render normally")
	}
}

func nonEmptyTrimmed(s string) []string {
	var out []string

	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}

	return out
}

// ── Label glyphs (14.4) ─────────────────────────────────────────────────

func TestLabelGlyphsAreSingleCellEvenWithEastAsianWidth(t *testing.T) {
	t.Parallel()

	cond := runewidth.NewCondition()
	cond.EastAsianWidth = true

	for _, g := range []string{glyphPending, glyphInjected, glyphDeferred, glyphDropped, glyphNotice} {
		if w := cond.StringWidth(g); w != 1 {
			t.Errorf("glyph %q is %d cells wide with East-Asian-ambiguous width enabled, want 1", g, w)
		}
	}

	for _, rejected := range []string{"■", "◆"} {
		if cond.StringWidth(rejected) == 1 {
			t.Logf("note: %q is not ambiguous on this runewidth table", rejected)
		}
	}
}

func TestEveryStageLabelIsEnglishAndStable(t *testing.T) {
	t.Parallel()

	cases := map[blockStage]string{
		stOpener:       "",
		stSending:      "",
		stSteerPending: "⋯ steer pending",
		stQueued:       "⋯ queued",
		stHeld:         "⋯ held",
		stDeferred:     "↷ arrived after the reply · new turn",
		stDropped:      "⊘ not processed · stopped",
		stStopRequest:  labelStopRequested,
	}

	for stage, want := range cases {
		if got := (echoBlock{stage: stage}).label(); got != want {
			t.Errorf("stage %d label = %q, want %q", stage, got, want)
		}
	}
}

// TestRequestInFlightErrorIsInert: the daemon's structural rejection must
// never tear down the live turn (it now only answers exec/control overlaps).
func TestRequestInFlightErrorIsInert(t *testing.T) {
	t.Parallel()

	m, _ := steeringTestModel(t)
	sendTurn(m, "prima") // live turn
	if !m.waiting {
		t.Fatal("precondition: live turn missing")
	}

	tm, cmd := m.Update(replyFinal{Resp: protocol.Response{
		Status:       protocol.ResponseStatusError,
		ResponseType: protocol.ResponseTypeReply,
		ErrorCode:    "request_in_flight",
	}})
	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}
	*m = *mm

	if m.quitting || m.exitSet {
		t.Error("request_in_flight tore the session down")
	}
	if !m.waiting {
		t.Error("live turn abandoned on request_in_flight")
	}
	if cmd == nil {
		t.Fatal("session must keep listening (recv re-armed)")
	}
}

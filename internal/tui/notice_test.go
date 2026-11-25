package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

func notice(m *Model, turnID, text string) {
	control(m, protocol.Response{Content: protocol.ControlNotice, Notice: &protocol.NoticeInfo{Text: text}, Turn: &protocol.TurnInfo{TurnID: turnID}})
}

// liveTurn starts a turn and announces it with turn_started, returning the
// opener's request id.
func liveTurn(t *testing.T, m *Model, d *fakeDealer, text string) string {
	t.Helper()

	sendTurn(m, text)

	id := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: id, Content: protocol.ControlTurnStarted, Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{id}}})

	return id
}

func order(view string, needles ...string) bool {
	last := -1

	for _, n := range needles {
		i := strings.Index(view, n)
		if i < 0 || i < last {
			return false
		}

		last = i
	}

	return true
}

func TestNotice_AppearsMidTurnWithoutChangingState(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	liveTurn(t, m, d, "lavora")

	state, turnSeq, armSeq := m.state, m.turnSeq, m.timeoutSeq

	notice(m, "t1", "**Report 1** pronto")

	if m.state != state || !m.waiting || m.turnSeq != turnSeq {
		t.Fatalf("a notice must not change the turn state: state=%v waiting=%v", m.state, m.waiting)
	}

	if m.timeoutSeq == armSeq {
		t.Fatal("a notice of the live turn must re-arm the reply timeout")
	}

	if len(m.body) != 1 || m.body[0].kind != bodyNotice {
		t.Fatalf("body = %+v", m.body)
	}

	view := plainContent(m)
	if !strings.Contains(view, labelNotice) || !strings.Contains(view, "Report 1") || !order(view, "lavora", labelNotice) {
		t.Fatalf("notice block missing or misplaced:\n%s", view)
	}
}

func TestNotice_SteerBetweenNoticesKeepsOrderLiveAndArchived(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	openerID := liveTurn(t, m, d, "avvia")

	notice(m, "t1", "NOTICE-A")

	sendTurn(m, "STEER-R2")
	steerID := lastSent(t, d).RequestID
	control(m, protocol.Response{RequestID: steerID, Content: protocol.ControlMessageAccepted, Turn: &protocol.TurnInfo{Disposition: protocol.DispositionSteer, TurnID: "t1"}})
	control(m, protocol.Response{RequestID: openerID, Content: protocol.ControlSteerInjected, Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{steerID}}})

	notice(m, "t1", "NOTICE-B")

	if view := plainContent(m); !order(view, "avvia", "NOTICE-A", "STEER-R2", "NOTICE-B") {
		t.Fatalf("live order wrong:\n%s", view)
	}

	finalReply(m, protocol.Response{RequestID: openerID, Content: "RISPOSTA", Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID, steerID}}})

	if view := plainContent(m); !order(view, "avvia", "NOTICE-A", "STEER-R2", "NOTICE-B", "RISPOSTA") {
		t.Fatalf("archived order wrong:\n%s", view)
	}

	if len(m.body) != 0 || len(m.sentIDs) != 0 {
		t.Fatalf("live state not cleared: body=%v sent=%v", m.body, m.sentIDs)
	}
}

func TestNotice_HandbackLineKeepsItsPositionBetweenNotices(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	openerID := liveTurn(t, m, d, "avvia")

	notice(m, "t1", "NOTICE-A")
	control(m, protocol.Response{Content: protocol.ControlSubagentHandback, Handback: &protocol.HandbackNotice{Name: "brief", Status: "completed"}})
	notice(m, "t1", "NOTICE-B")

	finalReply(m, protocol.Response{RequestID: openerID, Content: "FINE", Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID}}})

	if view := plainContent(m); !order(view, "NOTICE-A", "── subagent brief · completed", "NOTICE-B", "FINE") {
		t.Fatalf("handback line must sit between the notices:\n%s", view)
	}
}

func TestNotice_StandaloneWhenIdleOrOtherTurn(t *testing.T) {
	t.Parallel()

	// Idle (e.g. after a reconnect during a running Turn): standalone block.
	m, _ := steeringTestModel(t)
	notice(m, "t-old", "STANDALONE-IDLE")

	if len(m.completed) != 1 || m.completed[0].notice != "STANDALONE-IDLE" || m.waiting {
		t.Fatalf("completed=%+v waiting=%v", m.completed, m.waiting)
	}

	if view := plainContent(m); !strings.Contains(view, "STANDALONE-IDLE") || !strings.Contains(view, labelNotice) {
		t.Fatalf("standalone notice not rendered:\n%s", view)
	}

	// Another turn is live: the notice stays at the tail, then is archived.
	m2, d := steeringTestModel(t)
	openerID := liveTurn(t, m2, d, "avvia")
	notice(m2, "t-other", "STANDALONE-TAIL")

	if len(m2.body) != 0 || len(m2.standalone) != 1 {
		t.Fatalf("body=%v standalone=%v", m2.body, m2.standalone)
	}

	finalReply(m2, protocol.Response{RequestID: openerID, Content: "FINE", Turn: &protocol.TurnInfo{TurnID: "t1", RequestIDs: []string{openerID}}})

	if len(m2.standalone) != 0 || len(m2.completed) != 2 || m2.completed[1].notice != "STANDALONE-TAIL" {
		t.Fatalf("standalone not archived after the turn: %+v", m2.completed)
	}
}

func TestNotice_RedactedAtRender(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	m.sanitizer = privacy.New(map[string]string{"/home/secret": "~"})
	m.invalidateRenderers()

	liveTurn(t, m, d, "avvia")
	notice(m, "t1", "file in /home/secret/x")

	view := plainContent(m)
	if strings.Contains(view, "/home/secret") || !strings.Contains(view, "~/x") {
		t.Fatalf("client-side redaction missing:\n%s", view)
	}
}

func TestNotice_ResizeRerendersAtNewWidth(t *testing.T) {
	t.Parallel()

	m, d := steeringTestModel(t)
	liveTurn(t, m, d, "avvia")
	notice(m, "t1", strings.Repeat("parola ", 30))

	tm, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	if mm, ok := tm.(*Model); ok {
		*m = *mm
	}

	for _, line := range strings.Split(plainContent(m), "\n") {
		if w := runewidth.StringWidth(line); w > 40 {
			t.Fatalf("line wider than the resized viewport (%d > 40): %q", w, line)
		}
	}
}

func TestNotice_GlyphIsDistinctFromSteeringGlyphs(t *testing.T) {
	t.Parallel()

	for _, g := range []string{glyphPending, glyphInjected, glyphDeferred, glyphDropped} {
		if g == glyphNotice {
			t.Fatalf("notice glyph %q must differ from the steering glyphs", g)
		}
	}
}

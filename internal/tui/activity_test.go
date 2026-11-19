package tui

import (
	"strings"
	"testing"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/marco-rozz/ready/internal/protocol"
)

const testUsingTool = "using_tool"

func TestActivityCorrelationAndStreamingEnd(t *testing.T) {
	m := newTestModel(t)
	m.waiting = true
	m.state = stateThinking
	m.activityRequestID = "current"
	count := 1
	resp := protocol.Response{ConnectionID: m.connectionID, RequestID: "old", Activity: &protocol.Activity{State: testUsingTool, Sequence: 1, ToolName: "wrong", AdditionalTools: &count}}
	before := m.timeoutSeq
	m.handleControl(resp)
	if m.activityTool != "" || m.timeoutSeq != before {
		t.Fatal("stale request affected turn")
	}
	resp.RequestID = "current"
	resp.Activity.ToolName = "query"
	m.handleControl(resp)
	if m.activityTool != "query" || m.activityAdditional != 1 || m.timeoutSeq != before+1 {
		t.Fatalf("activity not applied: %+v", m)
	}
	if !strings.Contains(m.spinnerLabel(), "using tool query (+1)") {
		t.Fatal(m.spinnerLabel())
	}
	m.handleControl(resp)
	if m.timeoutSeq != before+1 {
		t.Fatal("duplicate rearmed timeout")
	}
	m.buffer.WriteString("answer")
	resp.Activity = &protocol.Activity{State: "thinking", Sequence: 2}
	m.handleControl(resp)
	if m.state != stateStreaming || m.activityTool != "" {
		t.Fatal("tool end did not restore streaming")
	}
	m.completeTurn("answer")
	m.handleControl(resp)
	if m.activityTool != "" || m.state != stateIdle {
		t.Fatal("late activity changed completed turn")
	}
}

func TestActivityRearmsOnlyForCurrentTransition(t *testing.T) {
	m := newTestModel(t)
	m.ta.SetValue("test")
	m = update(t, m, keyMsg("enter"))
	oldArm := m.timeoutSeq
	count := 0
	m.handleControl(protocol.Response{ConnectionID: m.connectionID, RequestID: m.activityRequestID, Activity: &protocol.Activity{State: testUsingTool, Sequence: 1, ToolName: "long_tool", AdditionalTools: &count}})
	if m.timeoutSeq != oldArm+1 {
		t.Fatal("activity did not rearm")
	}
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: oldArm})
	if m.exitSet {
		t.Fatal("stale timer ended turn")
	}
	m = update(t, m, timeoutMsg{turnSeq: m.turnSeq, armSeq: m.timeoutSeq})
	if m.exitReason.Code != exitTimeout {
		t.Fatalf("silent long tool must time out: %+v", m.exitReason)
	}
}

func TestActivityLabelSanitizesControlsAndFits(t *testing.T) {
	m := newTestModel(t)
	m.waiting = true
	m.activityRequestID = "request"
	m.state = stateThinking
	n := 3
	m.handleControl(protocol.Response{ConnectionID: m.connectionID, RequestID: "request", Activity: &protocol.Activity{State: testUsingTool, Sequence: 1, ToolName: "bad\x1b[2J\n" + strings.Repeat("x", 200), AdditionalTools: &n}})
	if strings.ContainsAny(m.activityTool, "\x1b\n\r") {
		t.Fatalf("unsafe tool name: %q", m.activityTool)
	}
	for _, width := range []int{1, 4, 16, 24, 80} {
		m.width = width
		top := m.workingTopBorder(9)
		if lipgloss.Width(top) > width {
			t.Fatalf("width %d: %q", width, top)
		}
	}
}

func TestEmulatedActivityParallelContextAndOutage(t *testing.T) {
	m := newTestModel(t)
	m.ta.SetValue("execute the task")
	m = update(t, m, keyMsg("enter"))
	count := 1
	m.handleControl(protocol.Response{ConnectionID: m.connectionID, RequestID: m.activityRequestID, Activity: &protocol.Activity{State: testUsingTool, Sequence: 1, ToolName: "long_tool", AdditionalTools: &count}})
	if !strings.Contains(m.spinnerLabel(), "long_tool (+1)") {
		t.Fatal("parallel activity missing")
	}
	count = 0
	m.handleControl(protocol.Response{ConnectionID: m.connectionID, RequestID: m.activityRequestID, Activity: &protocol.Activity{State: testUsingTool, Sequence: 2, ToolName: "other_tool", AdditionalTools: &count}})
	m.handleControl(protocol.Response{ConnectionID: m.connectionID, RequestID: m.activityRequestID, Activity: &protocol.Activity{State: "thinking", Sequence: 3}})
	m.handleReply(protocol.Response{Status: protocol.ResponseStatusOK, RequestID: m.activityRequestID, Content: "answer"})
	if m.state != stateIdle || m.activityTool != "" {
		t.Fatal("reply did not clear activity")
	}
	m.ta.SetValue("/context")
	_, _ = m.sendInput()
	if len(m.pendingCtrl) != 1 {
		t.Fatal("context request not sent")
	}
	for id := range m.pendingCtrl {
		m.handleReply(protocol.Response{Status: protocol.ResponseStatusOK, RequestID: id, Content: "estimated context"})
	}
	m.handleControl(protocol.Response{Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, ModelState: protocol.ModelStateUnavailable})
	if m.state != stateIdle || !strings.Contains(m.modelRegion(), "LLM unavailable") {
		t.Fatal("outage state not retained")
	}
}

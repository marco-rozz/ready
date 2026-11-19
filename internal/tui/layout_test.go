// layout_test.go — the Subagent panel's layout-independence contract
// (openspec change subagent-panel-observability, tasks 1.2, D1): the panel
// takes its space ONLY from the transcript viewport's share; the input
// editor keeps exactly computeInputHeight() and the top bar, bottom bar and
// status line are always part of the rendered body, never clipped.
package tui

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/protocol"
)

// layoutModel builds a model with the given terminal size and a panel
// visible (one running Subagent with one pending handback).
func layoutModel(t *testing.T, w, h int) *Model {
	t.Helper()

	m := NewModel(nil, Options{})
	m.width, m.height = w, h
	m.subagents = &protocol.SubagentsInfo{
		Agents: []protocol.SubagentsAgentView{
			{SubagentID: tID1, Name: tNameDailyBrief, Type: pTypeGP, Status: tStatusRunning},
		},
		PendingHandbacks: 1,
	}
	m.applyLayout()

	return m
}

func TestLayoutPanelVisibleShrinksViewport(t *testing.T) {
	m := layoutModel(t, 80, 30)

	panelH := m.subagentsPanelHeight()
	inH := m.computeInputHeight()
	if panelH == 0 {
		t.Fatal("panel should be visible")
	}

	if got := m.vp.Height(); got != 30-inH-3-panelH {
		t.Fatalf("vp height = %d, want %d (30 - %d - 3 - %d)", got, 30-inH-3-panelH, inH, panelH)
	}

	// The input keeps exactly its computed height.
	if h := m.ta.Height(); h != inH {
		t.Fatalf("input height = %d, want computeInputHeight() = %d", h, inH)
	}
}

func TestLayoutPanelHiddenKeepsPreChangeArithmetic(t *testing.T) {
	m := NewModel(nil, Options{})
	m.width, m.height = 80, 30
	m.applyLayout()

	if got := m.vp.Height(); got != 30-m.computeInputHeight()-3 {
		t.Fatalf("vp height = %d, want %d", got, 30-m.computeInputHeight()-3)
	}
}

func TestLayoutTerminalTooShortFloorsViewportKeepsInput(t *testing.T) {
	// A 6-row terminal: max(5, 30% of 6) caps the editor at 5 lines; with
	// the panel visible the viewport floors at 1 while the input keeps its
	// computed height.
	m := layoutModel(t, 80, 6)

	if got := m.vp.Height(); got != 1 {
		t.Fatalf("vp height = %d, want the floor of 1", got)
	}

	if h := m.ta.Height(); h != m.computeInputHeight() {
		t.Fatalf("input height = %d, want computeInputHeight() = %d", h, m.computeInputHeight())
	}
}

// Regression (2026-11-18 screenshot): with the panel visible the rendered
// body fits the terminal and always carries the top bar, bottom bar and
// status line — the terminal never clips them off-screen.
func TestLayoutBarsAlwaysRenderedWithPanel(t *testing.T) {
	for _, h := range []int{30, 12, 6} {
		m := layoutModel(t, 80, h)

		_, bottomBar := m.inputBars()
		status := m.statusLine()
		if status == "" {
			t.Fatalf("status line empty at height %d", h)
		}

		view := m.View().Content
		if !strings.Contains(view, status) {
			t.Fatalf("status line clipped at height %d", h)
		}
		// The bottom bar carries the usage segment's separator glyphs; when
		// it is empty (no usage data) its placeholder frame is still there.
		if bottomBar != "" && !strings.Contains(view, bottomBar) {
			t.Fatalf("bottom bar clipped at height %d", h)
		}
		// The panel itself must render inside the body.
		if !strings.Contains(view, "Subagents") {
			t.Fatalf("panel missing from the rendered body at height %d", h)
		}
	}
}

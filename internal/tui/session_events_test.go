package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

func updateModel(t *testing.T, m *Model, msg tea.Msg) *Model {
	t.Helper()

	tm, _ := m.Update(msg)

	mm, ok := tm.(*Model)
	if !ok {
		t.Fatalf("Update returned %T", tm)
	}

	return mm
}

// session-lifecycle 8.1: session_changed clears the usage segment, replaces
// the Session ID in the info block, and renders `── new session ──`.
func TestSessionChangedEvent(t *testing.T) {
	m := newUsageTestModel(t, Options{
		ConnectionID: testSession,
		SessionID:    "0192old0-0000-7000-8000-000000000000",
		Sanitizer:    privacy.New(nil),
		Usage:        &protocol.UsageInfo{ReadTokens: 68000, TurnCount: 3, LLMCalls: 5},
	})

	if !strings.Contains(ansi.Strip(m.content), "0192old0") {
		t.Fatalf("the info block must show the handshake Session ID:\n%s", ansi.Strip(m.content))
	}

	state := m.state
	m = updateModel(t, m, controlAfter{Resp: protocol.Response{
		ConnectionID: testSession,
		SessionID:    "0192new0-0000-7000-8000-000000000000",
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlSessionChanged,
	}})

	if m.usage != nil {
		t.Error("the usage segment must be cleared until the next usage block")
	}

	if strings.Contains(m.statusLine(), "R68") {
		t.Errorf("status line still shows usage: %q", m.statusLine())
	}

	content := ansi.Strip(m.content)
	if !strings.Contains(content, "── new session ──") || !strings.Contains(content, "0192new0") || strings.Contains(content, "0192old0") {
		t.Errorf("transcript/info block after session_changed:\n%s", content)
	}

	if m.state != state {
		t.Error("session_changed must not change the turn state")
	}
}

// compacted renders `── context compacted · 142k → 31k tokens ──` and leaves
// the turn state unchanged.
func TestCompactedEvent(t *testing.T) {
	m := newUsageTestModel(t, Options{ConnectionID: testSession, Sanitizer: privacy.New(nil)})
	state := m.state

	m = updateModel(t, m, controlAfter{Resp: protocol.Response{
		ConnectionID: testSession,
		Status:       protocol.ResponseStatusOK,
		ResponseType: protocol.ResponseTypeControl,
		Content:      protocol.ControlCompacted,
		Compaction:   &protocol.CompactionInfo{TokensBefore: 142000, TokensAfter: 31000},
	}})

	if content := ansi.Strip(m.content); !strings.Contains(content, "── context compacted · 142k → 31k tokens ──") {
		t.Errorf("transcript:\n%s", content)
	}

	if m.state != state || len(m.completed) != 1 || m.completed[0].echo != "" {
		t.Error("the compaction line is informational, never a turn")
	}
}

// /new and /compact are daemon Session commands: forwarded byte-identically
// as ordinary messages, never intercepted by the TUI.
func TestSessionCommandsForwardedAsMessages(t *testing.T) {
	for _, text := range []string{"/new", "/compact keep the heating decisions"} {
		if _, reserved := reservedCommand(text); reserved {
			t.Errorf("%q must not be a TUI-reserved command", text)
		}
	}
}

func TestRoundTokens(t *testing.T) {
	for in, want := range map[int]string{142000: "142k", 31500: "31.5k", 940: "940"} {
		if got := roundTokens(in); got != want {
			t.Errorf("roundTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

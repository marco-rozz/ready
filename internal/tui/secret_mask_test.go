//nolint:goconst // literal fixtures are clearer inline than behind constants
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

const canaryValue = "never-draw-this-0123456789"

func TestSecretEditorNeverDrawsTheValueUnlessRevealed(t *testing.T) {
	m, _ := steeringTestModel(t)
	acceptSecrets(t, m, secretItems())
	m.ta.SetValue(canaryValue)

	view := m.View().Content
	if strings.Contains(view, canaryValue) {
		t.Fatal("the masked editor drew the credential")
	}
	if !strings.Contains(view, "•") || !strings.Contains(view, "26 chars") {
		t.Fatalf("the masked editor must show bullets and a count:\n%s", view)
	}
	m.handleSecretKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if !strings.Contains(m.View().Content, canaryValue) {
		t.Fatal("Ctrl+R must reveal the current field")
	}
	m.handleSecretKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	if strings.Contains(m.View().Content, canaryValue) {
		t.Fatal("Ctrl+R must hide it again")
	}
}

func TestSecretRevealDoesNotCarryToTheNextField(t *testing.T) {
	m, _ := steeringTestModel(t)
	acceptSecrets(t, m, secretItems())
	m.handleSecretKey(tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})
	m.ta.SetValue("first")
	_, _ = m.sendInput()
	m.handleReply(protocol.Response{
		RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK,
		SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted},
	})
	m.ta.SetValue(canaryValue)
	if strings.Contains(m.View().Content, canaryValue) {
		t.Fatal("reveal leaked into the next field")
	}
}

func TestSecretEditorWarnsAboutWhitespaceAndAsksToConfirmANewline(t *testing.T) {
	m, d := steeringTestModel(t)
	acceptSecrets(t, m, secretItems())
	m.ta.SetValue("  padded  ")
	if !strings.Contains(m.View().Content, "whitespace") {
		t.Fatal("leading/trailing whitespace must be flagged")
	}
	m.ta.SetValue("line1\nline2")
	if _, cmd := m.sendInput(); cmd != nil || m.secretInFlight || len(d.sends) != 0 {
		t.Fatal("a value with a newline must not be sent on the first RETURN")
	}
	if !strings.Contains(m.View().Content, "newline") {
		t.Fatal("the newline confirmation must be explained")
	}
	if _, cmd := m.sendInput(); cmd == nil || !m.secretInFlight {
		t.Fatal("the second RETURN must confirm and send")
	}
}

func TestSecretsCommandListsRotatesAndDeletesWithConfirmation(t *testing.T) {
	m, d := steeringTestModel(t)

	// `/secrets` lists everything (names/states/fields, never values).
	m.ta.SetValue("/secrets")
	_, _ = m.sendInput()
	if m.secretRequestAction != protocol.SecretEntryActionList {
		t.Fatal("/secrets must ask the daemon for the list")
	}
	m.handleReply(protocol.Response{
		RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK,
		SecretEntry: &protocol.SecretEntryResponse{
			Action: protocol.SecretEntryActionList, Outcome: protocol.SecretEntryOutcomeListed,
			Secrets: []protocol.PendingSecret{
				{Name: "ok_key", Fields: []string{"llm.providers.p.api_key"}, State: protocol.SecretStateSet},
				{Name: "orphan", State: protocol.SecretStateUnused},
				{Name: "/run/ext", Fields: []string{"channels.telegram.token"}, State: protocol.SecretStateExternal},
			},
			Credentials: []protocol.CredentialInfo{{Target: "mcp:ha", State: "stored"}},
		},
	})
	var table string
	for _, c := range m.completed {
		table += c.reply
	}
	for _, want := range []string{"ok_key", "orphan", "pending_external", "mcp:ha"} {
		if !strings.Contains(table, want) {
			t.Errorf("the table must show %q:\n%s", want, table)
		}
	}

	// Rotation of any name opens the editor on that name only.
	m.ta.SetValue("/secrets set ok_key")
	_, _ = m.sendInput()
	if m.secretRequestAction != protocol.SecretEntryActionBegin {
		t.Fatal("rotation must begin an operation")
	}
	m.handleReply(protocol.Response{
		RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK,
		SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionBegin, Outcome: protocol.SecretEntryOutcomeActive, OperationID: "op"},
	})
	if !m.secretMode || len(m.secretPass) != 1 || m.secretPass[0].Name != "ok_key" {
		t.Fatalf("rotation pass = %+v", m.secretPass)
	}
	m.leaveSecretEditor()
	_ = m.endSecretCmd()
	m.secretRequestID = ""

	// Deleting needs an explicit confirm word.
	before := len(d.sends)
	m.ta.SetValue("/secrets rm orphan")
	_, cmd := m.sendInput()
	if cmd != nil || len(d.sends) != before {
		t.Fatal("rm without `confirm` must not reach the daemon")
	}
	m.ta.SetValue("/secrets rm orphan confirm")
	_, _ = m.sendInput()
	if m.secretRequestAction != protocol.SecretEntryActionDelete {
		t.Fatal("rm … confirm must send a delete")
	}
}

func TestStatusLineAndInfoBlockNameTheAgent(t *testing.T) {
	d := &fakeDealer{}
	m := NewModel(d, Options{ConnectionID: testSessionID, AgentName: "kitchen", AgentID: "0190-aaaa", Sanitizer: privacy.New(nil)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if !strings.Contains(m.statusLine(), "kitchen") {
		t.Fatalf("the status line must name the agent: %q", m.statusLine())
	}
	if !strings.Contains(m.infoBlock, "kitchen (0190-aaaa)") {
		t.Fatalf("the info block must name the agent: %q", m.infoBlock)
	}
}

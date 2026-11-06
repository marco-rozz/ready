//nolint:goconst // literal protocol and terminal fixtures
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/protocol"
)

func secretItems() []protocol.PendingSecret {
	return []protocol.PendingSecret{
		{Name: "mcp_s_x_key", Fields: []string{"mcp_servers.s.headers.X.Key"}, State: protocol.SecretStatePending},
		{Name: "telegram_token", Fields: []string{"channels.telegram.token"}, State: protocol.SecretStatePending},
		{Name: "p_api_key", Fields: []string{"llm.providers.p.api_key"}, State: protocol.SecretStatePending},
	}
}

func acceptSecrets(t *testing.T, m *Model, items []protocol.PendingSecret) {
	t.Helper()
	_ = m.beginSecretCmd()
	if m.secretRequestAction != protocol.SecretEntryActionList {
		t.Fatal("begin must first list from disk")
	}
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionList, Outcome: protocol.SecretEntryOutcomeListed, Secrets: items}})
	if m.secretRequestAction != protocol.SecretEntryActionBegin {
		t.Fatal("list did not request begin")
	}
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionBegin, Outcome: protocol.SecretEntryOutcomeActive, OperationID: "op"}})
	if !m.secretMode {
		t.Fatal("accepted operation did not open editor")
	}
}

func TestSecretEditorIsolatesDraftsLiteralSlashValuesAndCorrelation(t *testing.T) {
	for _, value := range []string{"/new", "/session", "/secrets", "  literal ü : # value  "} {
		t.Run(value, func(t *testing.T) {
			m, d := steeringTestModel(t)
			m.ta.SetValue("saved chat draft")
			acceptSecrets(t, m, secretItems()[:1])
			if m.ta.Value() != "" {
				t.Fatal("chat draft entered secret editor")
			}
			m.ta.SetValue(value)
			_, cmd := m.sendInput()
			if cmd == nil {
				t.Fatal("literal value not submitted")
			}
			_ = cmd()
			if len(d.sends) != 1 {
				t.Fatal("not exactly one submission")
			}
			req, err := protocol.UnmarshalRequest(d.sends[0])
			if err != nil {
				t.Fatal(err)
			}
			if req.Type != protocol.RequestTypeSecretEntry || req.Content != "" || *req.SecretEntry.Value != value {
				t.Fatal("literal was interpreted as chat")
			}
			if _, again := m.sendInput(); again != nil {
				t.Fatal("duplicate submit sent")
			}
			m.handleReply(protocol.Response{RequestID: "unrelated", Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted}})
			if !m.secretInFlight {
				t.Fatal("unrelated reply completed submission")
			}
			m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted, Warnings: []string{"durability warning"}}})
			if m.secretMode || m.ta.Value() != "saved chat draft" || m.secretEditor.Value() != "" {
				t.Fatal("secret editor did not restore separate chat draft")
			}
			chatState := m.curEcho + strings.Join(m.pending, " ") + fmt.Sprint(m.completed) + m.content
			if strings.Contains(chatState, value) || strings.Contains(m.secretEditor.Value(), value) {
				t.Fatal("credential leaked into chat state")
			}
			if m.secretRequestAction != protocol.SecretEntryActionEnd {
				t.Fatal("completed pass did not release operation")
			}
		})
	}
}

func TestSecretEditorSkipFixedPassAndControlExit(t *testing.T) {
	m, _ := steeringTestModel(t)
	acceptSecrets(t, m, secretItems())
	m.ta.SetValue("skip-canary")
	m.handleSecretKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.secretMode || m.secretIndex != 1 || len(m.secretPending) != 3 || m.ta.Value() != "" {
		t.Fatal("skip lost pending metadata or leaked value")
	}
	if !strings.Contains(m.View().Content, "[2/3]") {
		t.Fatal("pass index is not fixed")
	}
	m.handleSecretKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.secretMode || m.quitting || m.secretRequestAction != protocol.SecretEntryActionEnd {
		t.Fatal("consecutive Escape did not exit/release")
	}
	for _, key := range []string{"ctrl+c", "ctrl+d"} {
		model, _ := steeringTestModel(t)
		acceptSecrets(t, model, secretItems())
		model.ta.SetValue("exit-canary")
		model.handleSecretKey(keyMsg(key))
		if model.secretMode || model.quitting || model.ta.Value() != "" || model.secretEditor.Value() != "" || model.secretRequestAction != protocol.SecretEntryActionEnd {
			t.Fatal("control key failed cleanup")
		}
	}
}

func TestSecretEditorRefusalGuidanceAndSteeringGate(t *testing.T) {
	for code, want := range map[string]string{protocol.ErrorCodeProfileRequired: "does not expose config-manager", protocol.ErrorCodeLoopbackRequired: "SSH", protocol.ErrorCodeNameInUse: "references it"} {
		m, _ := steeringTestModel(t)
		m.ta.SetValue("chat draft")
		_ = m.beginSecretCmd()
		m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusError, ErrorCode: code, Content: "owning-address must save/discard"})
		if m.quitting || m.secretMode || m.ta.Value() != "chat draft" || !strings.Contains(m.content, want) {
			t.Fatalf("bad refusal guidance for %s", code)
		}
	}
	m, _ := steeringTestModel(t)
	m.pending = []string{"queued chat"}
	if m.beginSecretCmd() != nil {
		t.Fatal("steering queue did not block entry")
	}
	m.pending = nil
	m.waiting = true
	if m.beginSecretCmd() != nil {
		t.Fatal("in-flight turn did not block entry")
	}
}

func TestSecretEditorSessionExpiryLostConnectionAndUnknownRefreshCleanup(t *testing.T) {
	for _, kind := range []string{"session change", "lost connection", "expired"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := steeringTestModel(t)
			acceptSecrets(t, m, secretItems())
			m.ta.SetValue("cleanup-secret-canary")
			switch kind {
			case "session change":
				m.handleControl(protocol.Response{Status: protocol.ResponseStatusOK, Content: protocol.ControlSessionChanged, SessionID: "new-session"})
			case "lost connection":
				m.quit(exitErr, "connection lost")
			case "expired":
				_, _ = m.sendInput()
				m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeSecretEntryExpired})
			}
			if m.secretMode || m.ta.Value() != "" || m.secretEditor.Value() != "" || strings.Contains(m.content, "cleanup-secret-canary") {
				t.Fatal("secret survived operation cleanup")
			}
		})
	}
}

func TestSecretEditorDefersSaveNotificationUntilTurnEnds(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.waiting = true
	m.handleControl(protocol.Response{Status: protocol.ResponseStatusOK, Content: "pending_secrets_changed"})
	if m.secretRequestID != "" || !m.secretRefreshNeeded {
		t.Fatal("notification was lost or opened during a turn")
	}
	m.handleReply(protocol.Response{Status: protocol.ResponseStatusOK, Content: "saved configuration"})
	if m.secretRequestAction != protocol.SecretEntryActionList || m.secretRefreshNeeded {
		t.Fatal("turn end failed to list newly pending credentials")
	}
}

func TestSecretEditorAcquisitionPreservesNewChatDraft(t *testing.T) {
	m, d := steeringTestModel(t)
	_ = m.beginSecretCmd()
	m.ta.SetValue("chat draft during acquisition")
	if _, cmd := m.sendInput(); cmd != nil || m.waiting || m.curEcho != "" || len(d.sends) != 0 || m.ta.Value() != "chat draft during acquisition" {
		t.Fatal("chat draft raced pending operation acquisition")
	}
}

func TestSecretEditorExternalResolutionReListsAndReleasesOldSource(t *testing.T) {
	m, _ := steeringTestModel(t)
	acceptSecrets(t, m, secretItems())
	m.ta.SetValue("abandoned-secret-canary")
	m.handleControl(protocol.Response{Status: protocol.ResponseStatusOK, Content: "pending_secrets_changed", PendingSecrets: []protocol.PendingSecret{}})
	if m.secretRequestAction != protocol.SecretEntryActionList || len(m.secretPending) != 3 {
		t.Fatal("event trusted instead of re-listing disk")
	}
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionList, Outcome: protocol.SecretEntryOutcomeListed, Secrets: []protocol.PendingSecret{{Name: "x", State: protocol.SecretStateSet}}}})
	if m.secretMode || m.secretRequestAction != protocol.SecretEntryActionEnd || len(m.secretPending) != 0 || strings.Contains(m.ta.Value(), "canary") {
		t.Fatal("external resolution left obsolete editor/operation")
	}
}

func TestSecretEditorOwnCommitNotificationPreservesPass(t *testing.T) {
	for _, beforeReply := range []bool{false, true} {
		t.Run(fmt.Sprint(beforeReply), func(t *testing.T) {
			m, _ := steeringTestModel(t)
			items := secretItems()
			acceptSecrets(t, m, items)
			m.ta.SetValue("first-value")
			_, _ = m.sendInput()
			notification := protocol.Response{Status: protocol.ResponseStatusOK, Content: "pending_secrets_changed"}
			if beforeReply {
				m.handleControl(notification)
			}
			m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted}})
			m.ta.SetValue("second-unsubmitted-value")
			if !beforeReply {
				m.handleControl(notification)
				m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: protocol.SecretEntryActionList, Outcome: protocol.SecretEntryOutcomeListed, Secrets: items[1:]}})
			}
			if !m.secretMode || m.secretIndex != 1 || m.ta.Value() != "second-unsubmitted-value" {
				t.Fatal("own notification interrupted the pass or cleared the next value")
			}
		})
	}
}

func TestSecretEditorUnsolicitedExpiryClearsValueAndPreservesChat(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.ta.SetValue("saved chat draft")
	acceptSecrets(t, m, secretItems())
	m.ta.SetValue("abandoned-secret-canary")
	m.handleControl(protocol.Response{Status: protocol.ResponseStatusError, ResponseType: protocol.ResponseTypeControl, ErrorCode: protocol.ErrorCodeSecretEntryExpired})
	if m.secretMode || m.secretOperation != "" || m.quitting || m.ta.Value() != "saved chat draft" || m.secretEditor.Value() != "" || len(m.secretPending) != 3 {
		t.Fatal("idle expiry failed to clear the editor independently of live chat")
	}
	if strings.Contains(m.content, "abandoned-secret-canary") || !strings.Contains(m.content, "/secrets") {
		t.Fatal("unsafe or missing expiry guidance")
	}
}

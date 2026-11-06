//nolint:goconst // literal protocol fixtures
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/protocol"
)

func listReply(m *Model, secrets []protocol.PendingSecret, creds []protocol.CredentialInfo) {
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionList, Outcome: protocol.SecretEntryOutcomeListed, Secrets: secrets, Credentials: creds,
	}})
}

func savedReply() protocol.Response {
	return protocol.Response{Status: protocol.ResponseStatusOK, Content: "saved", ConfigSaved: true}
}

func reloadOK(m *Model) {
	m.handleReply(protocol.Response{
		RequestID: m.reloadRequestID, Status: protocol.ResponseStatusOK,
		ModelRef: "ollama::gemma4:31b", ModelState: protocol.ModelStateDefault, AgentName: "casa-new", AgentID: "id-1",
	})
}

func loginCredFixture() []protocol.CredentialInfo {
	return []protocol.CredentialInfo{{Target: "llm:codex", State: "login_required"}}
}

func TestActivation_SaveWithNothingToCollectGoesStraightToReload(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.waiting = true
	m.curEcho = "save it"
	m.handleReply(savedReply())
	if m.secretRequestAction != protocol.SecretEntryActionList {
		t.Fatal("a saved Turn must first list the on-disk state")
	}
	listReply(m, nil, nil)
	if !m.reloading || m.state != stateReloading || m.reloadRequestID == "" {
		t.Fatal("zero pending secrets and logins must go straight to the runtime reload")
	}

	// Input typed during the reload is queued, never sent.
	m.ta.InsertString("hello")
	_, _ = m.sendInput()
	if len(m.pending) != 1 || m.waiting {
		t.Fatal("typed input must queue while reloading")
	}

	reloadOK(m)
	if m.reloading || m.state == stateReloading && !m.waiting {
		t.Fatal("reload completion must leave the reloading state")
	}
	if m.modelRef != "ollama::gemma4:31b" || m.agentName != "casa-new" {
		t.Fatalf("refreshed model/agent not applied: %q %q", m.modelRef, m.agentName)
	}
	if !strings.Contains(m.content, "configuration reloaded") {
		t.Fatal("the transcript must show the reload line")
	}
	if !m.waiting || len(m.pending) != 0 {
		t.Fatal("the steering queue must drain after the reload")
	}
}

func TestActivation_FullPhase1Phase2Phase3Sequence(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.waiting = true
	m.handleReply(savedReply())
	listReply(m, secretItems()[:1], loginCredFixture())
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionBegin, Outcome: protocol.SecretEntryOutcomeActive, OperationID: "op",
	}})
	if !m.secretMode {
		t.Fatal("Phase 1 must open the RED editor")
	}
	m.ta.SetValue("a-secret-value")
	_, _ = m.sendInput()
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted,
	}})
	// The pass is complete: the editor releases the operation (End).
	if m.secretRequestAction != protocol.SecretEntryActionEnd {
		t.Fatal("the completed pass must release the operation")
	}
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionEnd, Outcome: protocol.SecretEntryOutcomeReleased,
	}})

	// Phase 2: begin for llm:codex.
	if m.oauthRequestAction != protocol.OAuthLoginActionBegin || m.oauthPass[0] != "llm:codex" {
		t.Fatalf("Phase 2 did not start (action %q pass %v)", m.oauthRequestAction, m.oauthPass)
	}
	m.handleReply(protocol.Response{RequestID: m.oauthRequestID, Status: protocol.ResponseStatusOK, OAuthLogin: &protocol.OAuthLoginResponse{
		Action: protocol.OAuthLoginActionBegin, OperationID: "oauth-1", Target: "llm:codex", Outcome: protocol.OAuthLoginOutcomeWaiting,
		AuthURL: "https://auth.example/authorize", AcceptsManual: true,
	}})
	if !m.oauthMode || !strings.Contains(m.ta.Placeholder, "[1/1] OAuth login: llm:codex") {
		t.Fatalf("OAuth mode not entered: %q", m.ta.Placeholder)
	}
	if !strings.Contains(m.content, "https://auth.example/authorize") {
		t.Fatal("the authorization URL must be shown in the transcript")
	}

	// The browser callback completes on the daemon.
	m.handleControl(protocol.Response{
		Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, Content: protocol.ControlOAuthLoginResult,
		OAuthLogin: &protocol.OAuthLoginResponse{OperationID: "oauth-1", Target: "llm:codex", Outcome: protocol.OAuthLoginOutcomeCommitted},
	})
	if m.oauthMode {
		t.Fatal("the committed login must leave OAuth mode")
	}
	if !m.reloading {
		t.Fatal("Phase 3 must follow the last login")
	}
}

func TestActivation_ManualSubmitSendsPastedValue(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.credentials = loginCredFixture()
	m.actPhase = phaseOAuth
	_ = m.advanceActivation()
	m.handleReply(protocol.Response{RequestID: m.oauthRequestID, Status: protocol.ResponseStatusOK, OAuthLogin: &protocol.OAuthLoginResponse{
		Action: protocol.OAuthLoginActionBegin, OperationID: "oauth-2", Target: "llm:codex", Outcome: protocol.OAuthLoginOutcomeWaiting, AcceptsManual: true,
	}})
	m.ta.SetValue("http://localhost:1455/auth/callback?code=c&state=s")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.oauthRequestAction != protocol.OAuthLoginActionSubmit {
		t.Fatal("Enter with pasted text must submit")
	}
}

func TestActivation_EscInPhase2StillReloadsAfterSave(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.waiting = true
	m.handleReply(savedReply())
	listReply(m, nil, loginCredFixture())
	m.handleReply(protocol.Response{RequestID: m.oauthRequestID, Status: protocol.ResponseStatusOK, OAuthLogin: &protocol.OAuthLoginResponse{
		Action: protocol.OAuthLoginActionBegin, OperationID: "oauth-3", Target: "llm:codex", Outcome: protocol.OAuthLoginOutcomeWaiting,
	}})
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.oauthRequestAction != protocol.OAuthLoginActionCancel {
		t.Fatal("Esc must send oauth_login cancel")
	}
	m.handleReply(protocol.Response{RequestID: m.oauthRequestID, Status: protocol.ResponseStatusOK, OAuthLogin: &protocol.OAuthLoginResponse{
		Action: protocol.OAuthLoginActionCancel, OperationID: "oauth-3", Target: "llm:codex", Outcome: protocol.OAuthLoginOutcomeCancelled,
	}})
	if !m.reloading {
		t.Fatal("a skipped login must still reload after a committed save")
	}
}

func TestActivation_StartupWithSkippedItemsDoesNotReload(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.secretKnown = true
	m.secretPending = secretItems()[:1]
	_ = m.startActivation()
	listReply(m, secretItems()[:1], nil)
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionBegin, Outcome: protocol.SecretEntryOutcomeActive, OperationID: "op",
	}})
	m.handleSecretKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.handleSecretKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.secretRequestAction != protocol.SecretEntryActionEnd {
		t.Fatal("skipping must release the operation")
	}
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionEnd, Outcome: protocol.SecretEntryOutcomeReleased,
	}})
	if m.reloading || m.reloadRequestID != "" {
		t.Fatal("nothing was saved or committed: no reload")
	}
	if m.actPhase != phaseNone {
		t.Fatalf("the cycle must be over, phase=%v", m.actPhase)
	}
}

func TestActivation_OnDemandRotationTriggersReload(t *testing.T) {
	m, _ := steeringTestModel(t)
	all := []protocol.PendingSecret{{Name: "telegram_token", Fields: []string{"channels.telegram.token"}, State: protocol.SecretStateSet}}
	m.secretAll = all
	_ = m.secretsCommand([]string{"set", "telegram_token"})
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionBegin, Outcome: protocol.SecretEntryOutcomeActive, OperationID: "op",
	}})
	if !m.secretMode {
		t.Fatal("rotation did not open the editor")
	}
	m.ta.SetValue("new-value")
	_, _ = m.sendInput()
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionSubmit, Outcome: protocol.SecretEntryOutcomeCommitted,
	}})
	m.handleReply(protocol.Response{RequestID: m.secretRequestID, Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{
		Action: protocol.SecretEntryActionEnd, Outcome: protocol.SecretEntryOutcomeReleased,
	}})
	if !m.reloading {
		t.Fatal("a committed rotation must activate through a runtime reload")
	}
}

func TestActivation_ConfigReloadedEventUpdatesStatusAndTranscript(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.handleControl(protocol.Response{
		Status: protocol.ResponseStatusOK, ResponseType: protocol.ResponseTypeControl, Content: protocol.ControlConfigReloaded,
		ModelRef: "or::m", ModelState: protocol.ModelStateFallback, AgentName: "casa-2",
	})
	if m.modelState != protocol.ModelStateFallback || m.agentName != "casa-2" || !strings.Contains(m.content, "configuration reloaded") {
		t.Fatal("config_reloaded was not applied")
	}
}

func TestActivation_ReloadFailureReturnsToIdle(t *testing.T) {
	m, _ := steeringTestModel(t)
	m.reloadRequired = true
	m.actPhase = phaseReload
	_ = m.advanceActivation()
	m.handleReply(protocol.Response{RequestID: m.reloadRequestID, Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeWriteNotCommitted, Content: "Runtime reload failed; the previous configuration stays active"})
	if m.reloading || m.state != stateIdle || !strings.Contains(m.content, "runtime reload failed") {
		t.Fatal("a failed reload must surface and return to idle")
	}
}

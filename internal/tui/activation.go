package tui

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/go-zeromq/zmq4"
	"github.com/google/uuid"
	"github.com/marco-rozz/ready/internal/protocol"
)

// Post-save activation (change post-save-activation-and-runtime-reload): after
// a Turn whose final reply carries config_saved, after a handshake that still
// lists incomplete items, or after an on-demand Secret rotation, the TUI runs
// — without leaving the alternate screen — three phases in sequence:
//
//	1. direct secret entry (the existing RED input area) for pending `!secret`s;
//	2. interactive OAuth login for every `login_required` target, coordinated
//	   over ZMQ so this client never touches the agent directory;
//	3. an in-process Runtime reload, when a save happened or a Secret/OAuth
//	   Credential was committed during the cycle.

// activationPhase is the next phase a cycle will run.
type activationPhase int

const (
	phaseNone activationPhase = iota
	phaseSecrets
	phaseOAuth
	phaseReload
)

const (
	// credentialLoginRequired is the wire state of an OAuth target that needs
	// an interactive login.
	credentialLoginRequired = "login_required"
	oauthPlaceholderFmt     = "[%d/%d] OAuth login: %s (complete in browser, paste redirect URL/code + Enter, or Esc to skip)"
	oauthPlaceholderNoPaste = "[%d/%d] OAuth login: %s (complete in the browser, or Esc to skip)"
	reloadedLine            = "── configuration reloaded ──"
)

// openBrowser opens a URL in the system browser, best effort. A package
// variable so tests never launch a real browser.
//
//nolint:gochecknoglobals // test seam
var openBrowser = func(url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	_ = cmd.Run()
}

// loginRequiredTargets lists the OAuth targets needing an interactive login,
// in the daemon's deterministic order (llm:* first, then mcp:*).
func loginRequiredTargets(creds []protocol.CredentialInfo) []string {
	var out []string
	for _, c := range creds {
		if c.State == credentialLoginRequired {
			out = append(out, c.Target)
		}
	}

	return out
}

// activationBusy reports whether something else owns the editor or the wire
// right now; the cycle then resumes at its next join point.
func (m *Model) activationBusy() bool {
	return m.waiting || len(m.pending) > 0 || m.secretMode || m.oauthMode || m.reloading ||
		m.secretRequestID != "" || m.oauthRequestID != "" || m.quitting
}

// startActivation begins a cycle at Phase 1 unless one is already running.
func (m *Model) startActivation() tea.Cmd {
	if m.actPhase != phaseNone {
		return nil
	}
	m.actPhase = phaseSecrets

	return m.advanceActivation()
}

// startActivationNoRun marks a cycle as started without running it (the caller
// batches advanceActivation after its own bookkeeping).
func (m *Model) startActivationNoRun() {
	if m.actPhase == phaseNone {
		m.actPhase = phaseSecrets
	}
}

// advanceActivation moves the cycle to its next phase. It is called at every
// join point (cycle start, secret list/begin/end settled, OAuth step done,
// turn completed) and is a no-op while the editor or wire is busy.
func (m *Model) advanceActivation() tea.Cmd {
	if m.actPhase == phaseNone || m.activationBusy() {
		return nil
	}

	switch m.actPhase {
	case phaseSecrets:
		// Phase 1 is attempted once per cycle. Its first request is a list,
		// which also refreshes the Credential states Phase 2 needs.
		m.actPhase = phaseOAuth

		return m.beginSecretCmd()
	case phaseOAuth:
		m.actPhase = phaseReload
		if targets := loginRequiredTargets(m.credentials); len(targets) > 0 {
			m.oauthPass = targets
			m.oauthIndex = 0

			return m.beginOAuthCmd()
		}

		return m.advanceActivation()
	case phaseReload:
		m.actPhase = phaseNone
		if m.reloadRequired || m.onboardingCommitted {
			return m.startReloadCmd()
		}
	case phaseNone:
	}

	return nil
}

// ── Phase 2: interactive OAuth login ────────────────────────────────────

func (m *Model) beginOAuthCmd() tea.Cmd {
	if m.oauthIndex >= len(m.oauthPass) {
		m.oauthPass = nil

		return m.advanceActivation()
	}

	return m.sendOAuthRequest(&protocol.OAuthLoginRequest{
		Action: protocol.OAuthLoginActionBegin,
		Target: m.oauthPass[m.oauthIndex],
	})
}

func (m *Model) sendOAuthRequest(req *protocol.OAuthLoginRequest) tea.Cmd {
	requestID := uuid.New().String()
	m.oauthRequestID = requestID
	m.oauthRequestAction = req.Action
	payload, _ := protocol.Marshal(protocol.Request{
		ConnectionID: m.connectionID, Type: protocol.RequestTypeOAuthLogin, RequestID: requestID,
		Timestamp: time.Now().UTC().Format(time.RFC3339), OAuthLogin: req,
	})
	d := m.dealer

	return func() tea.Msg {
		if err := d.Send(zmq4.NewMsg(payload)); err != nil {
			return errMsg{err}
		}

		return sentMsg{}
	}
}

// oauthProgress renders the "[i/N]" counter of the current pass.
func (m *Model) oauthProgress() (int, int) { return m.oauthIndex + 1, len(m.oauthPass) }

// enterOAuthMode swaps the chat editor for the OAuth editor.
func (m *Model) enterOAuthMode(target string, acceptsManual bool) {
	if !m.oauthMode {
		m.chatEditor = m.ta
		editor := textarea.New()
		editor.SetStyles(textareaStyles(true))
		editor.Prompt = ""
		editor.ShowLineNumbers = false
		_ = editor.Focus()
		m.oauthEditor = editor
		m.ta = m.oauthEditor
	}
	i, n := m.oauthProgress()
	if acceptsManual {
		m.ta.Placeholder = fmt.Sprintf(oauthPlaceholderFmt, i, n, target)
	} else {
		m.ta.Placeholder = fmt.Sprintf(oauthPlaceholderNoPaste, i, n, target)
	}
	m.ta.Reset()
	m.oauthMode = true
	m.oauthTarget = target
	m.oauthAcceptsManual = acceptsManual
	m.applyLayout()
	m.rebuildViewport()
}

func (m *Model) leaveOAuthMode() {
	if m.oauthMode {
		m.ta.Reset()
		m.ta = m.chatEditor
	}
	m.oauthMode = false
	m.oauthOperation = ""
	m.oauthTarget = ""
	m.oauthAcceptsManual = false
	m.applyLayout()
	m.rebuildViewport()
}

// oauthNext moves to the next target of the pass, or on to Phase 3.
func (m *Model) oauthNext() tea.Cmd {
	m.oauthIndex++

	return m.beginOAuthCmd()
}

// oauthAbort ends the pass (an authorization refusal applies to every target).
func (m *Model) oauthAbort() tea.Cmd {
	m.oauthPass = nil
	m.oauthIndex = 0

	return m.advanceActivation()
}

func (m *Model) handleOAuthResponse(resp protocol.Response) (tea.Model, tea.Cmd) {
	action := m.oauthRequestAction
	m.oauthRequestID, m.oauthRequestAction = "", ""
	target := m.oauthTarget
	if target == "" && m.oauthIndex < len(m.oauthPass) {
		target = m.oauthPass[m.oauthIndex]
	}

	if resp.Status == protocol.ResponseStatusError {
		if action == protocol.OAuthLoginActionSubmit && resp.ErrorCode == protocol.ErrorCodeInvalidValue {
			m.appendInfoLine("OAuth callback rejected: " + safeOAuthMessage(resp) + " — paste it again or Esc to skip")

			return m, m.recvCmd()
		}
		m.leaveOAuthMode()
		m.appendInfoLine("OAuth login for " + target + " not completed: " + resp.ErrorCode + guidanceFor(resp.ErrorCode))
		if resp.ErrorCode == protocol.ErrorCodeProfileRequired || resp.ErrorCode == protocol.ErrorCodeLoopbackRequired {
			return m, tea.Batch(m.recvCmd(), m.oauthAbort())
		}

		return m, tea.Batch(m.recvCmd(), m.oauthNext())
	}
	if resp.OAuthLogin == nil {
		m.leaveOAuthMode()

		return m, tea.Batch(m.recvCmd(), m.oauthNext())
	}

	switch resp.OAuthLogin.Outcome {
	case protocol.OAuthLoginOutcomeWaiting:
		m.oauthOperation = resp.OAuthLogin.OperationID
		m.enterOAuthMode(target, resp.OAuthLogin.AcceptsManual)
		url := resp.OAuthLogin.AuthURL
		m.appendInfoLine("OAuth login for " + target + " — open this URL if the browser does not: " + url)

		return m, tea.Batch(m.recvCmd(), func() tea.Msg {
			openBrowser(url)

			return sentMsg{}
		})
	case protocol.OAuthLoginOutcomeCommitted:
		m.onboardingCommitted = true
		m.leaveOAuthMode()
		m.appendInfoLine("OAuth login committed: " + target)
	case protocol.OAuthLoginOutcomeNotRequired:
		m.appendInfoLine("OAuth login for " + target + ": no interactive login required")
	default: // cancelled, refused
		m.leaveOAuthMode()
	}

	return m, tea.Batch(m.recvCmd(), m.oauthNext())
}

// handleOAuthResult consumes the unsolicited oauth_login_result control event
// (browser callback completed on the daemon).
func (m *Model) handleOAuthResult(resp protocol.Response) (tea.Model, tea.Cmd) {
	if !m.oauthMode || resp.OAuthLogin == nil {
		return m, m.recvCmd()
	}
	if resp.OAuthLogin.OperationID != "" && resp.OAuthLogin.OperationID != m.oauthOperation {
		return m, m.recvCmd()
	}
	target := m.oauthTarget
	m.leaveOAuthMode()
	if resp.Status == protocol.ResponseStatusError || resp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeCommitted {
		m.appendInfoLine("OAuth login for " + target + " failed: " + safeOAuthMessage(resp))
	} else {
		m.onboardingCommitted = true
		m.appendInfoLine("OAuth login committed: " + target)
	}

	return m, tea.Batch(m.recvCmd(), m.oauthNext())
}

func safeOAuthMessage(resp protocol.Response) string {
	if resp.Content != "" {
		return resp.Content
	}

	return resp.ErrorCode
}

func guidanceFor(code string) string {
	switch code {
	case protocol.ErrorCodeProfileRequired:
		return " — the active profile does not expose config-manager"
	case protocol.ErrorCodeLoopbackRequired:
		return " — use a numeric loopback bind or SSH forwarding"
	default:
		return ""
	}
}

func (m *Model) handleOAuthKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.Key().Keystroke() {
	case keyControlC, keyControlD:
		return m.quit(exitSIGINT, "")
	case "esc", "escape":
		if m.oauthRequestID != "" || m.oauthOperation == "" {
			return m, nil
		}

		return m, m.sendOAuthRequest(&protocol.OAuthLoginRequest{
			Action: protocol.OAuthLoginActionCancel, OperationID: m.oauthOperation, Target: m.oauthTarget,
		})
	case keyEnter:
		value := strings.TrimSpace(m.ta.Value())
		if !m.oauthAcceptsManual || value == "" || m.oauthRequestID != "" || m.oauthOperation == "" {
			return m, nil
		}
		m.ta.Reset()

		return m, m.sendOAuthRequest(&protocol.OAuthLoginRequest{
			Action: protocol.OAuthLoginActionSubmit, OperationID: m.oauthOperation, Target: m.oauthTarget, Value: &value,
		})
	case "shift+enter", "alt+enter", "ctrl+j":
		return m, nil
	default:
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)

		return m, cmd
	}
}

// ── Phase 3: in-process Runtime reload ──────────────────────────────────

func (m *Model) startReloadCmd() tea.Cmd {
	m.reloadRequired, m.onboardingCommitted = false, false
	m.reloading = true
	m.state = stateReloading
	m.tickOn = true
	m.reloadRequestID = uuid.New().String()
	m.applyLayout()
	m.rebuildViewport()

	return tea.Batch(m.sendCmdWithID(protocol.RequestTypeRuntimeReload, "", m.reloadRequestID), m.tickCmd())
}

// applyReloaded installs the refreshed daemon state carried by a
// runtime_reload reply or a config_reloaded event.
func (m *Model) applyReloaded(resp protocol.Response) {
	if resp.ModelState != "" {
		m.modelRef, m.modelState = resp.ModelRef, resp.ModelState
	}
	if resp.AgentName != "" || resp.AgentID != "" {
		m.agentName = resp.AgentName
		m.agentLabel = agentLabel(resp.AgentName, resp.AgentID)
		m.infoBlock = buildInfoBlock(m.daemonAddress, m.agentLabel, m.agentDir, m.sessionID, m.clientVersion)
	}
	if resp.Credentials != nil {
		m.credentials = resp.Credentials
	}
	if resp.Secrets != nil {
		m.secretKnown = true
		m.secretAll = append([]protocol.PendingSecret(nil), resp.Secrets...)
		m.secretPending = pendingOnly(resp.Secrets)
	}
}

func (m *Model) handleReloadResponse(resp protocol.Response) (tea.Model, tea.Cmd) {
	m.reloadRequestID = ""
	m.reloading = false
	m.tickOn = false
	m.state = stateIdle

	if resp.Status == protocol.ResponseStatusError {
		m.appendInfoLine("runtime reload failed: " + safeOAuthMessage(resp))
	} else {
		m.applyReloaded(resp)
		m.appendInfoLine(reloadedLine)
	}
	m.applyLayout()
	m.rebuildViewport()

	return m, tea.Batch(m.recvCmd(), m.startNextQueuedTurn())
}

// handleConfigReloaded consumes the unsolicited config_reloaded event: the
// daemon reloaded because of a save on another channel.
func (m *Model) handleConfigReloaded(resp protocol.Response) (tea.Model, tea.Cmd) {
	m.applyReloaded(resp)
	m.appendInfoLine(reloadedLine)

	return m, m.recvCmd()
}

// resetActivation drops every in-flight activation step (a new Session or a
// lost operation invalidates them).
func (m *Model) resetActivation() {
	m.leaveOAuthMode()
	m.actPhase = phaseNone
	m.oauthPass, m.oauthIndex = nil, 0
	m.oauthRequestID, m.oauthRequestAction = "", ""
	m.reloadRequired, m.onboardingCommitted = false, false
}

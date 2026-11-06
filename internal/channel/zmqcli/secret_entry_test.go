//nolint:goconst // protocol fixture values
package zmqcli

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-zeromq/zmq4"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/rs/zerolog"
)

func secretRequest(connection, id string, action protocol.SecretEntryAction) protocol.Request {
	payload := &protocol.SecretEntryRequest{Action: action}
	switch action {
	case protocol.SecretEntryActionSubmit:
		value := "unique-secret-canary-value"
		payload.Value = &value
		payload.OperationID = "op"
		payload.Name = "n"
	case protocol.SecretEntryActionDelete:
		payload.Name = "n"
	case protocol.SecretEntryActionEnd:
		payload.OperationID = "op"
	}

	return protocol.Request{ConnectionID: connection, Type: protocol.RequestTypeSecretEntry, RequestID: id, Timestamp: time.Now().UTC().Format(time.RFC3339), SecretEntry: payload}
}

func secretReply(req protocol.Request) protocol.Response {
	outcome := protocol.SecretEntryOutcomeListed
	switch req.SecretEntry.Action {
	case protocol.SecretEntryActionBegin:
		outcome = protocol.SecretEntryOutcomeActive
	case protocol.SecretEntryActionSubmit:
		outcome = protocol.SecretEntryOutcomeCommitted
	case protocol.SecretEntryActionEnd:
		outcome = protocol.SecretEntryOutcomeReleased
	}

	return protocol.Response{Status: protocol.ResponseStatusOK, SecretEntry: &protocol.SecretEntryResponse{Action: req.SecretEntry.Action, OperationID: "op", Outcome: outcome}}
}

func dealerResponse(t *testing.T, dealer zmq4.Socket) protocol.Response {
	t.Helper()
	msg, err := dealer.Recv()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := protocol.UnmarshalResponse(msg.Frames[len(msg.Frames)-1])
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func TestSecretEntryOwnsConnectionButKeepsPingAndSessionEndResponsive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	buf := &lockedBuf{}
	ch, addr := newIntegrationChannel(ctx, t, "old-token", time.Minute, zerolog.New(buf).Level(zerolog.DebugLevel))
	ch.payloadsEnabled = true // configured before requests; all logs are captured
	started := make(chan struct{})
	settle := make(chan struct{})
	released := make(chan string, 1)
	ch.SetSecretEntryReleaseHandler(func(owner string) { released <- owner })
	ch.SetSecretEntryHandler(func(hctx context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		if req.SecretEntry.Action == protocol.SecretEntryActionSubmit {
			close(started)
			select {
			case <-settle:
			case <-hctx.Done():
			}
		}

		return secretReply(req)
	})
	dealer := dialStatusDealer(ctx, t, addr, "direct-owner")
	doHandshake(t, dealer, "direct-owner", "old-token")
	if resp := sendAndRecv(t, dealer, secretRequest("direct-owner", "begin", protocol.SecretEntryActionBegin)); resp.Status != protocol.ResponseStatusOK {
		t.Fatal(resp)
	}
	for _, kind := range []protocol.RequestType{protocol.RequestTypeMessage, protocol.RequestTypeExec} {
		resp := sendAndRecv(t, dealer, protocol.Request{ConnectionID: "direct-owner", Type: kind, RequestID: string(kind), Content: "/new", Timestamp: time.Now().UTC().Format(time.RFC3339)})
		if resp.ErrorCode != protocol.ErrorCodeSecretEntryActive {
			t.Fatal("owner conversational traffic dispatched")
		}
	}
	select {
	case <-ch.incoming:
		t.Fatal("secret/conversational input entered agent queue")
	default:
	}
	sendDealerRequest(t, dealer, secretRequest("direct-owner", "submit", protocol.SecretEntryActionSubmit))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("submit never started")
	}
	ping := protocol.Request{ConnectionID: "direct-owner", Type: protocol.RequestTypePing, RequestID: "ping", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if resp := sendAndRecv(t, dealer, ping); resp.RequestID != "ping" || resp.Status != protocol.ResponseStatusOK {
		t.Fatal("ping blocked behind commit")
	}
	if resp := sendAndRecv(t, dealer, secretRequest("direct-owner", "overlap", protocol.SecretEntryActionSubmit)); resp.ErrorCode != protocol.ErrorCodeRequestInFlight {
		t.Fatal("overlapping submission accepted")
	}
	end := protocol.Request{ConnectionID: "direct-owner", Type: protocol.RequestTypeSessionEnd, RequestID: "end", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if resp := sendAndRecv(t, dealer, end); resp.RequestID != "end" {
		t.Fatal("session_end blocked behind commit")
	}
	select {
	case <-released:
		t.Fatal("operation released before persistence settled")
	default:
	}
	close(settle)
	select {
	case owner := <-released:
		if owner != "direct-owner" {
			t.Fatal("wrong release owner")
		}
	case <-ctx.Done():
		t.Fatal("operation was not released")
	}
	if resp := dealerResponse(t, dealer); resp.RequestID != "submit" {
		t.Fatal("submit reply lost correlation")
	}
	if strings.Contains(buf.String(), "unique-secret-canary-value") {
		t.Fatal("DEBUG payload log leaked credential")
	}
}

func TestSecretEntryExpiryIgnoresLivePings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, time.Minute)
	released := make(chan string, 1)
	ch.SetSecretEntryReleaseHandler(func(owner string) { released <- owner })
	ch.SetSecretEntryHandler(func(_ context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		return secretReply(req)
	})
	dealer := dialStatusDealer(ctx, t, addr, "expiry-owner")
	doHandshake(t, dealer, "expiry-owner", "test-secret")
	_ = sendAndRecv(t, dealer, secretRequest("expiry-owner", "begin", protocol.SecretEntryActionBegin))
	// Await the end of the worker's response bookkeeping before advancing time.
	ch.secretWorkMu.Lock()
	ch.secretMu.Lock()
	ch.secretLastActivity = time.Now().Add(-2 * time.Minute)
	ch.secretMu.Unlock()
	ch.secretWorkMu.Unlock()
	ch.sess.touchPing()
	ch.checkIdleTimeout()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("pings renewed secret operation")
	}
	if resp := dealerResponse(t, dealer); resp.ResponseType != protocol.ResponseTypeControl || resp.ErrorCode != protocol.ErrorCodeSecretEntryExpired {
		t.Fatal("operation expiry did not notify the live TUI")
	}
	if !sessionIsActive(t, dealer, "expiry-owner") {
		t.Fatal("operation expiry ended live connection")
	}
}

func TestSecretEntryIdentityInflightAndLoopbackGates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, 0)
	called := make(chan struct{}, 1)
	ch.SetSecretEntryHandler(func(_ context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		called <- struct{}{}

		return secretReply(req)
	})
	owner := dialStatusDealer(ctx, t, addr, "owner")
	doHandshake(t, owner, "owner", "test-secret")
	foreign := dialStatusDealer(ctx, t, addr, "foreign")
	if resp := sendAndRecv(t, foreign, secretRequest("owner", "forged", protocol.SecretEntryActionBegin)); resp.ErrorCode != protocol.ErrorCodeNoActiveSession {
		t.Fatal("routing identity not checked")
	}
	ch.pendingMu.Lock()
	ch.pending["owner"] = pendingEntry{requestID: "turn"}
	ch.pendingMu.Unlock()
	if resp := sendAndRecv(t, owner, secretRequest("owner", "during-turn", protocol.SecretEntryActionBegin)); resp.ErrorCode != protocol.ErrorCodeRequestInFlight {
		t.Fatal("in-flight turn did not block begin")
	}
	select {
	case <-called:
		t.Fatal("refused request invoked handler")
	default:
	}
}

func TestSecretEntrySecretSwapKeepsOldConnectionAndRejectsOldToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, 0)
	old := dialStatusDealer(ctx, t, addr, "old-owner")
	doHandshake(t, old, "old-owner", "test-secret")
	ch.ReplaceAuthSecret("new-secret")
	if !sessionIsActive(t, old, "old-owner") {
		t.Fatal("rotation disconnected owner")
	}
	_ = sendAndRecv(t, old, protocol.Request{ConnectionID: "old-owner", Type: protocol.RequestTypeSessionEnd, Timestamp: time.Now().UTC().Format(time.RFC3339)})
	client := dialStatusDealer(ctx, t, addr, "new-owner")
	probe := protocol.Request{ConnectionID: "new-owner", Type: protocol.RequestTypeSessionStart, ProtocolVersion: protocol.Version, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	challenge := sendAndRecv(t, client, probe)
	probe.AuthToken = hmacHex("test-secret", challenge.Content)
	if resp := sendAndRecv(t, client, probe); resp.Status != protocol.ResponseStatusAuthFail {
		t.Fatal("old token still authenticates")
	}
	doHandshake(t, client, "new-owner", "new-secret")
}

func TestSecretEntryShutdownDoesNotWaitForSlowHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, 0)
	started := make(chan struct{})
	settled := make(chan struct{})
	ch.SetSecretEntryHandler(func(hctx context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		close(started)
		<-hctx.Done()
		close(settled)

		return secretReply(req)
	})
	dealer := dialStatusDealer(ctx, t, addr, "shutdown-owner")
	doHandshake(t, dealer, "shutdown-owner", "test-secret")
	sendDealerRequest(t, dealer, secretRequest("shutdown-owner", "list", protocol.SecretEntryActionList))
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("handler never started")
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel disk work")
	}
}

func TestSecretEntryWildcardListenerRefusesEntryButKeepsOrdinaryChat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := NewChannel(ctx, config.ZMQCLIChannelConfig{Bind: "tcp://0.0.0.0:0", ConnectionIdleTimeout: time.Minute}, "test-secret", false, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ch.Close() })
	_, port, err := net.SplitHostPort(strings.TrimPrefix(ch.listener.Addr(), "tcp://"))
	if err != nil {
		t.Fatal(err)
	}
	dealer := dialStatusDealer(ctx, t, "tcp://"+net.JoinHostPort("127.0.0.1", port), "wildcard-owner")
	doHandshake(t, dealer, "wildcard-owner", "test-secret")
	ch.SetSecretEntryHandler(func(_ context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		t.Error("ineligible listener invoked handler")

		return secretReply(req)
	})
	if resp := sendAndRecv(t, dealer, secretRequest("wildcard-owner", "begin", protocol.SecretEntryActionBegin)); resp.ErrorCode != protocol.ErrorCodeLoopbackRequired {
		t.Fatal("local peer bypassed actual wildcard binding")
	}
	startEchoAgent(ctx, ch)
	resp := sendAndRecv(t, dealer, protocol.Request{ConnectionID: "wildcard-owner", RequestID: "chat", Type: protocol.RequestTypeMessage, Content: "ordinary chat", Timestamp: time.Now().UTC().Format(time.RFC3339)})
	if resp.Status != protocol.ResponseStatusOK || resp.Content != "echo: ordinary chat" {
		t.Fatal("loopback restriction affected ordinary chat")
	}
}

func TestPendingDiscoveryDoesNotBlockReceiveAndCancelsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, time.Minute)
	started := make(chan struct{})
	stopped := make(chan struct{})
	ch.SetPendingSource(func(pctx context.Context) []protocol.PendingSecret {
		if _, ok := pctx.Deadline(); !ok {
			t.Error("pending discovery has no deadline")
		}
		close(started)
		<-pctx.Done()
		close(stopped)

		return nil
	})
	dealer := dialStatusDealer(ctx, t, addr, "pending-owner")
	probe := protocol.Request{ConnectionID: "pending-owner", Type: protocol.RequestTypeSessionStart, ProtocolVersion: protocol.Version, Timestamp: time.Now().UTC().Format(time.RFC3339)}
	challenge := sendAndRecv(t, dealer, probe)
	probe.AuthToken = hmacHex("test-secret", challenge.Content)
	sendDealerRequest(t, dealer, probe)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("discovery did not start")
	}
	ping := protocol.Request{ConnectionID: probe.ConnectionID, Type: protocol.RequestTypePing, RequestID: "ping-during-discovery", Timestamp: probe.Timestamp}
	if resp := sendAndRecv(t, dealer, ping); resp.RequestID != ping.RequestID || resp.Status != protocol.ResponseStatusOK {
		t.Fatal("pending I/O blocked receive")
	}
	end := protocol.Request{ConnectionID: probe.ConnectionID, Type: protocol.RequestTypeSessionEnd, RequestID: "end-during-discovery", Timestamp: probe.Timestamp}
	if resp := sendAndRecv(t, dealer, end); resp.RequestID != end.RequestID {
		t.Fatal("pending I/O blocked session_end")
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("shutdown did not cancel pending discovery")
	}
}

// A secret rotated outside the daemon must authenticate the FIRST handshake
// after the rotation: a challenge captures the secret it was issued under, so
// the refresh has to run before the nonce is issued.
func TestRotatedSecretAuthenticatesTheFirstHandshake(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, time.Minute)

	current := "test-secret"
	ch.SetSecretRefresh(func(context.Context) (string, bool) { return current, true })

	handshake := func(connection, secret string) protocol.Response {
		dealer := dialStatusDealer(ctx, t, addr, connection)
		probe := protocol.Request{ConnectionID: connection, Type: protocol.RequestTypeSessionStart, ProtocolVersion: protocol.Version, Timestamp: time.Now().UTC().Format(time.RFC3339)}
		challenge := sendAndRecv(t, dealer, probe)
		probe.AuthToken = hmacHex(secret, challenge.Content)

		return sendAndRecv(t, dealer, probe)
	}

	if resp := handshake("before-rotation", "test-secret"); resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("handshake with the current secret: %+v", resp)
	}
	ch.sess.releaseConnection("before-rotation")

	current = "rotated-secret-0123456789-0123456789"
	if resp := handshake("after-rotation", "rotated-secret-0123456789-0123456789"); resp.Status != protocol.ResponseStatusOK {
		t.Fatalf("the first handshake after a rotation must use the new secret: %+v", resp)
	}
	ch.sess.releaseConnection("after-rotation")
	if resp := handshake("stale-client", "test-secret"); resp.Status == protocol.ResponseStatusOK {
		t.Fatal("the old secret must no longer authenticate")
	}
}

func TestOAuthLoginAndRuntimeReloadOverZMQ(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ch, addr := newTestChannel(ctx, t, time.Minute)

	oauthReleased := make(chan string, 1)
	ch.SetOAuthLoginReleaseHandler(func(owner string) { oauthReleased <- owner })
	ch.SetSecretEntryHandler(func(_ context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		return secretReply(req)
	})
	ch.SetOAuthLoginHandler(func(_ context.Context, _ []byte, _ bool, req protocol.Request) protocol.Response {
		switch req.OAuthLogin.Action {
		case protocol.OAuthLoginActionBegin:
			return protocol.Response{
				Status: protocol.ResponseStatusOK,
				OAuthLogin: &protocol.OAuthLoginResponse{
					Action:        protocol.OAuthLoginActionBegin,
					OperationID:   "op-oauth-1",
					Target:        req.OAuthLogin.Target,
					Outcome:       protocol.OAuthLoginOutcomeWaiting,
					AuthURL:       "https://auth.example/authorize",
					AcceptsManual: true,
				},
			}
		case protocol.OAuthLoginActionCancel:
			return protocol.Response{
				Status: protocol.ResponseStatusOK,
				OAuthLogin: &protocol.OAuthLoginResponse{
					Action:      protocol.OAuthLoginActionCancel,
					OperationID: req.OAuthLogin.OperationID,
					Target:      req.OAuthLogin.Target,
					Outcome:     protocol.OAuthLoginOutcomeCancelled,
				},
			}
		default:
			return protocol.Response{Status: protocol.ResponseStatusError, ErrorCode: protocol.ErrorCodeMalformedRequest}
		}
	})
	ch.SetRuntimeReloadHandler(func(_ context.Context, _ []byte, _ bool, _ protocol.Request) protocol.Response {
		return protocol.Response{
			Status:     protocol.ResponseStatusOK,
			ModelRef:   "ollama::gemma4:31b",
			ModelState: protocol.ModelStateDefault,
			AgentID:    "aid",
			AgentName:  "casa-reloaded",
		}
	})

	dealer := dialStatusDealer(ctx, t, addr, "oauth-owner")
	doHandshake(t, dealer, "oauth-owner", "test-secret")

	// Start oauth_login.
	beginReq := protocol.Request{
		ConnectionID: "oauth-owner",
		Type:         protocol.RequestTypeOAuthLogin,
		RequestID:    "ob1",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		OAuthLogin:   &protocol.OAuthLoginRequest{Action: protocol.OAuthLoginActionBegin, Target: "llm:codex"},
	}
	beginResp := sendAndRecv(t, dealer, beginReq)
	if beginResp.Status != protocol.ResponseStatusOK || beginResp.OAuthLogin == nil || beginResp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeWaiting {
		t.Fatalf("unexpected oauth_login begin response: %+v", beginResp)
	}

	// While oauth_login is waiting, secret_entry begin and runtime_reload are refused with secret_entry_active.
	if resp := sendAndRecv(t, dealer, secretRequest("oauth-owner", "sb1", protocol.SecretEntryActionBegin)); resp.ErrorCode != protocol.ErrorCodeSecretEntryActive {
		t.Fatalf("secret_entry begin accepted during active oauth_login: %+v", resp)
	}
	reloadReq := protocol.Request{
		ConnectionID: "oauth-owner",
		Type:         protocol.RequestTypeRuntimeReload,
		RequestID:    "rr1",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}
	if resp := sendAndRecv(t, dealer, reloadReq); resp.ErrorCode != protocol.ErrorCodeSecretEntryActive {
		t.Fatalf("runtime_reload accepted during active oauth_login: %+v", resp)
	}

	// Completing via SendOAuthLoginResult releases ownership and delivers the control event.
	if err := ch.SendOAuthLoginResult("oauth-owner", protocol.Response{
		Status: protocol.ResponseStatusOK,
		OAuthLogin: &protocol.OAuthLoginResponse{
			OperationID: "op-oauth-1",
			Target:      "llm:codex",
			Outcome:     protocol.OAuthLoginOutcomeCommitted,
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctrlResp := dealerResponse(t, dealer)
	if ctrlResp.Content != protocol.ControlOAuthLoginResult || ctrlResp.OAuthLogin == nil || ctrlResp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeCommitted {
		t.Fatalf("unexpected control event: %+v", ctrlResp)
	}

	// Now runtime_reload succeeds.
	reloadReq.RequestID = "rr2"
	reloadResp := sendAndRecv(t, dealer, reloadReq)
	if reloadResp.Status != protocol.ResponseStatusOK || reloadResp.AgentName != "casa-reloaded" {
		t.Fatalf("unexpected runtime_reload response: %+v", reloadResp)
	}

	// Starting oauth_login and ending the session calls oauthRelease.
	beginReq.RequestID = "ob2"
	_ = sendAndRecv(t, dealer, beginReq)
	endReq := protocol.Request{
		ConnectionID: "oauth-owner",
		Type:         protocol.RequestTypeSessionEnd,
		RequestID:    "end1",
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
	}
	_ = sendAndRecv(t, dealer, endReq)
	select {
	case got := <-oauthReleased:
		if got != "oauth-owner" {
			t.Fatalf("oauthReleased owner = %q, want oauth-owner", got)
		}
	case <-ctx.Done():
		t.Fatal("oauthRelease not called on session_end")
	}
}

//nolint:goconst // literal protocol fixtures
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/channel/zmqcli"
	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/rs/zerolog"
)

func testCodexJWT(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	if err != nil {
		t.Fatal(err)
	}

	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newTestCodexTokenServer(t *testing.T, accountID string) *httptest.Server {
	t.Helper()
	jwt := testCodexJWT(t, accountID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  jwt,
			"refresh_token": "rt-test-123",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(srv.Close)

	return srv
}

type oauthFixture struct {
	cfg      *config.Config
	store    *secretstore.Store
	svc      *oauthLoginService
	handler  zmqcli.OAuthLoginHandler
	results  chan protocol.Response
	changed  chan struct{}
	loopback *string // address of the last loopback listener the service bound
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	ctx := t.Context()
	mcpNoAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(mcpNoAuth.Close)

	doc := `config_version: 2
llm:
  default: codex::gpt-5
  providers:
    codex:
      provider: openai-codex
      api: responses
channels:
  zmqcli:
    enabled: true
    auth_token: !secret zmqcli_auth_token
mcp_servers:
  open_srv:
    url: ` + mcpNoAuth.URL + `
    oauth:
      grant: auto
    profiles:
      default:
        allow: ["*"]
`
	path := filepath.Join(t.TempDir(), config.ConfigFileName)
	if err := os.WriteFile(path, []byte(withTestAgent(doc)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretValues = config.NewSecretSource(store)
	cfg.LLM.Secrets = cfg.SecretValues

	tokenSrv := newTestCodexTokenServer(t, "acct-browser-999")
	f := &oauthFixture{cfg: cfg, store: store, results: make(chan protocol.Response, 4), changed: make(chan struct{}, 4), loopback: new(string)}
	f.svc = newOAuthLoginService(
		ctx,
		func(cctx context.Context) *config.Config { return diskConfig(cctx, cfg) },
		store,
		func(_ string, resp protocol.Response) { f.results <- resp },
		func(context.Context) { f.changed <- struct{}{} },
	)
	f.svc.newCodexClient = func() *codexauth.Client {
		c := codexauth.NewClient(tokenSrv.Client())
		c.Endpoints.Token = tokenSrv.URL

		return c
	}
	f.svc.startCodexLoopback = func(fctx context.Context, expectedState string) (*codexauth.LoopbackServer, error) {
		srv, berr := codexauth.StartLoopbackServerOn(fctx, "127.0.0.1:0", expectedState)
		if berr == nil {
			*f.loopback = srv.Addr()
		}

		return srv, berr
	}
	f.handler = buildOAuthLoginHandler(f.svc, func() bool { return true })

	return f
}

func oauthReq(action protocol.OAuthLoginAction, op, target string, value *string) protocol.Request {
	return protocol.Request{
		ConnectionID: "conn-1", Type: protocol.RequestTypeOAuthLogin, RequestID: "r",
		OAuthLogin: &protocol.OAuthLoginRequest{Action: action, OperationID: op, Target: target, Value: value},
	}
}

func (f *oauthFixture) begin(t *testing.T, target string) protocol.Response {
	t.Helper()

	return f.handler(t.Context(), nil, true, oauthReq(protocol.OAuthLoginActionBegin, "", target, nil))
}

func TestOAuthLoginHandler_GatesProfileAndLoopback(t *testing.T) {
	f := newOAuthFixture(t)
	denied := buildOAuthLoginHandler(f.svc, func() bool { return false })
	if resp := denied(t.Context(), nil, true, oauthReq(protocol.OAuthLoginActionBegin, "", "llm:codex", nil)); resp.ErrorCode != protocol.ErrorCodeProfileRequired {
		t.Fatalf("expected profile_required, got %+v", resp)
	}
	if resp := f.handler(t.Context(), nil, false, oauthReq(protocol.OAuthLoginActionBegin, "", "llm:codex", nil)); resp.ErrorCode != protocol.ErrorCodeLoopbackRequired {
		t.Fatalf("expected loopback_required, got %+v", resp)
	}
}

func TestOAuthLoginHandler_BrowserCallbackPushesResult(t *testing.T) {
	f := newOAuthFixture(t)
	ctx := t.Context()
	begin := f.begin(t, "llm:codex")
	if begin.Status != protocol.ResponseStatusOK || begin.OAuthLogin == nil ||
		begin.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeWaiting ||
		begin.OAuthLogin.AuthURL == "" || !begin.OAuthLogin.AcceptsManual {
		t.Fatalf("unexpected begin response: %+v", begin)
	}
	parsed, err := url.Parse(begin.OAuthLogin.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("missing state in auth_url")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+*f.loopback+"/auth/callback?code=browser-code-1&state="+url.QueryEscape(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("callback GET failed: %v", err)
	}
	_ = cb.Body.Close()

	select {
	case res := <-f.results:
		if res.Status != protocol.ResponseStatusOK || res.Content != protocol.ControlOAuthLoginResult ||
			res.OAuthLogin == nil || res.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeCommitted {
			t.Fatalf("unexpected oauth_login_result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for oauth_login_result")
	}
	select {
	case <-f.changed:
	case <-time.After(time.Second):
		t.Fatal("expected pending_secrets_changed notification")
	}
	creds := deriveOAuthCredentials(ctx, f.cfg, f.store)
	if len(creds) == 0 || creds[0].Target != "llm:codex" || creds[0].State != "usable" {
		t.Fatalf("credential not usable after browser callback: %+v", creds)
	}
}

func TestOAuthLoginHandler_ManualSubmit(t *testing.T) {
	f := newOAuthFixture(t)
	begin := f.begin(t, "llm:codex")
	if begin.OAuthLogin == nil {
		t.Fatalf("begin failed: %+v", begin)
	}
	op := begin.OAuthLogin.OperationID
	parsed, _ := url.Parse(begin.OAuthLogin.AuthURL)
	state := parsed.Query().Get("state")

	bad := "http://localhost:1455/auth/callback?code=abc&state=wrong-state"
	if resp := f.handler(t.Context(), nil, true, oauthReq(protocol.OAuthLoginActionSubmit, op, "llm:codex", &bad)); resp.ErrorCode != protocol.ErrorCodeInvalidValue {
		t.Fatalf("expected invalid_value for a bad state paste, got %+v", resp)
	}

	good := "http://localhost:1455/auth/callback?code=manual-code-2&state=" + state
	resp := f.handler(t.Context(), nil, true, oauthReq(protocol.OAuthLoginActionSubmit, op, "llm:codex", &good))
	if resp.Status != protocol.ResponseStatusOK || resp.OAuthLogin == nil || resp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeCommitted {
		t.Fatalf("manual submit failed: %+v", resp)
	}
}

func TestOAuthLoginHandler_CancelReleasesThePortImmediately(t *testing.T) {
	f := newOAuthFixture(t)
	begin := f.begin(t, "llm:codex")
	if begin.OAuthLogin == nil {
		t.Fatalf("begin failed: %+v", begin)
	}
	bound := *f.loopback
	resp := f.handler(t.Context(), nil, true, oauthReq(protocol.OAuthLoginActionCancel, begin.OAuthLogin.OperationID, "llm:codex", nil))
	if resp.Status != protocol.ResponseStatusOK || resp.OAuthLogin == nil || resp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeCancelled {
		t.Fatalf("cancel failed: %+v", resp)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", bound)
	if err != nil {
		t.Fatalf("callback port %s still occupied after cancel: %v", bound, err)
	}
	_ = ln.Close()
}

func TestOAuthLoginHandler_MCPNotRequired(t *testing.T) {
	f := newOAuthFixture(t)
	resp := f.begin(t, "mcp:open_srv")
	if resp.Status != protocol.ResponseStatusOK || resp.OAuthLogin == nil || resp.OAuthLogin.Outcome != protocol.OAuthLoginOutcomeNotRequired {
		t.Fatalf("expected not_required for an open MCP server, got %+v", resp)
	}
}

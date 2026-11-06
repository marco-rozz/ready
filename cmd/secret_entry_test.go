//nolint:goconst // literal fixtures are clearer inline than behind constants
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/codexauth"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/oauth"
	"github.com/marco-rozz/ready/internal/protocol"
	"github.com/marco-rozz/ready/internal/tools"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v3"
)

func TestSecretEntryHandlerAuthorizationAndZMQAdoption(t *testing.T) {
	doc := "config_version: 2\nllm:\n  default: ollama::m\n  providers:\n    ollama:\n      provider: ollama\n      api: ollama\nchannels:\n  zmqcli:\n    enabled: true\n    auth_token: !secret zmqcli_auth_token\n"
	path := filepath.Join(t.TempDir(), config.ConfigFileName)
	if err := os.WriteFile(path, []byte(withTestAgent(doc)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}
	state := &bootSecretState{Store: store}
	adoptions := 0
	var adopted string
	entry := newSecretEntry(cfg, state, func(value string) { adoptions++; adopted = value }, nil)

	if isConfigManagerAllowed(cfg, "default") {
		t.Fatal("default profile must not allow config-manager")
	}

	request := protocol.Request{ConnectionID: "authenticated-owner", SecretEntry: &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionBegin}}
	defaultHandler := buildSecretEntryHandler(entry, isConfigManagerAllowed(cfg, "default"))
	if resp := defaultHandler(context.Background(), nil, true, request); resp.ErrorCode != protocol.ErrorCodeProfileRequired {
		t.Fatal("default profile accepted entry")
	}
	cfg.BuiltinTools.Profiles["custom-ops"] = config.OwnerProfileConfig{Allow: []string{config.ConfigManagerToolKey}}
	if !isConfigManagerAllowed(cfg, "custom-ops") {
		t.Fatal("custom profile allowing config-manager must be authorized")
	}
	admin := buildSecretEntryHandler(entry, isConfigManagerAllowed(cfg, "custom-ops"))
	if resp := admin(context.Background(), nil, false, request); resp.ErrorCode != protocol.ErrorCodeLoopbackRequired {
		t.Fatal("non-loopback accepted entry")
	}
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionList}
	listed := defaultHandler(context.Background(), nil, false, request)
	if listed.Status != protocol.ResponseStatusOK || listed.SecretEntry == nil || len(listed.SecretEntry.Secrets) != 1 || listed.SecretEntry.Secrets[0].State != protocol.SecretStatePending {
		t.Fatalf("pending metadata unavailable outside admin: %+v", listed)
	}
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionBegin}
	started := admin(context.Background(), nil, true, request)
	if started.SecretEntry == nil || started.SecretEntry.Outcome != protocol.SecretEntryOutcomeActive {
		t.Fatalf("begin refused: %+v", started)
	}
	canary := "  unique-canary-012345678901234567890123  "
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionSubmit, OperationID: started.SecretEntry.OperationID, Name: "zmqcli_auth_token", Value: &canary}
	committed := admin(context.Background(), nil, true, request)
	if committed.SecretEntry == nil || committed.SecretEntry.Outcome != protocol.SecretEntryOutcomeCommitted {
		t.Fatalf("commit failed: %+v", committed)
	}
	if adoptions != 1 || adopted != canary {
		t.Fatalf("the live channel secret was not adopted exactly as entered (adoptions=%d)", adoptions)
	}
	if payload, _ := store.View(context.Background()); payload.Secrets["zmqcli_auth_token"].Value != canary {
		t.Fatal("the value must reach the store exactly as entered (whitespace preserved)")
	}
	encoded, err := protocol.Marshal(committed)
	if err != nil || strings.Contains(string(encoded), strings.TrimSpace(canary)) {
		t.Fatal("response leaked the value")
	}
	short := "short"
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionEnd, OperationID: started.SecretEntry.OperationID}
	if ended := admin(context.Background(), nil, true, request); ended.Status != protocol.ResponseStatusOK {
		t.Fatalf("end refused: %+v", ended)
	}
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionBegin, Name: "zmqcli_auth_token"}
	rotation := admin(context.Background(), nil, true, request)
	request.SecretEntry = &protocol.SecretEntryRequest{Action: protocol.SecretEntryActionSubmit, OperationID: rotation.SecretEntry.OperationID, Name: "zmqcli_auth_token", Value: &short}
	refused := admin(context.Background(), nil, true, request)
	if refused.ErrorCode != protocol.ErrorCodeInvalidValue || refused.SecretEntry == nil || refused.SecretEntry.RuleCode != "too_short" {
		t.Fatalf("a value rule must refuse with its rule code: %+v", refused)
	}
}

func TestChatNonTUISurfacesNeverStartSecretEditor(t *testing.T) {
	for _, mode := range []string{flagNameExec, flagNameRaw, "piped"} {
		t.Run(mode, func(t *testing.T) {
			d := newFakeChatDaemon(t, true, []protocol.PendingSecret{{Name: "telegram_token", Fields: []string{degradedChannelsKey + "." + degradedTelegramKey + "." + chatTokenFlag}, State: protocol.SecretStatePending}})
			command := newChatActionCmd(t, d)
			if mode != flagNameExec {
				if err := command.Set(flagNameExec, ""); err != nil {
					t.Fatal(err)
				}
			}
			if mode == flagNameRaw {
				if err := command.Set(flagNameRaw, "true"); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- runChat(context.Background(), command, &stdout, &stderr, strings.NewReader("/secrets\n"))
			}()
			select {
			case err := <-done:
				var exit cli.ExitCoder
				if !errors.As(err, &exit) || exit.ExitCode() != ExitOK {
					t.Fatalf("non-TUI failed: %v", err)
				}
			case <-time.After(execHangTestTimeout):
				t.Fatal("non-TUI client entered interactive secret workflow")
			}
			select {
			case <-d.secretRequests:
				t.Fatal("conversational content became secret control")
			default:
			}
			if strings.Contains(stdout.String(), "❯ secret") || strings.Contains(stderr.String(), "❯ secret") {
				t.Fatal("non-TUI credential editor rendered")
			}
		})
	}
}

func TestSecretEntryFailureCodesNeverExposePrivateDiagnostics(t *testing.T) {
	for failure, code := range map[error]string{
		tools.ErrInvalidValue:       protocol.ErrorCodeInvalidValue,
		tools.ErrSecretEntryExpired: protocol.ErrorCodeSecretEntryExpired,
		tools.ErrSecretEntryActive:  protocol.ErrorCodeSecretEntryActive,
		tools.ErrNoPendingSecrets:   protocol.ErrorCodeNoPendingSecrets,
		tools.ErrUnknownName:        protocol.ErrorCodeUnknownName,
		tools.ErrNameInUse:          protocol.ErrorCodeNameInUse,
		config.ErrCommitTransient:   protocol.ErrorCodeWriteNotCommitted,
	} {
		resp := secretEntryResponse(tools.SecretEntryResult{}, errors.Join(failure, errors.New("private-canary-value")), protocol.SecretEntryActionSubmit)
		encoded, err := protocol.Marshal(resp)
		if err != nil || resp.ErrorCode != code || strings.Contains(string(encoded), "private-canary-value") {
			t.Fatalf("unsafe error mapping: %+v", resp)
		}
	}
}

func TestChatTokenFileAuthenticatesWithLiteralWhitespace(t *testing.T) {
	for _, value := range []string{"  literal-zmq-secret-01234567890123456789  ", "literal-zmq-secret-01234567890123456789\n"} {
		d := newFakeChatDaemonWithSecret(t, true, value)
		command := newChatActionCmd(t, d)
		if err := command.Set(chatTokenFlag, ""); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(t.TempDir(), "zmq.token")
		if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := command.Set("token-file", file); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		err := runChat(t.Context(), command, &stdout, &stderr, strings.NewReader(""))
		var exit cli.ExitCoder
		if !errors.As(err, &exit) || exit.ExitCode() != ExitOK {
			t.Fatalf("literal token did not authenticate: %v", err)
		}
	}
}

func TestDeriveOAuthCredentialsOrderAndStatesFromDisk(t *testing.T) {
	doc := `config_version: 2
llm:
  default: codex-b::gpt-5
  providers:
    codex-b:
      provider: openai-codex
      api: responses
    codex-a:
      provider: openai-codex
      api: responses
channels:
  zmqcli:
    enabled: true
    auth_token: !secret zmqcli_auth_token
mcp_servers:
  mcp_z:
    url: https://example.com/mcp
    oauth:
      grant: authorization_code
    profiles:
      default:
        allow: ["*"]
  mcp_a:
    url: https://example.com/mcp2
    oauth:
      grant: client_credentials
      client_id: cid
      client_secret: !secret mcp_a_secret
    profiles:
      default:
        allow: ["*"]
  mcp_m:
    url: https://example.com/mcp3
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
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}

	// Store a usable credential for codex-a and mcp_m.
	codexStore := codexauth.NewSecretStore(store, cfg.OAuth.Path)
	codexCred := codexauth.NewCredential("codex-a")
	codexCred.AccessToken = "at"
	codexCred.RefreshToken = "rt"
	codexCred.AccountID = "acct-1"
	codexCred.Expiry = time.Now().Add(time.Hour)
	codexCred.UpdatedAt = time.Now()
	if saveErr := codexStore.Save(t.Context(), codexCred); saveErr != nil {
		t.Fatal(saveErr)
	}

	mcpStore := oauth.NewSecretStore(store, cfg.OAuth.Path)
	mcpCred := oauth.NewCredential("mcp_m")
	mcpCred.AccessToken = "mcp-at"
	mcpCred.Expiry = time.Now().Add(time.Hour)
	mcpCred.ClientID = "cid-m"
	if saveErr := mcpStore.Save(t.Context(), mcpCred); saveErr != nil {
		t.Fatal(saveErr)
	}

	// Also save an orphaned credential not in config.yaml; it must be omitted.
	orphanCred := oauth.NewCredential("orphan")
	orphanCred.AccessToken = "orphan-at"
	orphanCred.Expiry = time.Now().Add(time.Hour)
	orphanCred.ClientID = "cid-o"
	if saveErr := mcpStore.Save(t.Context(), orphanCred); saveErr != nil {
		t.Fatal(saveErr)
	}

	state := &bootSecretState{Store: store}
	entry := newSecretEntry(cfg, state, nil, nil)
	res, err := entry.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	wantOrder := []struct {
		target string
		state  string
	}{
		{"llm:codex-a", "usable"},
		{"llm:codex-b", "login_required"},
		{"mcp:mcp_a", "autonomous"},
		{"mcp:mcp_m", "usable"},
		{"mcp:mcp_z", "login_required"},
	}
	if len(res.Credentials) != len(wantOrder) {
		t.Fatalf("got %d credentials (%+v), want %d", len(res.Credentials), res.Credentials, len(wantOrder))
	}
	for i, w := range wantOrder {
		if res.Credentials[i].Target != w.target || res.Credentials[i].State != w.state {
			t.Fatalf("credentials[%d] = %+v, want target=%s state=%s", i, res.Credentials[i], w.target, w.state)
		}
	}
}

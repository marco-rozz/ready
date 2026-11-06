// Degraded-boot tests (guided-init B.2/B.3/B.5): the degraded view presents
// pending-affected subsystems as off, pending providers fail per candidate,
// a generated/refused ZMQ secret, and the pending set derived from the store.
//
//nolint:goconst // literal fixtures are clearer inline than behind constants
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/rs/zerolog"
)

const (
	degradedChannelsKey = "channels"
	degradedTelegramKey = "telegram"
)

// writeTempConfigFile is this file's config-fixture writer (the config
// package's test helper is not exported).
func writeTempConfigFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), config.ConfigFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	return path
}

func TestDegradedBootView_SubsystemsOff(t *testing.T) {
	cfg := testBootConfig(t)
	pendingPaths := map[string]bool{
		"channels.telegram.token":                 true,
		"home_assistant.websocket.token":          true,
		"mcp_servers.ha.oauth.client_secret":      true,
		"mcp_servers.other.headers.Authorization": true,
	}

	view := degradedBootView(cfg, pendingPaths)

	if view == cfg {
		t.Fatal("the degraded view must be a copy, not the original config")
	}
	if view.Channels.Telegram.Enabled {
		t.Error("telegram must present as disabled with a pending token")
	}
	if view.HomeAssistant.Enabled {
		t.Error("home assistant must present as disabled with a pending websocket token")
	}
	if _, ok := view.MCPServers["ha"]; ok {
		t.Error("the MCP server owning a pending secret must be skipped")
	}
	if _, ok := view.MCPServers["other"]; ok {
		t.Error("the MCP server owning a pending header must be skipped")
	}

	// The original config is never mutated.
	if !cfg.Channels.Telegram.Enabled || !cfg.HomeAssistant.Enabled {
		t.Error("the original configuration must remain untouched")
	}
}

func TestPendingForRef(t *testing.T) {
	pending := map[string]bool{"llm.providers.openrouter.api_key": true}
	if !pendingForRef(nil, "openrouter::gpt", pending) {
		t.Error("the pending provider must fail the reference")
	}
	if pendingForRef(nil, "ollama::gemma4:31b", pending) {
		t.Error("an unrelated provider must not fail")
	}
	if pendingForRef(nil, "gibberish", pending) {
		t.Error("an unparsable reference must not fail")
	}
}

func TestPendingBootEffectsWarnEveryFieldAndNeverValues(t *testing.T) {
	var out bytes.Buffer
	logger := zerolog.New(&out)
	report := secretstore.Report{Secrets: []secretstore.SecretStatus{
		{Name: "telegram_token", Fields: []string{"channels.telegram.token"}, State: secretstore.StatePending},
		{Name: "openrouter_api_key", Fields: []string{"llm.providers.openrouter.api_key"}, State: secretstore.StatePending},
		{Name: "fine", Fields: []string{"x.y"}, State: secretstore.StateSet},
	}}

	logPendingBootEffects(logger, report, true, "/tmp/agent")
	text := out.String()
	for _, field := range []string{"channels.telegram.token", "llm.providers.openrouter.api_key"} {
		if strings.Count(text, field) != 1 {
			t.Fatalf("expected exactly one warning for %s: %s", field, text)
		}
	}
	if strings.Contains(text, "x.y") {
		t.Fatalf("a resolved secret must not warn: %s", text)
	}
}

// bootFixture loads a config from YAML and returns it with a silent logger.
func bootFixture(t *testing.T, yamlBody string) (*config.Config, string) {
	t.Helper()
	path := writeTempConfigFile(t, withTestAgent(yamlBody))
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return cfg, filepath.Dir(path)
}

const zmqBootConfig = "config_version: 2\nllm:\n  default: ollama::gemma4:31b\n  providers:\n    ollama:\n      provider: ollama\n      api: ollama\n      base_url: http://127.0.0.1:11434\nchannels:\n  zmqcli:\n    enabled: true\n    bind: tcp://127.0.0.1:7600\n    auth_token: %s\n"

func TestBootGeneratesAnAbsentZMQSecret(t *testing.T) {
	cfg, dir := bootFixture(t, strings.Replace(zmqBootConfig, "%s", "!secret zmqcli_auth_token", 1))
	var logs bytes.Buffer
	state, err := prepareBootSecrets(t.Context(), cfg, zerolog.New(&logs), dir)
	if err != nil {
		t.Fatalf("an absent ZMQ secret must be generated, not refuse boot: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending after generation = %v", state.Pending)
	}
	payload, _ := state.Store.View(t.Context())
	value := payload.Secrets["zmqcli_auth_token"].Value
	if len(value) != 64 {
		t.Fatalf("generated secret length = %d, want 64 hex characters", len(value))
	}
	if strings.Contains(logs.String(), value) {
		t.Fatal("the generated secret leaked into the log")
	}
	if !strings.Contains(logs.String(), "ready secrets reveal") {
		t.Fatalf("the INFO must carry the reveal hint: %s", logs.String())
	}
	// A second boot reuses it.
	again, err := prepareBootSecrets(t.Context(), cfg, zerolog.Nop(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Store.View(t.Context()); got.Secrets["zmqcli_auth_token"].Value != value {
		t.Fatal("a second boot regenerated the secret")
	}
}

func TestBootRefusesANonConformingZMQSecretAndNeverRegeneratesIt(t *testing.T) {
	cfg, dir := bootFixture(t, strings.Replace(zmqBootConfig, "%s", "!secret zmqcli_auth_token", 1))
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(t.Context(), func(p *secretstore.Payload) error {
		p.Secrets["zmqcli_auth_token"] = secretstore.Secret{Value: "too-short"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = prepareBootSecrets(t.Context(), cfg, zerolog.Nop(), dir)
	if err == nil || !strings.Contains(err.Error(), "ready secrets set zmqcli_auth_token") {
		t.Fatalf("boot must refuse with set guidance, got: %v", err)
	}
	if strings.Contains(err.Error(), "too-short") {
		t.Fatalf("refusal echoes the value: %v", err)
	}
	if got, _ := store.View(t.Context()); got.Secrets["zmqcli_auth_token"].Value != "too-short" {
		t.Fatal("the present secret was overwritten")
	}
}

func TestBootRefusesAMissingExternalZMQFile(t *testing.T) {
	cfg, dir := bootFixture(t, strings.Replace(zmqBootConfig, "%s", "!secret_file /nonexistent/ready/token", 1))
	if _, err := prepareBootSecrets(t.Context(), cfg, zerolog.Nop(), dir); err == nil || !strings.Contains(err.Error(), "missing or empty") {
		t.Fatalf("a missing external file must refuse boot, got: %v", err)
	}
}

func TestPendingBootRestartActivation(t *testing.T) {
	body := todoAwareBootSeed() + "  telegram:\n    enabled: true\n    token: !secret telegram_token\n    allow_from: [\"1\"]\n"
	cfg, dir := bootFixture(t, body)
	state, err := prepareBootSecrets(t.Context(), cfg, zerolog.Nop(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Pending["channels.telegram.token"] {
		t.Fatalf("a reference without a value must be pending: %v", state.Pending)
	}
	if view := degradedBootView(cfg, state.Pending); view == cfg || view.Channels.Telegram.Enabled {
		t.Fatal("the degraded view must switch Telegram off")
	}

	// Writing the value (RED editor / `ready secrets set`) resolves it at the
	// next boot: activation is a restart boundary, not an in-process mutation.
	if err = state.Store.Update(t.Context(), func(p *secretstore.Payload) error {
		p.Secrets["telegram_token"] = secretstore.Secret{Value: "123:valid"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next, err := prepareBootSecrets(t.Context(), cfg, zerolog.Nop(), dir)
	if err != nil || len(next.Pending) != 0 {
		t.Fatalf("after the value is stored: pending=%v err=%v", next.Pending, err)
	}
}

func todoAwareBootSeed() string {
	return "config_version: 2\nllm:\n  default: ollama::gemma4:31b\n  providers:\n    ollama:\n      provider: ollama\n      api: ollama\n      base_url: http://127.0.0.1:11434\nchannels:\n  zmqcli:\n    enabled: false\n"
}

func TestPendingOwnedByTarget(t *testing.T) {
	cfg := testBootConfig(t)
	pending := map[string]bool{
		"mcp_servers.ha.oauth.client_secret": true,
		"llm.providers.codex.api_key":        true,
	}

	if !pendingOwnedByTarget(cfg, oauthTarget{Kind: targetMCP, Name: "ha"}, pending) {
		t.Error("an MCP target owning a pending secret must refuse")
	}
	if !pendingOwnedByTarget(cfg, oauthTarget{Kind: targetLLM, Name: "codex"}, pending) {
		t.Error("an LLM target owning a pending key must refuse")
	}
	if pendingOwnedByTarget(cfg, oauthTarget{Kind: targetMCP, Name: "other"}, pending) {
		t.Error("an unrelated target must not refuse")
	}
}

func TestSecretsSetCommandQuotesTheAgent(t *testing.T) {
	got := secretsSetCommand("/agents/a'b", "openai_api_key")
	if got != "ready secrets set openai_api_key --agent-dir '/agents/a'\"'\"'b'" {
		t.Fatalf("quoted command = %q", got)
	}
}

// testBootConfig builds a working boot-time config fixture (two MCP
// servers, telegram and HA enabled, two LLM providers).
func testBootConfig(t *testing.T) *config.Config {
	t.Helper()

	cfgData := "config_version: 2\nllm:\n  default: ollama::gemma4:31b\n  failover:\n    - openrouter::gpt\n  providers:\n    ollama:\n      provider: ollama\n      api: ollama\n      base_url: http://127.0.0.1:11434\n    openrouter:\n      provider: openrouter\n      api: chat_completions\n      api_key: !secret openrouter_api_key\nchannels:\n  zmqcli:\n    enabled: false\n  telegram:\n    enabled: true\n    token: !secret telegram_token\n    allow_from: [\"1\"]\nhome_assistant:\n  enabled: true\n  websocket:\n    url: ws://ha\n    token: !secret home_assistant_token\nmcp_servers:\n  ha:\n    url: http://ha\n    oauth:\n      grant: client_credentials\n      client_id: cid\n      client_secret: !secret mcp_ha_client_secret\n  other:\n    url: http://other\n    headers:\n      Authorization: !secret mcp_other_authorization\n"

	cfg, err := config.Load(t.Context(), writeTempConfigFile(t, withTestAgent(cfgData)))
	if err != nil {
		t.Fatalf("Load fixture: %v", err)
	}

	return cfg
}

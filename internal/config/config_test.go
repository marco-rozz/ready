package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testOllamaEntry    = "ollama-entry"
	testOllamaEntryKey = "ollama-entry" // provider map key

	testOllamaEntryRef  = testOllamaEntry + "::model"
	testProviderOllama  = ProviderOllama
	testModel           = "test"
	testModelGemma      = "gemma4:31b"
	testModelOllamaProv = "ollama-gemma"
	testProvOR          = "or"
	testProvOC          = "oc"
	testAPIKeyLit       = "key"
)

func TestLoad_ValidConfig(t *testing.T) {
	content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
      base_url: "http://localhost:11434"
channels:
  cli:
    enabled: true
  serial:
    enabled: false
    port: /dev/ttyUSB0
    baud_rate: 2400
  telegram:
    enabled: false
    token: ""
mcp_servers:
  home_assistant:
    url: http://ha.local:8123/mcp
    headers:
      Authorization: "Bearer token123"
skills:
  path: .ready/skills/
  hot_reload: true
sessions:
  path: .ready/sessions/
compaction:
  max_turns: 100
`
	path := writeTempFile(t, content)

	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.LLM.Default != "ollama-gemma::gemma4:31b" {
		t.Errorf("expected default ollama-gemma::gemma4:31b, got %s", cfg.LLM.Default)
	}

	entry, ok := cfg.LLM.Providers[testModelOllamaProv]
	if !ok {
		t.Fatal("expected ollama-gemma provider")
	}

	if entry.Provider != testProviderOllama {
		t.Errorf("expected provider ollama, got %s", entry.Provider)
	}

	if entry.BaseURL != "http://localhost:11434" {
		t.Errorf("expected base_url http://localhost:11434, got %s", entry.BaseURL)
	}

	if !cfg.Channels.CLI.Enabled {
		t.Error("expected CLI enabled")
	}

	if cfg.Channels.Serial.BaudRate != 2400 {
		t.Errorf("expected baud rate 2400, got %d", cfg.Channels.Serial.BaudRate)
	}

	ha, ok := cfg.MCPServers["home_assistant"]
	if !ok {
		t.Fatal("expected home_assistant MCP server")
	}

	if ha.URL != "http://ha.local:8123/mcp" {
		t.Errorf("unexpected MCP URL: %s", ha.URL)
	}

	if cfg.Compaction.MaxTurns != 100 {
		t.Errorf("expected compaction.max_turns 100, got %d", cfg.Compaction.MaxTurns)
	}
}

func TestLoad_CLIOnlyConfig(t *testing.T) {
	content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
`
	path := writeTempFile(t, content)

	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.Channels.CLI.Enabled {
		t.Error("expected CLI enabled")
	}

	if cfg.Channels.Serial.Enabled {
		t.Error("expected serial disabled")
	}

	if cfg.Channels.Telegram.Enabled {
		t.Error("expected telegram disabled")
	}

	// Check defaults were applied (runtime home = the directory holding the
	// elected config).
	runtimeHome := filepath.Dir(path)
	if cfg.Skills.Path != filepath.Join(runtimeHome, "skills") {
		t.Errorf("expected default skills path, got %s", cfg.Skills.Path)
	}

	if cfg.Sessions.Path != filepath.Join(runtimeHome, "sessions") || cfg.Sessions.Reset.Mode != ResetModeDaily || cfg.Sessions.Reset.DailyAt != "04:00" {
		t.Errorf("unexpected sessions defaults: %+v", cfg.Sessions)
	}

	if cfg.Sessions.Retention.Days() != 30 || cfg.Sessions.Retention.MaxTotalSize != 200<<20 {
		t.Errorf("unexpected retention defaults: %+v", cfg.Sessions.Retention)
	}

	if !cfg.Compaction.AutoEnabled() || cfg.Compaction.Skill != "home-compaction" || cfg.Compaction.ReserveTokens != 16384 ||
		cfg.Compaction.KeepRecentTokens != 20000 || cfg.Compaction.MaxTurns != 20 || cfg.Compaction.KeepRecentTurns != 5 ||
		cfg.Compaction.Timeout != 180*time.Second {
		t.Errorf("unexpected compaction defaults: %+v", cfg.Compaction)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(context.Background(), "/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempFile(t, `{{{invalid yaml:::`)

	_, err := Load(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestValidate_MissingDefault(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing default")
	}
}

func TestValidate_MissingModels(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{Default: testModelOllamaProv + "::" + testModel},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing providers")
	}
}

func TestValidate_DanglingDefault(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: "nonexistent::model",
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for dangling default provider")
	}
}

func TestValidate_DanglingFailover(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default:  testModelOllamaProv + "::" + testModel,
			Failover: []string{"nonexistent::model"},
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for dangling failover provider")
	}
}

func TestValidate_BothCredentialFields(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOR + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOR: {
					Provider:   ProviderOpenRouter,
					APIKey:     "inline",
					APIKeyFile: "file",
				},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for both api_key and api_key_file")
	}
}

func TestValidate_APIOnOllama(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testOllamaEntryRef,
			Providers: map[string]LLMProviderConfig{
				testOllamaEntryKey: {
					Provider: testProviderOllama,
					API:      APIChatCompletions,
				},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for api on ollama")
	}
}

func TestValidate_ResponsesAccepted(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOC + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOC: {
					Provider: ProviderOpenCode,
					API:      APIResponses,
					APIKey:   "key",
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected api: responses to validate, got: %v", err)
	}
}

func TestValidate_GoogleAccepted(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOC + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOC: {
					Provider: ProviderOpenCode,
					API:      APIGoogle,
					APIKey:   "key",
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected api: google to validate, got: %v", err)
	}
}

func TestValidate_MissingAPIKeyForKeyedProvider(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOC + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOC: {Provider: ProviderOpenCode},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing api_key on opencode")
	}
}

func TestValidate_LegacyLLMShapeRejected(t *testing.T) {
	content := `
llm:
  provider: "ollama"
  api: ollama
  model: "gemma4:31b"
channels:
  cli:
    enabled: true
`
	path := writeTempFile(t, content)
	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for legacy llm shape")
	}
	if !strings.Contains(err.Error(), "superseded") {
		t.Errorf("expected 'superseded' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "llm.provider") {
		t.Errorf("expected 'llm.provider' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "llm.providers") {
		t.Errorf("expected error to point to llm.providers, got: %v", err)
	}
}

func TestValidate_LegacyModelsShapeRejected(t *testing.T) {
	content := `
llm:
  default: "ollama-gemma"
  models:
    ollama-gemma:
      provider: "ollama"
      api: ollama
      model: "gemma4:31b"
channels:
  cli:
    enabled: true
`
	path := writeTempFile(t, content)
	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for superseded llm.models shape")
	}
	if !strings.Contains(err.Error(), "llm.models") {
		t.Errorf("expected 'llm.models' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "llm.providers") {
		t.Errorf("expected error to point to llm.providers, got: %v", err)
	}
	if !strings.Contains(err.Error(), "<provider>::<model>") {
		t.Errorf("expected error to name the <provider>::<model> form, got: %v", err)
	}
}

func TestValidate_MissingAPIIsRejected(t *testing.T) {
	content := `
llm:
  default: "zen::deepseek-v4-flash-free"
  providers:
    zen:
      provider: "opencode"
      api_key: "test-key"
channels:
  cli:
    enabled: true
`
	path := writeTempFile(t, content)
	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for a missing api, got nil")
	}
	if !strings.Contains(err.Error(), "llm.providers.zen.api is required") {
		t.Errorf("expected error to name the provider instance and the api field, got: %v", err)
	}
}

// TestValidate_ProviderAPITable is the table-driven provider/api compatibility
// matrix (task 1.2): `api` is required on every instance and only the
// documented values are valid per provider type. The same `api` value
// (chat_completions) is valid on BOTH opencode and openrouter — the wire
// protocol does not select the component, the provider type does.
func TestValidate_ProviderAPITable(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		api      string
		wantErr  bool
	}{
		// Ollama accepts exactly its native protocol.
		{"ollama with api ollama", ProviderOllama, APIOllama, false},
		{"ollama with api chat_completions", ProviderOllama, APIChatCompletions, true},
		{"ollama with api responses", ProviderOllama, APIResponses, true},
		{"ollama with api anthropic", ProviderOllama, APIAnthropic, true},
		{"ollama with api google", ProviderOllama, APIGoogle, true},
		{"ollama with missing api", ProviderOllama, "", true},
		// OpenRouter accepts exactly chat_completions.
		{"openrouter with api chat_completions", ProviderOpenRouter, APIChatCompletions, false},
		{"openrouter with api responses", ProviderOpenRouter, APIResponses, true},
		{"openrouter with api ollama", ProviderOpenRouter, APIOllama, true},
		{"openrouter with missing api", ProviderOpenRouter, "", true},
		// OpenCode accepts the documented four APIs.
		{"opencode with api chat_completions", ProviderOpenCode, APIChatCompletions, false},
		{"opencode with api anthropic", ProviderOpenCode, APIAnthropic, false},
		{"opencode with api google", ProviderOpenCode, APIGoogle, false},
		{"opencode with api responses", ProviderOpenCode, APIResponses, false},
		{"opencode with api ollama", ProviderOpenCode, APIOllama, true},
		{"opencode with missing api", ProviderOpenCode, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := LLMProviderConfig{Provider: tc.provider, API: tc.api}
			if tc.provider != ProviderOllama {
				entry.APIKey = testAPIKeyLit
			}

			cfg := &Config{
				LLM: LLMConfig{
					Default:   "prov::model",
					Providers: map[string]LLMProviderConfig{"prov": entry},
				},
			}

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected validation to pass, got: %v", err)
			}
			if tc.wantErr && err != nil {
				if !strings.Contains(err.Error(), "llm.providers.prov.api") {
					t.Errorf("expected error to name llm.providers.prov.api, got: %v", err)
				}
				if !strings.Contains(err.Error(), "prov") {
					t.Errorf("expected error to name the provider instance, got: %v", err)
				}
			}
		})
	}
}

// TestValidate_RecoveryDefaultsAndValidation covers tasks 1.3/1.4: defaults
// apply to an omitted block, and invalid durations, a backoff_factor of one
// or less, and an inverted max/initial pair are rejected naming the field.
func TestValidate_RecoveryDefaultsAndValidation(t *testing.T) {
	t.Run("defaults apply when omitted", func(t *testing.T) {
		content := `
llm:
  default: "zen::deepseek-v4-flash-free"
  providers:
    zen:
      provider: "opencode"
      api: chat_completions
      api_key: "test-key"
channels:
  cli:
    enabled: true
`
		path := writeTempFile(t, content)
		cfg, err := Load(context.Background(), path)
		if err != nil {
			t.Fatalf("unexpected load error: %v", err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}

		if got := cfg.LLM.ResolvedFailoverAttemptTimeout(); got != 45*time.Second {
			t.Errorf("FailoverAttemptTimeout = %s, want 45s", got)
		}
		if got := cfg.LLM.ResolvedFailoverRetries(); got != 2 {
			t.Errorf("FailoverRetries = %d, want 2", got)
		}
		if got := cfg.LLM.ResolvedFailoverRetryBackoff(); got != 2*time.Second {
			t.Errorf("FailoverRetryBackoff = %s, want 2s", got)
		}

		r := cfg.LLM.ResolvedRecovery()
		if r.Soft.InitialCooldown != 30*time.Second || r.Soft.MaxCooldown != 5*time.Minute {
			t.Errorf("soft ladder = %s..%s, want 30s..5m", r.Soft.InitialCooldown, r.Soft.MaxCooldown)
		}
		if r.Hard.InitialCooldown != 5*time.Minute || r.Hard.MaxCooldown != 1*time.Hour {
			t.Errorf("hard ladder = %s..%s, want 5m..1h", r.Hard.InitialCooldown, r.Hard.MaxCooldown)
		}
		if r.BackoffFactor != 2 {
			t.Errorf("BackoffFactor = %v, want 2", r.BackoffFactor)
		}
		if r.ProbeTimeout != 10*time.Second {
			t.Errorf("ProbeTimeout = %s, want 10s", r.ProbeTimeout)
		}
		if r.IdleProbeWindow != 15*time.Minute {
			t.Errorf("IdleProbeWindow = %s, want 15m", r.IdleProbeWindow)
		}
	})

	invalidCases := []struct {
		name      string
		recovery  string
		wantField string
	}{
		{
			"negative attempt timeout",
			"",
			"llm.failover_attempt_timeout",
		},
		{
			"negative soft initial cooldown",
			"  recovery:\n    soft:\n      initial_cooldown: -30s\n",
			"llm.recovery.soft.initial_cooldown",
		},
		{
			"inverted soft bounds",
			"  recovery:\n    soft:\n      initial_cooldown: 5m\n      max_cooldown: 30s\n",
			"llm.recovery.soft.max_cooldown",
		},
		{
			"inverted hard bounds",
			"  recovery:\n    hard:\n      initial_cooldown: 1h\n      max_cooldown: 5m\n",
			"llm.recovery.hard.max_cooldown",
		},
		{
			"backoff factor of one",
			"  recovery:\n    backoff_factor: 1\n",
			"llm.recovery.backoff_factor",
		},
		{
			"backoff factor below one",
			"  recovery:\n    backoff_factor: 0.5\n",
			"llm.recovery.backoff_factor",
		},
		{
			"negative failover retries",
			"  failover_retries: -1\n",
			"llm.failover_retries",
		},
		{
			"too many failover retries",
			"  failover_retries: 6\n",
			"llm.failover_retries",
		},
		{
			"zero retry backoff",
			"  failover_retry_backoff: 0s\n",
			"llm.failover_retry_backoff",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := ""
			if tc.name == "negative attempt timeout" {
				attempt = "  failover_attempt_timeout: -45s\n"
			}
			content := "\nllm:\n  default: \"zen::deepseek-v4-flash-free\"\n" + attempt + tc.recovery +
				"  providers:\n    zen:\n      provider: \"opencode\"\n      api: chat_completions\n      api_key: \"test-key\"\nchannels:\n  cli:\n    enabled: true\n"
			path := writeTempFile(t, content)
			cfg, err := Load(context.Background(), path)
			if err != nil {
				t.Fatalf("unexpected load error: %v", err)
			}

			err = cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("expected error to name %s, got: %v", tc.wantField, err)
			}
		})
	}

	t.Run("explicit zero disables attempt budget and retries", func(t *testing.T) {
		content := `
llm:
  default: "zen::model"
  failover_attempt_timeout: 0s
  failover_retries: 0
  providers:
    zen:
      provider: "opencode"
      api: chat_completions
      api_key: "test-key"
channels:
  cli:
    enabled: true
`
		cfg, err := Load(context.Background(), writeTempFile(t, content))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() error = %v", err)
		}
		if got := cfg.LLM.ResolvedFailoverAttemptTimeout(); got != 0 {
			t.Errorf("attempt timeout = %s, want disabled", got)
		}
		if got := cfg.LLM.ResolvedFailoverRetries(); got != 0 {
			t.Errorf("retries = %d, want disabled", got)
		}
	})
}

// TestValidate_SameAPIOnDifferentProviders pins the provider/api component
// split at the validation layer (task 1.2): chat_completions is valid on both
// an opencode and an openrouter instance in the same registry — the provider
// type, not the api value, selects the construction path.
func TestValidate_SameAPIOnDifferentProviders(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default:  "oc::model",
			Failover: []string{"or::model"},
			Providers: map[string]LLMProviderConfig{
				"oc": {Provider: ProviderOpenCode, API: APIChatCompletions, APIKey: "k"},
				"or": {Provider: ProviderOpenRouter, API: APIChatCompletions, APIKey: "k"},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected both chat_completions instances to validate, got: %v", err)
	}
}

func TestValidate_TelegramEnabledWithoutToken(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		Channels: ChannelsConfig{
			Telegram: TelegramChannelConfig{Enabled: true, Token: ""},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for telegram without token")
	}
}

func TestValidate_SerialEnabledWithoutPort(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		Channels: ChannelsConfig{
			Serial: SerialChannelConfig{Enabled: true, Port: ""},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for serial without port")
	}
}

func TestValidate_ValidConfig(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default:   testProviderOllama + "::" + testModelGemma,
			Providers: map[string]LLMProviderConfig{testProviderOllama: {Provider: testProviderOllama, API: APIOllama}},
		},
		Channels: ChannelsConfig{
			CLI: CLIChannelConfig{Enabled: true},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestLoad_NewOwnerProfileShape(t *testing.T) {
	content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
mcp_servers:
  home_assistant:
    url: http://ha.local:8123/mcp
    profiles:
      default:
        deny: ["get_state"]
      edit:
        allow: ["*"]
home_assistant:
  enabled: false
  profiles:
    default:
      allow: ["ha_state_query"]
builtin_tools:
  profiles:
    default:
      allow: ["wait"]
    edit:
      allow: ["*"]
`
	path := writeTempFile(t, content)

	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error for new owner-profile shape: %v", err)
	}

	ha := cfg.MCPServers["home_assistant"]
	if len(ha.Profiles["default"].Deny) != 1 || ha.Profiles["default"].Deny[0] != "get_state" {
		t.Errorf("expected home_assistant.profiles.default.deny=[get_state], got %#v", ha.Profiles["default"])
	}

	if len(cfg.BuiltinTools.Profiles["default"].Allow) != 1 || cfg.BuiltinTools.Profiles["default"].Allow[0] != "wait" {
		t.Errorf("expected builtin_tools.profiles.default.allow=[wait], got %#v", cfg.BuiltinTools.Profiles["default"])
	}
}

func TestLoad_DiscloseDeniedToolsFlag(t *testing.T) {
	base := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
mcp_profile: default
`

	t.Run("flag alongside inject_skills loads and validates", func(t *testing.T) {
		content := base + `
profiles:
  default:
    inject_skills: ["home-provisioning"]
    disclose_denied_tools: true
`
		path := writeTempFile(t, content)

		cfg, err := Load(context.Background(), path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}

		prof := cfg.Profiles["default"]
		if !prof.DiscloseDeniedTools {
			t.Error("expected disclose_denied_tools=true")
		}
		if len(prof.InjectSkills) != 1 || prof.InjectSkills[0] != "home-provisioning" {
			t.Errorf("expected inject_skills=[home-provisioning], got %#v", prof.InjectSkills)
		}
	})

	t.Run("absent flag defaults false", func(t *testing.T) {
		content := base + `
profiles:
  default: {}
`
		path := writeTempFile(t, content)

		cfg, err := Load(context.Background(), path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.Profiles["default"].DiscloseDeniedTools {
			t.Error("expected disclose_denied_tools to default false")
		}
	})

	t.Run("non-boolean value rejected at load", func(t *testing.T) {
		content := base + `
profiles:
  default:
    disclose_denied_tools: "yes-please"
`
		path := writeTempFile(t, content)

		_, err := Load(context.Background(), path)
		if err == nil {
			t.Fatal("expected error for non-boolean disclose_denied_tools")
		}
		if !strings.Contains(err.Error(), "disclose_denied_tools") && !strings.Contains(err.Error(), "bool") {
			t.Errorf("expected error naming the offending key/type, got: %v", err)
		}
	})
}

func TestValidate_RejectsPreRefactorProfileShape(t *testing.T) {
	content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
profiles:
  default:
    allow: ["*turn_on"]
    deny: ["*config_*"]
    edit_tools: false
`
	path := writeTempFile(t, content)

	cfg, err := Load(context.Background(), path)
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for pre-refactor profiles.default.allow/deny/edit_tools shape")
	}

	for _, field := range []string{"profiles.default.allow", "profiles.default.deny", "profiles.default.edit_tools"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("expected error to name field %q, got: %v", field, err)
		}
	}

	if !strings.Contains(err.Error(), "MIGRATION.md") {
		t.Errorf("expected error to point to MIGRATION.md, got: %v", err)
	}
}

func TestApplyDefaults_TwinThresholds(t *testing.T) {
	cfg := &Config{
		LLM:      validLLMConfig(),
		Channels: ChannelsConfig{CLI: CLIChannelConfig{Enabled: true}},
	}
	cfg.applyDefaults()

	if cfg.HomeAssistant.StaleThresholdStates != 60*time.Second {
		t.Errorf("expected default stale_threshold_states=60s, got %v", cfg.HomeAssistant.StaleThresholdStates)
	}
	if cfg.HomeAssistant.HardStaleThreshold != 300*time.Second {
		t.Errorf("expected default hard_stale_threshold=300s, got %v", cfg.HomeAssistant.HardStaleThreshold)
	}
	if cfg.HomeAssistant.OutstandingPingsThreshold != 2 {
		t.Errorf("expected default outstanding_pings_threshold=2, got %d", cfg.HomeAssistant.OutstandingPingsThreshold)
	}
	if cfg.BuiltinTools.Profiles == nil {
		t.Fatal("expected builtin_tools.profiles to be synthesized when absent")
	}
	if _, ok := cfg.BuiltinTools.Profiles["default"]; !ok {
		t.Error("expected synthesized builtin_tools.profiles.default")
	}
}

func TestHasEnabledChannels(t *testing.T) {
	cfg := &Config{}
	if cfg.HasEnabledChannels() {
		t.Error("expected no enabled channels")
	}

	cfg.Channels.CLI.Enabled = true
	if !cfg.HasEnabledChannels() {
		t.Error("expected enabled channels")
	}
}

// TestLoggingThoughts verifies the Thoughts field behaviour:
// absent → false; explicit true → true; explicit false → false.
func TestLoggingThoughts_Absent(t *testing.T) {
	content := `
llm:
  default: "p::test"
  providers:
    p:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
logging:
  level: info
`
	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Logging.Thoughts {
		t.Error("expected Thoughts=false when absent from YAML")
	}
}

func TestLoggingThoughts_ExplicitTrue(t *testing.T) {
	content := `
llm:
  default: "p::test"
  providers:
    p:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
logging:
  level: debug
  thoughts: true
`
	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Logging.Thoughts {
		t.Error("expected Thoughts=true when explicitly set")
	}
}

func TestLoggingThoughts_ExplicitFalse(t *testing.T) {
	content := `
llm:
  default: "p::test"
  providers:
    p:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
logging:
  level: info
  thoughts: false
`
	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Logging.Thoughts {
		t.Error("expected Thoughts=false when explicitly false")
	}
}

func TestLoggingThoughts_NonBooleanError(t *testing.T) {
	content := `
llm:
  default: "p::test"
  providers:
    p:
      provider: "ollama"
      api: ollama
channels:
  cli:
    enabled: true
logging:
  thoughts:
    - not
    - a
    - bool
`
	_, err := Load(context.Background(), writeTempFile(t, content))
	if err == nil {
		t.Error("expected error when thoughts is a sequence (non-boolean)")
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	return path
}

func strPtr(s string) *string { return &s }

const testServerKey = "home_assistant"

// TestEffectiveToolPrefix verifies the three-way resolution logic:
// nil → server key, "" → no prefix, non-empty → use value.
func TestEffectiveToolPrefix(t *testing.T) {
	tests := []struct {
		name      string
		cfg       MCPServerConfig
		serverKey string
		want      string
	}{
		{
			name:      "nil pointer uses server key",
			cfg:       MCPServerConfig{ToolPrefix: nil},
			serverKey: testServerKey,
			want:      testServerKey,
		},
		{
			name:      "empty string disables prefix",
			cfg:       MCPServerConfig{ToolPrefix: strPtr("")},
			serverKey: testServerKey,
			want:      "",
		},
		{
			name:      "non-empty string uses custom prefix",
			cfg:       MCPServerConfig{ToolPrefix: strPtr("ha")},
			serverKey: testServerKey,
			want:      "ha",
		},
		{
			name:      "non-empty overrides any server key",
			cfg:       MCPServerConfig{ToolPrefix: strPtr("media")},
			serverKey: "media_player",
			want:      "media",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.EffectiveToolPrefix(tc.serverKey)
			if got != tc.want {
				t.Errorf("EffectiveToolPrefix(%q) = %q, want %q", tc.serverKey, got, tc.want)
			}
		})
	}
}

func TestValidate_KeepAliveOnNonOllama(t *testing.T) {
	ka := 30 * time.Minute
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOR + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOR: {
					Provider:  ProviderOpenRouter,
					API:       APIChatCompletions,
					APIKey:    testAPIKeyLit,
					KeepAlive: &ka,
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for keep_alive on openrouter")
	}
	if !strings.Contains(err.Error(), "ollama-only") {
		t.Errorf("expected error to mention ollama-only, got: %v", err)
	}
	if !strings.Contains(err.Error(), testProvOR) {
		t.Errorf("expected error to name the provider instance, got: %v", err)
	}
}

func TestValidate_OptionsOnNonOllama(t *testing.T) {
	temp := float32(0.3)
	cfg := &Config{
		LLM: LLMConfig{
			Default: testProvOC + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testProvOC: {
					Provider: ProviderOpenCode,
					API:      APIChatCompletions,
					APIKey:   testAPIKeyLit,
					Options:  &OllamaOptions{Temperature: &temp},
				},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for options on opencode")
	}
	if !strings.Contains(err.Error(), "ollama-only") {
		t.Errorf("expected error to mention ollama-only, got: %v", err)
	}
}

func TestValidate_OllamaKeepAliveAndOptionsOK(t *testing.T) {
	ka := 30 * time.Minute
	np := 256
	temp := float32(0.3)
	cfg := &Config{
		LLM: LLMConfig{
			Default: testModelOllamaProv + "::" + testModel,
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {
					Provider:  testProviderOllama,
					API:       APIOllama,
					KeepAlive: &ka,
					Options:   &OllamaOptions{NumPredict: &np, Temperature: &temp},
				},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected validation to pass for ollama provider with keep_alive+options, got: %v", err)
	}
}

// TestValidate_SingleProviderRegistry covers the single-provider scenario from
// the spec: one llm.providers entry and a default referencing a model on it.
func TestValidate_SingleProviderRegistry(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testModelOllamaProv + "::" + testModelGemma,
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected single-provider registry to validate, got: %v", err)
	}
}

// TestValidate_ReferenceMissingSeparator covers the spec scenario where a
// reference is a bare model name with no '::'.
func TestValidate_ReferenceMissingSeparator(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testModelGemma, // no '::'
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for reference without '::'")
	}
	if !strings.Contains(err.Error(), "missing the '::' separator") {
		t.Errorf("expected missing-separator error, got: %v", err)
	}
}

// TestValidate_ReferenceEmptyModelID covers the spec scenario where a
// reference has the form '<provider>::' (empty model-id).
func TestValidate_ReferenceEmptyModelID(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testModelOllamaProv + "::",
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for empty model-id")
	}
	if !strings.Contains(err.Error(), "empty model-id") {
		t.Errorf("expected empty model-id error, got: %v", err)
	}
}

// TestValidate_DuplicateReference covers the spec scenario where the same
// '<provider>::<model>' appears in both default and failover.
func TestValidate_DuplicateReference(t *testing.T) {
	ref := testModelOllamaProv + "::" + testModel
	cfg := &Config{
		LLM: LLMConfig{
			Default:  ref,
			Failover: []string{ref},
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for duplicate reference")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

// TestValidate_DuplicateReferenceInFailover covers the same reference twice
// within failover.
func TestValidate_DuplicateReferenceInFailover(t *testing.T) {
	ref := testModelOllamaProv + "::" + testModel
	other := testModelOllamaProv + "::other"
	cfg := &Config{
		LLM: LLMConfig{
			Default:  other,
			Failover: []string{ref, ref},
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for duplicate reference in failover")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

// TestValidate_ModelIDWithSingleColon covers the spec scenario where a
// reference is 'ollama::gemma4:31b' — split on the first '::' only, model-id
// 'gemma4:31b' used verbatim.
func TestValidate_ModelIDWithSingleColon(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: testModelOllamaProv + "::" + testModelGemma, // gemma4:31b
			Providers: map[string]LLMProviderConfig{
				testModelOllamaProv: {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected reference with single-colon model-id to validate, got: %v", err)
	}
}

// TestValidate_TwoSameTypeProviders covers the spec scenario where two
// opencode providers are declared under different instance names with
// different base_urls; references qualify the intended instance by name.
func TestValidate_TwoSameTypeProviders(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default:  "zen1::gpt-5.4-mini",
			Failover: []string{"zen2::claude-sonnet-4-5"},
			Providers: map[string]LLMProviderConfig{
				"zen1": {Provider: ProviderOpenCode, API: APIResponses, APIKey: "k1", BaseURL: "https://gw1/zen/v1"},
				"zen2": {Provider: ProviderOpenCode, API: APIAnthropic, APIKey: "k2", BaseURL: "https://gw2/zen/v1"},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected two-same-type-providers registry to validate, got: %v", err)
	}
}

// TestValidate_ProviderInstanceNameWithSeparator covers the spec rule that
// provider instance names MUST NOT contain '::'.
func TestValidate_ProviderInstanceNameWithSeparator(t *testing.T) {
	cfg := &Config{
		LLM: LLMConfig{
			Default: "weird::name::gemma4:31b",
			Providers: map[string]LLMProviderConfig{
				"weird::name": {Provider: testProviderOllama, API: APIOllama},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for provider instance name containing '::'")
	}
	if !strings.Contains(err.Error(), "must not contain '::'") {
		t.Errorf("expected '::' name error, got: %v", err)
	}
}

// TestParseModelReference covers the helper's split semantics directly.
func TestParseModelReference(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		wantProv    string
		wantModel   string
		wantErr     bool
		errContains string
	}{
		{"basic", "ollama::gemma4:31b", "ollama", "gemma4:31b", false, ""},
		{"first-separator only", "a::b::c", "a", "b::c", false, ""},
		{"missing separator", "gemma4:31b", "", "", true, "missing the '::' separator"},
		{"empty model-id", "p::", "", "", true, "empty model-id"},
		{"empty provider", "::model", "", "", true, "empty provider name"},
		{"empty ref", "", "", "", true, "missing the '::' separator"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov, model, err := ParseModelReference(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tc.ref)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("expected error containing %q, got: %v", tc.errContains, err)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.ref, err)
			}
			if prov != tc.wantProv {
				t.Errorf("provider = %q, want %q", prov, tc.wantProv)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
		})
	}
}

// TestValidate_ContextWindow covers the tui-usage-observability validation
// rule: context_window is optional on every provider type, must be positive
// when set, and boot fails naming the field otherwise.
func TestValidate_ContextWindow(t *testing.T) {
	t.Run("absent is accepted", func(t *testing.T) {
		cfg := &Config{
			LLM: LLMConfig{
				Default: testOllamaEntryRef,
				Providers: map[string]LLMProviderConfig{
					testOllamaEntryKey: {Provider: testProviderOllama, API: APIOllama},
				},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("positive is accepted on any provider", func(t *testing.T) {
		cfg := &Config{
			LLM: LLMConfig{
				Default: "or-entry::model",
				Providers: map[string]LLMProviderConfig{
					"or-entry": {Provider: ProviderOpenRouter, API: APIChatCompletions, APIKey: "inline", ContextWindow: 128000},
				},
			},
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	for _, bad := range []int{-1, -8192} {
		t.Run(fmt.Sprintf("negative %d rejected", bad), func(t *testing.T) {
			cfg := &Config{
				LLM: LLMConfig{
					Default: testOllamaEntryRef,
					Providers: map[string]LLMProviderConfig{
						testOllamaEntryKey: {Provider: testProviderOllama, API: APIOllama, ContextWindow: bad},
					},
				},
			}
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error for a negative context_window")
			}
			if !strings.Contains(err.Error(), "context_window") {
				t.Errorf("error must name the field, got: %v", err)
			}
		})
	}
}

// session-lifecycle 1.5/2.3/3.7: removed or renamed keys fail validation with
// a message naming the replacement (plain yaml.Unmarshal would drop them).
func TestValidate_RemovedKeysRejected(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"plans_path", "persistence:\n  plans_path: .ready/plans/\n", "persistence.plans_path removed"},
		{"plan_ttl", "persistence:\n  plan_ttl: 24h\n", "persistence.plan_ttl removed"},
		{"session_timeout", "  zmqcli:\n    session_timeout: 15m\n", "use channels.zmqcli.connection_idle_timeout"},
		{"checkpoints_path", "persistence:\n  checkpoints_path: .ready/checkpoints/\n", "use sessions.path"},
		{"max_history", "persistence:\n  max_history: 50\n", "use compaction.max_turns"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: ollama
      api: ollama
channels:
  cli:
    enabled: true
` + tc.block

			cfg, err := Load(context.Background(), writeTempFile(t, content))
			if err != nil {
				t.Fatalf("unexpected load error: %v", err)
			}

			err = cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func loadSessionsYAML(t *testing.T, block string) (*Config, error) {
	t.Helper()

	content := `
llm:
  default: "ollama-gemma::gemma4:31b"
  providers:
    ollama-gemma:
      provider: ollama
      api: ollama
channels:
  cli:
    enabled: true
` + block

	cfg, err := Load(context.Background(), writeTempFile(t, content))
	if err != nil {
		t.Fatalf("unexpected load error: %v", err)
	}

	return cfg, cfg.Validate()
}

// session-lifecycle 3.7: a channel override replaces only the fields it sets.
func TestSessionResetFor_ChannelOverrideMerges(t *testing.T) {
	cfg, err := loadSessionsYAML(t, `  telegram:
    session_reset:
      mode: daily+idle
      idle_after: 6h
sessions:
  reset:
    daily_at: "05:30"
    notify: false
`)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tg := cfg.SessionResetFor("telegram")
	if tg.Mode != ResetModeDailyIdle || tg.IdleAfter != 6*time.Hour || tg.DailyAt != "05:30" || tg.NotifyEnabled() {
		t.Errorf("telegram rule = %+v, want daily+idle/6h/05:30/notify off", tg)
	}

	cli := cfg.SessionResetFor("cli")
	if cli.Mode != ResetModeDaily || cli.DailyAt != "05:30" {
		t.Errorf("cli rule = %+v, want the global rule", cli)
	}
}

func TestValidate_SessionsRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"bad mode":          "sessions:\n  reset:\n    mode: weekly\n",
		"bad daily_at":      "sessions:\n  reset:\n    daily_at: \"25:00\"\n",
		"idle without time": "sessions:\n  reset:\n    mode: idle\n",
		"negative days":     "sessions:\n  retention:\n    retention_days: -1\n",
		"keep >= max turns": "compaction:\n  max_turns: 5\n  keep_recent_turns: 5\n",
		"channel override":  "  telegram:\n    session_reset:\n      mode: idle\n",
	}

	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadSessionsYAML(t, block); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestSessionRetention_ZeroDaysAllowed(t *testing.T) {
	cfg, err := loadSessionsYAML(t, "sessions:\n  retention:\n    retention_days: 0\n    max_total_size: 1MB\n")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.Sessions.Retention.Days() != 0 || cfg.Sessions.Retention.MaxTotalSize != 1<<20 {
		t.Errorf("retention = %+v", cfg.Sessions.Retention)
	}
}

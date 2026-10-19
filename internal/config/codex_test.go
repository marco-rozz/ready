package config

import (
	"strings"
	"testing"
	"time"
)

func codexConfig() *Config {
	return &Config{
		LLM: LLMConfig{
			Default:  "codex-work::gpt-5-codex",
			Failover: []string{"codex-personal::gpt-5-codex"},
			Providers: map[string]LLMProviderConfig{
				"codex-work":     {Provider: ProviderOpenAICodex, API: APIResponses},
				"codex-personal": {Provider: ProviderOpenAICodex, API: APIResponses},
			},
		},
		// Pin the channels default off: these tests assert on the LLM provider
		// surface only, and an unconfigured channels block now defaults to
		// zmqcli enabled (which requires an auth token at validation time).
		Channels: ChannelsConfig{ZMQCLI: ZMQCLIChannelConfig{Enabled: false}, zmqCLIEnabledSet: true},
	}
}

func TestValidateOpenAICodexProviders(t *testing.T) {
	cfg := codexConfig()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for name, provider := range cfg.LLM.Providers {
		if provider.Name != name {
			t.Fatalf("provider name = %q, want %q", provider.Name, name)
		}
		if provider.BaseURL != "https://chatgpt.com/backend-api" {
			t.Fatalf("base_url = %q", provider.BaseURL)
		}
	}
}

func TestOpenAICodexPreservesExplicitBaseURLAndLegacyProvider(t *testing.T) {
	cfg := codexConfig()
	work := cfg.LLM.Providers["codex-work"]
	work.BaseURL = "https://chatgpt.example/backend-api"
	cfg.LLM.Providers["codex-work"] = work
	cfg.LLM.Providers["local"] = LLMProviderConfig{Provider: ProviderOllama, API: APIOllama}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.LLM.Providers["codex-work"].BaseURL; got != "https://chatgpt.example/backend-api" {
		t.Fatalf("explicit base_url = %q", got)
	}
	if got := cfg.LLM.Providers["local"].BaseURL; got != "" {
		t.Fatalf("legacy Ollama config changed unexpectedly: base_url=%q", got)
	}
}

func TestValidateOpenAICodexRejectsStaticCredentialsAndWrongAPI(t *testing.T) {
	for name, mutate := range map[string]func(*LLMProviderConfig){
		"api_key":      func(p *LLMProviderConfig) { p.APIKey = "secret" },
		"api_key_file": func(p *LLMProviderConfig) { p.APIKeyFile = "/secret" },
		"wrong_api":    func(p *LLMProviderConfig) { p.API = APIChatCompletions },
		"keep_alive":   func(p *LLMProviderConfig) { d := time.Second; p.KeepAlive = &d },
		"options":      func(p *LLMProviderConfig) { p.Options = &OllamaOptions{} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := codexConfig()
			entry := cfg.LLM.Providers["codex-work"]
			mutate(&entry)
			cfg.LLM.Providers["codex-work"] = entry
			cfg.applyDefaults()
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "codex-work") {
				t.Fatalf("Validate error = %v", err)
			}
		})
	}
}

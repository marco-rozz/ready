package llm

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

// testLocalProv is the shared provider-instance name in this file's fixtures.
const testLocalProv = "local"

// Task 2.6: layout mapping per api kind; unknown protocol degrades to
// LayoutHeadSafe rather than risking a trailing-system-message 400
// (design.md §D1).
func TestPromptLayoutFor(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.LLMProviderConfig
		want PromptLayout
	}{
		{"ollama", config.LLMProviderConfig{Provider: config.ProviderOllama}, LayoutTailDynamic},
		{"openrouter", config.LLMProviderConfig{Provider: config.ProviderOpenRouter}, LayoutTailDynamic},
		{"opencode-chat_completions", config.LLMProviderConfig{Provider: config.ProviderOpenCode, API: config.APIChatCompletions}, LayoutTailDynamic},
		{"opencode-responses", config.LLMProviderConfig{Provider: config.ProviderOpenCode, API: config.APIResponses}, LayoutTailDynamic},
		{"opencode-anthropic", config.LLMProviderConfig{Provider: config.ProviderOpenCode, API: config.APIAnthropic}, LayoutHeadSafe},
		{"opencode-google", config.LLMProviderConfig{Provider: config.ProviderOpenCode, API: config.APIGoogle}, LayoutHeadSafe},
		{"opencode-unknown-api", config.LLMProviderConfig{Provider: config.ProviderOpenCode, API: "some-future-api"}, LayoutHeadSafe},
		{"unknown-provider", config.LLMProviderConfig{Provider: "some-future-provider"}, LayoutHeadSafe},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := promptLayoutFor(&tt.cfg)
			if got != tt.want {
				t.Errorf("promptLayoutFor(%+v) = %v, want %v", tt.cfg, got, tt.want)
			}
		})
	}
}

// TestPromptLayout_String verifies the log-facing vocabulary from §D1.
func TestPromptLayout_String(t *testing.T) {
	if LayoutTailDynamic.String() != "tail-dynamic" {
		t.Errorf("LayoutTailDynamic.String() = %q, want %q", LayoutTailDynamic.String(), "tail-dynamic")
	}
	if LayoutHeadSafe.String() != "head-safe" {
		t.Errorf("LayoutHeadSafe.String() = %q, want %q", LayoutHeadSafe.String(), "head-safe")
	}
}

// TestCompileCandidates_OrderAndDescriptors pins the immutable ordered
// candidate list (tasks 1.5/1.6): default first, then failover entries in
// declared order, each descriptor carrying the resolved provider instance,
// wire API, and prompt layout.
func TestCompileCandidates_OrderAndDescriptors(t *testing.T) {
	registry := &config.LLMConfig{
		Default:  testLocalProv + "::gemma4:31b",
		Failover: []string{"cloud::claude-4", testLocalProv + "::qwen3"},
		Providers: map[string]config.LLMProviderConfig{
			testLocalProv: {Provider: config.ProviderOllama, API: config.APIOllama},
			"cloud":       {Provider: config.ProviderOpenCode, API: config.APIAnthropic, APIKey: "k"},
		},
	}

	descs, err := CompileCandidates(registry)
	if err != nil {
		t.Fatalf("CompileCandidates() error: %v", err)
	}
	if len(descs) != 3 {
		t.Fatalf("len(descs) = %d, want 3", len(descs))
	}

	if descs[0].Ref != testLocalProv+"::gemma4:31b" || descs[0].Provider != "local" || descs[0].API != config.APIOllama {
		t.Errorf("descs[0] = %+v, want the default first", descs[0])
	}
	if descs[0].Layout != LayoutTailDynamic {
		t.Errorf("descs[0].Layout = %v, want tail-dynamic", descs[0].Layout)
	}
	if descs[1].Ref != "cloud::claude-4" || descs[1].API != config.APIAnthropic {
		t.Errorf("descs[1] = %+v, want the first failover second", descs[1])
	}
	if descs[1].Layout != LayoutHeadSafe {
		t.Errorf("descs[1].Layout = %v, want head-safe", descs[1].Layout)
	}
	if descs[2].Ref != testLocalProv+"::qwen3" || descs[2].Provider != "local" {
		t.Errorf("descs[2] = %+v, want the second failover third", descs[2])
	}
}

// TestCompileCandidates_FatalGlobalErrors pins the fatal-global versus
// per-candidate error split (task 1.5): malformed references, undeclared
// providers, empty model-ids, and duplicates fail compilation outright —
// they never become a per-candidate failure.
func TestCompileCandidates_FatalGlobalErrors(t *testing.T) {
	base := func() *config.LLMConfig {
		return &config.LLMConfig{
			Default: testLocalProv + "::model",
			Providers: map[string]config.LLMProviderConfig{
				testLocalProv: {Provider: config.ProviderOllama, API: config.APIOllama},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*config.LLMConfig)
		wantSub string
	}{
		{
			"undeclared provider in default",
			func(r *config.LLMConfig) { r.Default = "ghost::model" },
			"ghost",
		},
		{
			"undeclared provider in failover",
			func(r *config.LLMConfig) { r.Failover = []string{"ghost::model"} },
			"ghost",
		},
		{
			"empty model-id",
			func(r *config.LLMConfig) { r.Failover = []string{"local::"} },
			"empty model-id",
		},
		{
			"duplicate reference",
			func(r *config.LLMConfig) { r.Failover = []string{testLocalProv + "::model"} },
			"duplicate",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := base()
			tc.mutate(registry)

			if _, err := CompileCandidates(registry); err == nil {
				t.Fatal("expected a fatal global error, got nil")
			} else if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("expected error to mention %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

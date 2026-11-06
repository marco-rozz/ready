package config

import (
	"testing"
)

const (
	testOAuthServerURL = "https://example.com/mcp"
	testOAuthServerKey = "srv"
)

func validLLMConfig() LLMConfig {
	return LLMConfig{
		Default:   testModel + "::" + testModel,
		Providers: map[string]LLMProviderConfig{testModel: {Provider: testProviderOllama, API: APIOllama}},
	}
}

func TestApplyDefaults_OAuthRoot(t *testing.T) {
	cfg := &Config{Agent: testAgent, LLM: validLLMConfig()}
	cfg.applyDefaults()

	// The store default is no longer a cwd-relative literal: it anchors to
	// the Install home and is only resolved (by ApplyRuntimeHome) when an
	// OAuth-capable target exists. A direct construction with no target has
	// no consumer, so Path stays empty.
	if cfg.OAuth.Path != "" {
		t.Errorf("Path = %q, want empty (no OAuth targets configured)", cfg.OAuth.Path)
	}
	if cfg.OAuth.RedirectPort != 7601 {
		t.Errorf("RedirectPort = %d, want 7601", cfg.OAuth.RedirectPort)
	}
}

func TestApplyDefaults_OAuthGrantAuto(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: SecretString{Value: testOAuthServerURL}, OAuth: &MCPOAuthConfig{}},
		},
	}
	cfg.applyDefaults()

	if got := cfg.MCPServers[testOAuthServerKey].OAuth.Grant; got != grantAuto {
		t.Errorf("Grant = %q, want %q", got, grantAuto)
	}
}

func TestValidate_OAuth_UnknownGrant(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: SecretString{Value: testOAuthServerURL}, OAuth: &MCPOAuthConfig{Grant: "bogus"}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unknown grant")
	}
}

func TestValidate_OAuth_RedirectPortOutOfRange(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		OAuth: OAuthConfig{RedirectPort: 70000},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for redirect_port out of range")
	}
}

func TestValidate_OAuth_ExcludesHeaderAuthorization(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {
				URL:     SecretString{Value: testOAuthServerURL},
				Headers: map[string]SecretRef{"Authorization": testKeyRef},
				OAuth:   &MCPOAuthConfig{Grant: grantAuto},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for oauth + headers.Authorization")
	}
}

func TestValidate_OAuth_ExcludesSSETransport(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {
				URL:       SecretString{Value: testOAuthServerURL},
				Transport: "sse",
				OAuth:     &MCPOAuthConfig{Grant: grantAuto},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for oauth + transport: sse")
	}
}

func TestValidate_OAuth_ValidConfigPasses(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: SecretString{Value: testOAuthServerURL}, OAuth: &MCPOAuthConfig{Grant: "authorization_code"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestOAuthBootWarnings_NonHTTPS(t *testing.T) {
	cfg := &Config{
		Agent: testAgent,
		LLM:   validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			"insecure": {URL: SecretString{Value: "http://example.com/mcp"}, OAuth: &MCPOAuthConfig{Grant: grantAuto}},
			"secure":   {URL: SecretString{Value: testOAuthServerURL}, OAuth: &MCPOAuthConfig{Grant: grantAuto}},
			"noauth":   {URL: SecretString{Value: "http://example.com/mcp"}},
		},
	}

	warnings := cfg.OAuthBootWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
}

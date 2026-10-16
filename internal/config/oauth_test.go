package config

import (
	"os"
	"path/filepath"
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
	cfg := &Config{LLM: validLLMConfig()}
	cfg.applyDefaults()

	if cfg.OAuth.TokenStore != tokenStoreFile {
		t.Errorf("TokenStore = %q, want %q", cfg.OAuth.TokenStore, tokenStoreFile)
	}
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
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: testOAuthServerURL, OAuth: &MCPOAuthConfig{}},
		},
	}
	cfg.applyDefaults()

	if got := cfg.MCPServers[testOAuthServerKey].OAuth.Grant; got != grantAuto {
		t.Errorf("Grant = %q, want %q", got, grantAuto)
	}
}

func TestValidate_OAuth_UnknownGrant(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: testOAuthServerURL, OAuth: &MCPOAuthConfig{Grant: "bogus"}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unknown grant")
	}
}

func TestValidate_OAuth_TokenStoreRejectsKeychain(t *testing.T) {
	cfg := &Config{
		LLM:   validLLMConfig(),
		OAuth: OAuthConfig{TokenStore: "keychain"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for token_store=keychain")
	}
}

func TestValidate_OAuth_TokenStoreRejectsOtherValues(t *testing.T) {
	cfg := &Config{
		LLM:   validLLMConfig(),
		OAuth: OAuthConfig{TokenStore: "vault"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unsupported token_store")
	}
}

func TestValidate_OAuth_BothClientSecretAndFile(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {
				URL: testOAuthServerURL,
				OAuth: &MCPOAuthConfig{
					Grant:            "client_credentials",
					ClientSecret:     "a",
					ClientSecretFile: "/tmp/secret",
				},
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for client_secret + client_secret_file")
	}
}

func TestValidate_OAuth_RedirectPortOutOfRange(t *testing.T) {
	cfg := &Config{
		LLM:   validLLMConfig(),
		OAuth: OAuthConfig{RedirectPort: 70000},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for redirect_port out of range")
	}
}

func TestValidate_OAuth_ExcludesHeaderAuthorization(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {
				URL:     testOAuthServerURL,
				Headers: map[string]string{"Authorization": "Bearer x"},
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
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {
				URL:       testOAuthServerURL,
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
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			testOAuthServerKey: {URL: testOAuthServerURL, OAuth: &MCPOAuthConfig{Grant: "authorization_code"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestOAuthBootWarnings_NonHTTPS(t *testing.T) {
	cfg := &Config{
		LLM: validLLMConfig(),
		MCPServers: map[string]MCPServerConfig{
			"insecure": {URL: "http://example.com/mcp", OAuth: &MCPOAuthConfig{Grant: grantAuto}},
			"secure":   {URL: testOAuthServerURL, OAuth: &MCPOAuthConfig{Grant: grantAuto}},
			"noauth":   {URL: "http://example.com/mcp"},
		},
	}

	warnings := cfg.OAuthBootWarnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
}

func TestMCPOAuthConfig_ResolveClientSecret_Inline(t *testing.T) {
	oa := MCPOAuthConfig{ClientSecret: "inline-secret"}
	got, err := oa.ResolveClientSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "inline-secret" {
		t.Errorf("got %q, want %q", got, "inline-secret")
	}
}

func TestMCPOAuthConfig_ResolveClientSecret_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	oa := MCPOAuthConfig{ClientSecretFile: path}
	got, err := oa.ResolveClientSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "file-secret" {
		t.Errorf("got %q, want %q", got, "file-secret")
	}
}

func TestMCPOAuthConfig_ResolveClientSecret_Empty(t *testing.T) {
	oa := MCPOAuthConfig{}
	got, err := oa.ResolveClientSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

const testAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

func TestSecretSourceLookup(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{RuntimeHome: dir, Agent: AgentConfig{ID: testAgentID}}
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatal(err)
	}
	src := NewSecretSource(store)
	ref := SecretRef{Kind: SecretRefName, Name: "k"}
	ctx := t.Context()

	if _, ok, lerr := src.Lookup(ctx, ref, "llm.providers.p.api_key"); ok || lerr != nil {
		t.Fatalf("before any store: ok=%v err=%v", ok, lerr)
	}
	err = store.Update(ctx, func(p *secretstore.Payload) error {
		p.Secrets["k"] = secretstore.Secret{Value: " v "}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, err := src.Lookup(ctx, ref, "llm.providers.p.api_key"); !ok || err != nil || v != " v " {
		t.Fatalf("got %q ok=%v err=%v; the value must be used exactly as entered", v, ok, err)
	}
	// A present but non-conforming value behaves as pending.
	if _, ok, _ := src.Lookup(ctx, ref, secretstore.FieldZMQAuthToken); ok {
		t.Fatal("short ZMQ secret must be pending")
	}

	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("fromfile\n"), 0o600)
	if v, ok, _ := src.Lookup(ctx, SecretRef{Kind: SecretRefFile, Path: f}, "x"); !ok || v != "fromfile" {
		t.Fatalf("file ref = %q ok=%v", v, ok)
	}
	if _, ok, err := src.Lookup(ctx, SecretRef{Kind: SecretRefFile, Path: f + ".missing"}, "x"); ok || err != nil {
		t.Fatalf("missing file must be pending, not an error: ok=%v err=%v", ok, err)
	}
}

func TestOpenSecretStoreNeedsAgentID(t *testing.T) {
	if _, err := (&Config{RuntimeHome: t.TempDir()}).OpenSecretStore(zerolog.Nop(), true); !errors.Is(err, ErrAgentIDMissing) {
		t.Fatalf("err = %v, want ErrAgentIDMissing", err)
	}
}

func TestValidateAgentAndSecrets(t *testing.T) {
	cfg := func(mut func(*Config)) error {
		c := &Config{RuntimeHome: t.TempDir(), Agent: testAgent}
		c.LLM.Default = testModelRef
		c.LLM.Providers = map[string]LLMProviderConfig{"p": {Provider: ProviderOllama, API: ProviderOllama}}
		mut(c)

		return c.Validate()
	}
	if err := cfg(func(c *Config) { c.Agent = AgentConfig{} }); !errors.Is(err, ErrAgentIDMissing) {
		t.Errorf("missing agent.id = %v, want ErrAgentIDMissing", err)
	}
	if err := cfg(func(c *Config) { c.Agent.ID = "not-a-uuid" }); err == nil {
		t.Error("bad agent.id accepted")
	}
	if err := cfg(func(c *Config) { c.Agent.Name = strings.Repeat("n", 65) }); err == nil {
		t.Error("overlong agent.name accepted")
	}
	if err := cfg(func(c *Config) { c.Secrets.StoreKey = "file" }); err == nil {
		t.Error("store_key: file without store_key_file accepted")
	}
	err := cfg(func(c *Config) {
		c.LLM.Providers["p"] = LLMProviderConfig{
			Provider: ProviderOpenRouter, API: APIChatCompletions,
			APIKey: SecretRef{Kind: SecretRefFile, Path: filepath.Join(c.RuntimeHome, "secrets", "p.key")},
		}
	})
	if err == nil || !strings.Contains(err.Error(), "inside the agent directory") {
		t.Errorf("agent-local !secret_file: %v", err)
	}
	if err := cfg(func(*Config) {}); err != nil {
		t.Errorf("valid identity refused: %v", err)
	}
}

func TestSetStoreKeyLocation(t *testing.T) {
	cases := map[string]struct{ in, loc, file, want string }{
		"append when absent":     {"a: 1\n", "os_vault", "", "a: 1\nsecrets:\n  store_key: os_vault\n"},
		"replace existing":       {"a: 1\nsecrets:\n  store_key: agent_dir\nb: 2\n", "os_vault", "", "a: 1\nsecrets:\n  store_key: os_vault\nb: 2\n"},
		"switch to a file":       {"secrets:\n  store_key: os_vault\n", "file", "/run/k", "secrets:\n  store_key: file\n  store_key_file: /run/k\n"},
		"drop the file when not": {"secrets:\n  store_key: file\n  store_key_file: /run/k\nx: 1\n", "agent_dir", "", "secrets:\n  store_key: agent_dir\nx: 1\n"},
	}
	for name, c := range cases {
		got, err := SetStoreKeyLocation([]byte(c.in), c.loc, c.file)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, c.want)
		}
	}
}

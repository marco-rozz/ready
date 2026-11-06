package config

import (
	"context"
	"testing"

	"github.com/marco-rozz/ready/internal/secretstore"
	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

func decodeSecretString(t *testing.T, src string) SecretString {
	t.Helper()

	var wrapper struct {
		Field SecretString `yaml:"field"`
	}
	if err := yaml.Unmarshal([]byte("field: "+src+"\n"), &wrapper); err != nil {
		t.Fatalf("decode %s: %v", src, err)
	}

	return wrapper.Field
}

// Dynamic per instance: the same field type holds a plain literal (kept in
// clear) or a reference (resolved at boot) — the node tag decides.
func TestSecretString_Decode(t *testing.T) {
	t.Run("plain literal stays clear", func(t *testing.T) {
		s := decodeSecretString(t, `"http://homeassistant.local:8123"`)
		if s.Value != "http://homeassistant.local:8123" || s.IsReference() || s.Pending() {
			t.Fatalf("literal decoded wrong: %+v", s)
		}
		if s.Ref() != nil {
			t.Fatal("literal must not carry a reference")
		}
	})

	t.Run("reference keeps the name, never a value", func(t *testing.T) {
		s := decodeSecretString(t, `!secret mcp_ha_url`)
		if !s.IsReference() || !s.Pending() {
			t.Fatalf("reference decoded wrong: %+v", s)
		}
		if s.Value != "" {
			t.Fatal("the tag content must never become the value")
		}
		if s.Ref() == nil || s.Ref().Name != "mcp_ha_url" {
			t.Fatalf("reference name wrong: %+v", s.Ref())
		}
	})

	t.Run("invalid secret name refused", func(t *testing.T) {
		var wrapper struct {
			Field SecretString `yaml:"field"`
		}
		if err := yaml.Unmarshal([]byte("field: !secret BAD-NAME\n"), &wrapper); err == nil {
			t.Fatal("expected a refusal for a non-conforming secret name")
		}
	})
}

func TestSecretString_Resolve(t *testing.T) {
	dir := t.TempDir()
	const testLiteralURL = "ws://x"
	literalURL := SecretString{Value: testLiteralURL}
	cfg := &Config{
		Agent:       testAgent,
		LLM:         validLLMConfig(),
		RuntimeHome: dir,
		MCPServers: map[string]MCPServerConfig{
			"ha": {
				URL: SecretString{Value: "http://plain.example.com"},
			},
		},
	}
	cfg.applyDefaults()
	// Attach a store-backed source with the referenced value set.
	store, err := cfg.OpenSecretStore(zerolog.Nop(), false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg.SecretValues = NewSecretSource(store)

	t.Run("plain literal resolves to itself", func(t *testing.T) {
		literal := literalURL
		pending, err := literal.Resolve(context.Background(), cfg, "test.literal")
		if err != nil || pending || literal.Value != testLiteralURL {
			t.Fatalf("literal resolve = (%v,%v,%q)", pending, err, literal.Value)
		}
	})

	t.Run("reference resolves from the store", func(t *testing.T) {
		writeTestSecret(t, store, "mcp_ha_url", "ws://secret.example:8123/api/websocket")
		refField := decodeSecretString(t, `!secret mcp_ha_url`)
		pending, err := refField.Resolve(context.Background(), cfg, "test.ref")
		if err != nil || pending {
			t.Fatalf("reference resolve = (%v,%v)", pending, err)
		}
		if refField.Value != "ws://secret.example:8123/api/websocket" || refField.Pending() {
			t.Fatalf("resolved value wrong: %+v", refField)
		}
	})

	t.Run("absent value stays pending", func(t *testing.T) {
		refField := decodeSecretString(t, `!secret absent_url`)
		pending, err := refField.Resolve(context.Background(), cfg, "test.absent")
		if err != nil || !pending {
			t.Fatalf("absent resolve = (%v,%v), want pending", pending, err)
		}
		if refField.Value != "" {
			t.Fatal("a pending field must hold no value")
		}
	})
}

func writeTestSecret(t *testing.T, store *secretstore.Store, name, value string) {
	t.Helper()

	if err := store.Update(context.Background(), func(p *secretstore.Payload) error {
		p.Secrets[name] = secretstore.Secret{Value: value}

		return nil
	}); err != nil {
		t.Fatalf("write secret %s: %v", name, err)
	}
}

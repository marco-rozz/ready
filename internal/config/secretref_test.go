//nolint:goconst // literal fixtures are clearer inline than behind constants
package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type refDoc struct {
	Key  SecretRef            `yaml:"key"`
	Hdrs map[string]SecretRef `yaml:"headers"`
}

func decodeRef(t *testing.T, src string) (refDoc, error) {
	t.Helper()
	var d refDoc
	err := yaml.Unmarshal([]byte(src), &d)

	return d, err
}

func TestSecretRefAcceptedForms(t *testing.T) {
	d, err := decodeRef(t, "key: !secret openai_api_key\n")
	if err != nil || d.Key.Kind != SecretRefName || d.Key.Name != "openai_api_key" {
		t.Fatalf("name ref = %+v, %v", d.Key, err)
	}
	d, err = decodeRef(t, "key: !secret_file /run/credentials/x\n")
	if err != nil || d.Key.Kind != SecretRefFile || d.Key.Path != "/run/credentials/x" {
		t.Fatalf("file ref = %+v, %v", d.Key, err)
	}
	d, err = decodeRef(t, "headers:\n  Authorization: !secret mcp_ha_authorization\n")
	if err != nil || d.Hdrs["Authorization"].Name != "mcp_ha_authorization" {
		t.Fatalf("header ref = %+v, %v", d.Hdrs, err)
	}
}

// A tagged scalar must never decode as its plain value: yaml.v3 would do so
// for a bare string field.
func TestSecretTagNeverDecodesAsValue(t *testing.T) {
	var plain struct {
		Key string `yaml:"key"`
	}
	if err := yaml.Unmarshal([]byte("key: !secret x\n"), &plain); err != nil || plain.Key != "x" {
		t.Skipf("yaml.v3 behaviour changed (%q, %v): the premise of SecretRef no longer holds", plain.Key, err)
	}
	d, err := decodeRef(t, "key: !secret x\n")
	if err != nil {
		t.Fatal(err)
	}
	if d.Key.Name != "x" || d.Key.Kind != SecretRefName {
		t.Fatalf("got %+v; the reference must stay a reference", d.Key)
	}
}

func TestSecretRefRefusals(t *testing.T) {
	const literal = "sk-live-do-not-echo-this"
	cases := map[string]string{
		"untagged literal": "key: " + literal + "\n",
		"quoted literal":   "key: \"" + literal + "\"\n",
		"other tag":        "key: !env " + literal + "\n",
		"bad name":         "key: !secret Not-Valid\n",
		"empty name":       "key: !secret \"\"\n",
		"relative file":    "key: !secret_file relative/" + literal + "\n",
		"empty file":       "key: !secret_file \"\"\n",
		"mapping":          "key:\n  a: " + literal + "\n",
		"sequence":         "key: [" + literal + "]\n",
		"header literal":   "headers:\n  X: " + literal + "\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeRef(t, src)
			if !errors.Is(err, ErrInvalidSecretRef) {
				t.Fatalf("err = %v, want ErrInvalidSecretRef", err)
			}
			// Only the tag-bearing refusal (relative file) may mention the
			// text it was given; a literal value must never appear.
			if name != "relative file" && strings.Contains(err.Error(), literal) {
				t.Fatalf("error echoes the scalar: %v", err)
			}
		})
	}
}

func TestSecretRefRoundTrip(t *testing.T) {
	in := refDoc{Key: SecretRef{Kind: SecretRefName, Name: "a_b"}}
	out, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "key: !secret a_b") {
		t.Fatalf("marshalled = %q", out)
	}
	back, err := decodeRef(t, string(out))
	if err != nil || back.Key != in.Key {
		t.Fatalf("round trip = %+v, %v", back.Key, err)
	}
}

func TestCanonicalSecretName(t *testing.T) {
	cases := []struct {
		path []string
		want string
	}{
		{[]string{"llm", "providers", "ollama-gemma", "api_key"}, "ollama_gemma_api_key"},
		{[]string{"channels", "telegram", "token"}, "telegram_token"},
		{[]string{"channels", "zmqcli", "auth_token"}, "zmqcli_auth_token"},
		{[]string{"home_assistant", "websocket", "token"}, "home_assistant_token"},
		{[]string{"mcp_servers", "ha", "oauth", "client_secret"}, "mcp_ha_client_secret"},
		{[]string{"mcp_servers", "ha", "headers", "X-Api-Key"}, "mcp_ha_x_api_key"},
	}
	for _, c := range cases {
		got, ok := CanonicalSecretName(c.path)
		if !ok || got != c.want || !ValidSecretName(got) {
			t.Errorf("%v = %q, %v; want %q", c.path, got, ok, c.want)
		}
	}
	if _, ok := CanonicalSecretName([]string{"llm", "default"}); ok {
		t.Error("non-sensitive path produced a name")
	}
}

func TestUniqueSecretName(t *testing.T) {
	taken := map[string]bool{"a": true, "a_2": true}
	if got := UniqueSecretName("a", func(s string) bool { return taken[s] }); got != "a_3" {
		t.Fatalf("got %q, want a_3", got)
	}
	if got := UniqueSecretName("b", func(s string) bool { return taken[s] }); got != "b" {
		t.Fatalf("got %q, want b", got)
	}
}

const refConfigHead = `config_version: 2
agent:
  id: 0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001
llm:
  default: p::m
  providers:
    p:
      provider: openrouter
      api: chat_completions
`

func decodeAtTarget(t *testing.T, doc string) (*Config, error) {
	t.Helper()

	return decodeConfig(t.Context(), []byte(doc), filepath.Join(t.TempDir(), ConfigFileName), TargetConfigVersion)
}

func TestLoadRefusesLiteralInSensitiveField(t *testing.T) {
	const literal = "sk-live-super-secret"
	_, err := decodeAtTarget(t, refConfigHead+"      api_key: "+literal+"\n")
	if err == nil {
		t.Fatal("a literal api_key was accepted")
	}
	if strings.Contains(err.Error(), literal) {
		t.Fatalf("error echoes the literal: %v", err)
	}
	_, err = decodeAtTarget(t, refConfigHead+"      api_key: !secret p_api_key\nmcp_servers:\n  s:\n    url: https://x\n    headers:\n      X-Key: "+literal+"\n")
	if err == nil || strings.Contains(err.Error(), literal) {
		t.Fatalf("header literal: %v", err)
	}
}

func TestLoadAcceptsReferences(t *testing.T) {
	cfg, err := decodeAtTarget(t, refConfigHead+"      api_key: !secret p_api_key\n")
	if err != nil {
		t.Fatal(err)
	}
	if err = cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.LLM.Providers["p"].APIKey; got.Name != "p_api_key" {
		t.Fatalf("APIKey = %+v", got)
	}
}

func TestRemovedFileKeysAreRefusedWithGuidance(t *testing.T) {
	cases := map[string]string{
		"llm api_key_file":    refConfigHead + "      api_key: !secret k\n      api_key_file: /x\n",
		"zmq auth_token_file": refConfigHead + "      api_key: !secret k\nchannels:\n  zmqcli:\n    auth_token_file: /x\n",
		"oauth token_store":   refConfigHead + "      api_key: !secret k\noauth:\n  token_store: file\n",
	}
	for name, doc := range cases {
		cfg, err := decodeAtTarget(t, doc)
		if err == nil {
			err = cfg.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), "removed") {
			t.Errorf("%s: err = %v, want a removed-key refusal", name, err)
		}
	}
	_, err := decodeAtTarget(t, refConfigHead+"      api_key: !secret k\n      api_key_file: /x\n")
	cfg, _ := decodeAtTarget(t, refConfigHead+"      api_key: !secret k\n      api_key_file: /x\n")
	if err == nil && cfg != nil {
		if verr := cfg.Validate(); verr == nil || !strings.Contains(verr.Error(), "!secret_file") {
			t.Fatalf("guidance must name !secret_file: %v", verr)
		}
	}
}

func TestLiteralRefusalNamesFieldPathsNeverValues(t *testing.T) {
	const literal = "sk-never-echo-this"
	doc := refConfigHead + "      api_key: " + literal + "\nchannels:\n  telegram:\n    token: " + literal + "\n"
	_, err := decodeAtTarget(t, doc)
	if !errors.Is(err, ErrInvalidSecretRef) {
		t.Fatalf("err = %v", err)
	}
	msg := err.Error()
	for _, field := range []string{"llm.providers.p.api_key", "channels.telegram.token"} {
		if !strings.Contains(msg, field) {
			t.Errorf("refusal must name %s: %s", field, msg)
		}
	}
	if strings.Contains(msg, literal) {
		t.Fatalf("refusal echoes the literal: %s", msg)
	}
}

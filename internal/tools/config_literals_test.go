package tools

import (
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
)

const literalDoc = `config_version: 2
llm:
  providers:
    cloud:
      provider: openrouter
      api_key: %s
channels:
  telegram:
    enabled: false
    token: %s
mcp_servers:
  ha:
    url: http://ha.local
    headers:
      Authorization: %s
    oauth:
      client_secret: !secret mcp_ha_client_secret
`

func build(key, tok, hdr string) string {
	out := literalDoc
	for _, v := range []string{key, tok, hdr} {
		out = strings.Replace(out, "%s", v, 1)
	}

	return out
}

func TestReferencesAreNotLiterals(t *testing.T) {
	t.Parallel()

	data := []byte(build("!secret cloud_api_key", "!secret_file /run/x", "!secret mcp_ha_authorization"))
	n, err := remainingLiterals(data)
	if err != nil || n != 0 {
		t.Fatalf("literals = %d, %v; want none", n, err)
	}
}

func TestMaskLiteralsByPositionAcrossFieldKinds(t *testing.T) {
	t.Parallel()

	data := []byte(build(`"sk-quoted-secret"`, "plain-token-123", "'Bearer abc'"))
	masked, count, err := maskLiterals(data)
	if err != nil || count != 3 {
		t.Fatalf("count = %d, err = %v", count, err)
	}
	for _, leak := range []string{"sk-quoted-secret", "plain-token-123", "Bearer abc"} {
		if strings.Contains(string(masked), leak) {
			t.Fatalf("masked text still holds %q:\n%s", leak, masked)
		}
	}
	if strings.Count(string(masked), maskText) != 3 {
		t.Fatalf("expected three masks:\n%s", masked)
	}
	// Non-literal neighbours are untouched.
	if !strings.Contains(string(masked), "client_secret: !secret mcp_ha_client_secret") {
		t.Fatalf("reference was altered:\n%s", masked)
	}
	// Masking is idempotent and the mask is not a "new" literal.
	again, _, err := maskLiterals(masked)
	if err != nil || string(again) != string(masked) {
		t.Fatalf("masking must be idempotent: %v", err)
	}
	if paths, _ := newLiterals(masked); len(paths) != 0 {
		t.Fatalf("the mask must not count as a new literal: %v", paths)
	}
	if n, _ := remainingLiterals(masked); n != 3 {
		t.Fatalf("a save must still see %d remaining literals", n)
	}
}

func TestMultilineLiteralRefusesRead(t *testing.T) {
	t.Parallel()

	doc := strings.Replace(build("!secret k", "!secret t", "!secret h"), "api_key: !secret k", "api_key: |\n        line-one\n        line-two", 1)
	if _, _, err := maskLiterals([]byte(doc)); err == nil || !strings.Contains(err.Error(), "ready secrets adopt") {
		t.Fatalf("a block literal must refuse with adopt guidance, got: %v", err)
	}
}

func TestSubstituteLiteralsKeepsDocumentDecodable(t *testing.T) {
	t.Parallel()

	masked, _, _ := maskLiterals([]byte(build("sk-x", "tok-y", "hdr-z")))
	sub, err := substituteLiterals(masked)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := remainingLiterals(sub); n != 0 {
		t.Fatalf("substitution left %d literals", n)
	}
	if strings.Contains(string(sub), "sk-x") {
		t.Fatal("substitution leaked a value")
	}
}

func TestLiteralRefusalNamesCanonicalReferenceAndNeverTheValue(t *testing.T) {
	t.Parallel()

	paths, err := newLiterals([]byte(build("sk-do-not-echo", "!secret t", "!secret h")))
	if err != nil || len(paths) != 1 || paths[0] != "llm.providers.cloud.api_key" {
		t.Fatalf("paths = %v, %v", paths, err)
	}
	msg := literalRefusal(paths).Error()
	if !strings.Contains(msg, "!secret cloud_api_key") || strings.Contains(msg, "sk-do-not-echo") {
		t.Fatalf("refusal = %q", msg)
	}
}

func TestScalarAtAndReferenceChanged(t *testing.T) {
	t.Parallel()

	a := []byte("agent:\n  id: one\nchannels:\n  zmqcli:\n    auth_token: !secret a\n")
	b := []byte("agent:\n  id: one\nchannels:\n  zmqcli:\n    auth_token: !secret b\n")
	if referenceChanged(a, a, "agent", "id") || !referenceChanged(a, b, "channels", "zmqcli", "auth_token") {
		t.Fatal("referenceChanged misjudged")
	}
	if tag, v, ok := scalarAt(a, "channels", "zmqcli", "auth_token"); !ok || tag != config.TagSecret || v != "a" {
		t.Fatalf("scalarAt = %q %q %v", tag, v, ok)
	}
}

func TestAdoptLiteralsRewritesToReferencesAndSharesEqualValues(t *testing.T) {
	t.Parallel()

	data := []byte(build(`"sk-quoted"`, "same-value", "same-value"))
	next, items, err := AdoptLiterals(data, func(p string) string {
		c, _ := config.CanonicalSecretName(strings.Split(p, "."))

		return c
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := remainingLiterals(next); n != 0 {
		t.Fatalf("literals left after adopt: %d\n%s", n, next)
	}
	for _, leak := range []string{"sk-quoted", "same-value"} {
		if strings.Contains(string(next), leak) {
			t.Fatalf("rewritten document still holds %q", leak)
		}
	}
	if len(items) != 3 || items[0].Path != "llm.providers.cloud.api_key" || items[0].Value != "sk-quoted" {
		t.Fatalf("items = %+v", items)
	}
	if items[1].Name != items[2].Name {
		t.Fatalf("equal values must share one name: %q vs %q", items[1].Name, items[2].Name)
	}
}

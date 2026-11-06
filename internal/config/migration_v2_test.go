//nolint:goconst // literal fixtures are clearer inline than behind constants
package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

const v1Config = `config_version: 1
llm:
  default: or::model
  providers:
    or:
      provider: openrouter
      api: chat_completions
      api_key_file: secrets/or.key
channels:
  zmqcli:
    enabled: true
    bind: tcp://127.0.0.1:7600
    auth_token: %ZMQ%
  telegram:
    enabled: false
    token: ""
home_assistant:
  enabled: false
  websocket:
    url: ws://ha
    token_file: %HAFILE%
mcp_servers:
  s:
    url: https://example.com/mcp
    headers:
      X-Api-Key: header-secret
    oauth:
      grant: client_credentials
      client_id: cid
      client_secret: oauth-secret
oauth:
  token_store: file
`

type v1Agent struct {
	dir, path string
	zmq       string
	haFile    string
}

func newV1Agent(t *testing.T) v1Agent {
	t.Helper()
	dir := t.TempDir()
	external := filepath.Join(t.TempDir(), "ha.token")
	if err := os.WriteFile(external, []byte("ha-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets", "or.key"), []byte("sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := v1Agent{dir: dir, path: filepath.Join(dir, ConfigFileName), zmq: strings.Repeat("z", 40), haFile: external}
	body := strings.NewReplacer("%ZMQ%", a.zmq, "%HAFILE%", external).Replace(v1Config)
	if err := os.WriteFile(a.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// An earlier plaintext backup written by the old code.
	old := filepath.Join(dir, ConfigFileName+".bak.20260101T000000.000000000Z.123456")
	if err := os.WriteFile(old, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return a
}

func (a v1Agent) store(t *testing.T) *secretstore.Store {
	t.Helper()
	s, err := secretstore.New(secretstore.Options{Dir: a.dir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func TestMigrationV2ImportsSecretsAndCleansUp(t *testing.T) {
	a := newV1Agent(t)
	r, err := ProductionMigrations()
	if err != nil {
		t.Fatal(err)
	}
	cfg, report, err := r.Prepare(context.Background(), a.path, nil)
	if err != nil || !report.Committed {
		t.Fatalf("prepare = %+v, %v", report, err)
	}
	if cfg.ConfigVersion != 2 || cfg.Agent.ID == "" {
		t.Fatalf("migrated config = %+v", cfg.Agent)
	}

	// The document holds references only.
	body := readFile(t, a.path)
	for _, leak := range []string{a.zmq, "header-secret", "oauth-secret", "sk-from-file", "token_store", "_file:", "ha-token"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the migrated config still holds %q:\n%s", leak, body)
		}
	}
	for _, want := range []string{
		"api_key: !secret or_api_key", "auth_token: !secret zmqcli_auth_token", "X-Api-Key: !secret mcp_s_x_api_key",
		"client_secret: !secret mcp_s_client_secret", "token: !secret_file " + a.haFile, "agent:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `token: ""`) {
		t.Error("an empty token must simply disappear")
	}

	// The store has every internal value, exactly (one trailing newline stripped).
	p, err := a.store(t).View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"or_api_key": "sk-from-file", "zmqcli_auth_token": a.zmq, "mcp_s_x_api_key": "header-secret", "mcp_s_client_secret": "oauth-secret"}
	for name, v := range want {
		if p.Secrets[name].Value != v {
			t.Errorf("secret %s = %q, want %q", name, p.Secrets[name].Value, v)
		}
	}
	if _, ok := p.Secrets["home_assistant_token"]; ok {
		t.Error("an external file must be referenced, never imported")
	}
	if p.AgentID != cfg.Agent.ID {
		t.Fatal("the store is bound to a different agent id than the config")
	}

	// Backups: one encrypted, no plaintext; imported files gone; external untouched.
	entries, _ := os.ReadDir(a.dir)
	var sealed string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			if !strings.HasSuffix(e.Name(), ".age") {
				t.Errorf("plaintext backup survived: %s", e.Name())
			}
			sealed = filepath.Join(a.dir, e.Name())
		}
	}
	if sealed == "" {
		t.Fatal("no encrypted backup was written")
	}
	raw := readFile(t, sealed)
	if strings.Contains(raw, a.zmq) {
		t.Fatal("the encrypted backup holds a plaintext secret")
	}
	keyText := readFile(t, filepath.Join(a.dir, secretstore.KeyFileName))
	id, err := age.ParseX25519Identity(strings.TrimSpace(keyText))
	if err != nil {
		t.Fatal(err)
	}
	dec, err := age.Decrypt(bytes.NewReader([]byte(raw)), id)
	if err != nil {
		t.Fatalf("the backup does not decrypt with the store key: %v", err)
	}
	var plain bytes.Buffer
	_, _ = plain.ReadFrom(dec)
	if !strings.Contains(plain.String(), "api_key_file: secrets/or.key") {
		t.Fatal("the backup is not the original v1 document")
	}
	if _, err = os.Stat(filepath.Join(a.dir, "secrets", "or.key")); err == nil {
		t.Fatal("the imported agent-local key file was not deleted")
	}
	if _, err = os.Stat(a.haFile); err != nil {
		t.Fatal("the external token file must never be touched")
	}
	if len(p.Cleanup) != 0 {
		t.Fatalf("cleanup list not drained: %v", p.Cleanup)
	}
}

func TestMigrationV2RefusesRedactedMarkersWithoutEchoingValues(t *testing.T) {
	a := newV1Agent(t)
	body := strings.Replace(readFile(t, a.path), "header-secret", "REDACTED-TODO-Pabc", 1)
	if err := os.WriteFile(a.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, _ := ProductionMigrations()
	_, report, err := r.Prepare(context.Background(), a.path, nil)
	if err == nil || report.Committed || !errors.Is(err, ErrSecretImport) {
		t.Fatalf("a marker must refuse the migration: %+v, %v", report, err)
	}
	if !strings.Contains(err.Error(), "X-Api-Key") || strings.Contains(err.Error(), "REDACTED-TODO-Pabc") {
		t.Fatalf("refusal must name the path, not the marker: %v", err)
	}
	if readFile(t, a.path) != body {
		t.Fatal("a refused migration changed the config")
	}
	if _, serr := os.Stat(filepath.Join(a.dir, secretstore.StoreFileName)); serr == nil {
		t.Fatal("a refused migration created a store")
	}
}

func TestMigrationV2RefusesAValueThatBreaksARule(t *testing.T) {
	a := newV1Agent(t)
	body := strings.Replace(readFile(t, a.path), a.zmq, "short", 1)
	_ = os.WriteFile(a.path, []byte(body), 0o600)
	r, _ := ProductionMigrations()
	if _, _, err := r.Prepare(context.Background(), a.path, nil); err == nil || !strings.Contains(err.Error(), "too_short") {
		t.Fatalf("a short ZMQ secret must refuse with its rule, got %v", err)
	}
	if readFile(t, a.path) != body {
		t.Fatal("a refused migration changed the config")
	}
}

// A crash after the store write but before the config commit leaves v1 intact;
// the retry converges and mints no second identity.
func TestMigrationV2RetryAfterAStoreWriteReusesTheIdentity(t *testing.T) {
	a := newV1Agent(t)
	r, _ := ProductionMigrations()
	ctx, cancel := context.WithCancel(context.Background())
	failing := migrationIO{write: func(c context.Context, _, _ string, _ []byte, _ os.FileMode) (string, error) {
		cancel() // the commit step "crashes" after the import phase

		return "", c.Err()
	}}
	_, report, err := r.prepare(ctx, a.path, nil, failing)
	if err == nil || report.Committed {
		t.Fatalf("the injected failure must stop before the commit: %+v, %v", report, err)
	}
	if !strings.Contains(readFile(t, a.path), "api_key_file: secrets/or.key") {
		t.Fatal("v1 must be intact after a crash before the commit")
	}
	if _, serr := os.Stat(filepath.Join(a.dir, "secrets", "or.key")); serr != nil {
		t.Fatal("an imported file was deleted before the commit")
	}
	first, err := a.store(t).View(context.Background())
	if err != nil {
		t.Fatalf("the store written before the crash must open: %v", err)
	}

	cfg, report, err := r.Prepare(context.Background(), a.path, nil)
	if err != nil || !report.Committed {
		t.Fatalf("retry = %+v, %v", report, err)
	}
	if cfg.Agent.ID != first.AgentID {
		t.Fatalf("the retry minted a second identity: %q vs %q", cfg.Agent.ID, first.AgentID)
	}
}

func TestSecretCleanupRetriesAndKeepsFailedPaths(t *testing.T) {
	dir := t.TempDir()
	s, _ := secretstore.New(secretstore.Options{Dir: dir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, AgentID: testAgentID, Log: zerolog.Nop()})
	gone := filepath.Join(dir, "gone.key")
	missing := filepath.Join(dir, "never-existed")
	_ = os.WriteFile(gone, []byte("x"), 0o600)
	_ = s.Update(context.Background(), func(p *secretstore.Payload) error {
		p.Cleanup = []string{gone, missing}

		return nil
	})
	RunSecretCleanup(context.Background(), s, filepath.Join(dir, ConfigFileName), zerolog.Nop())
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("a recorded file survived cleanup")
	}
	p, _ := s.View(context.Background())
	if len(p.Cleanup) != 0 {
		t.Fatalf("done paths must leave the list: %v", p.Cleanup)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

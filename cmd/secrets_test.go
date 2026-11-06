//nolint:goconst // literal fixtures are clearer inline than behind constants
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/marco-rozz/ready/internal/secretstore"
)

const fixtureAgentConfig = "config_version: 2\nllm:\n  default: openrouter::m\n  providers:\n    openrouter:\n      provider: openrouter\n      api: chat_completions\n      api_key: !secret openrouter_api_key\nchannels:\n  zmqcli:\n    enabled: true\n    bind: tcp://127.0.0.1:7600\n    auth_token: !secret zmqcli_auth_token\n"

type secretsRun struct {
	dir            string
	stdout, stderr bytes.Buffer
	stdin          string
	tty            bool
	pass           []string
}

func newSecretsRun(t *testing.T, body string) *secretsRun {
	t.Helper()

	return &secretsRun{dir: writeTempAgentDir(t, body)}
}

func (r *secretsRun) do(t *testing.T, args ...string) error {
	t.Helper()
	r.stdout.Reset()
	r.stderr.Reset()
	passes := append([]string(nil), r.pass...)
	sio := secretsIO{
		stdin: strings.NewReader(r.stdin), stdout: &r.stdout, stderr: &r.stderr, stdinTTY: r.tty, stdoutTTY: r.tty,
		readPass: func(string) (string, error) {
			if len(passes) == 0 {
				return "", os.ErrInvalid
			}
			v := passes[0]
			passes = passes[1:]

			return v, nil
		},
	}
	root := &cli.Command{Name: homeAgent, Commands: []*cli.Command{newSecretsCommandIO(sio)}, ExitErrHandler: func(context.Context, *cli.Command, error) {}}

	return root.Run(context.Background(), append([]string{"ready", "secrets", args[0], flagAgentDir, r.dir}, args[1:]...))
}

func (r *secretsRun) value(t *testing.T, name string) string {
	t.Helper()
	store := testSecretStore(t, r.dir)
	p, err := store.View(t.Context())
	if err != nil {
		t.Fatalf("view: %v", err)
	}

	return p.Secrets[name].Value
}

func TestSecretsSetListNeverShowsValues(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "sk-super-secret\n"
	if err := r.do(t, "set", "openrouter_api_key", "--stdin"); err != nil {
		t.Fatalf("set: %v (%s)", err, r.stderr.String())
	}
	if got := r.value(t, "openrouter_api_key"); got != "sk-super-secret" {
		t.Fatalf("stored = %q (one trailing newline is removed, nothing else)", got)
	}
	if err := r.do(t, "list"); err != nil {
		t.Fatal(err)
	}
	out := r.stdout.String()
	if strings.Contains(out, "sk-super-secret") {
		t.Fatalf("list leaked a value:\n%s", out)
	}
	for _, want := range []string{"openrouter_api_key", "set", "zmqcli_auth_token", "pending", "llm.providers.openrouter.api_key", "0190aaaa"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
}

func TestSecretsSetAppliesValueRulesAndHiddenPrompt(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "too-short\n"
	if err := r.do(t, "set", "zmqcli_auth_token", "--stdin"); err == nil || !strings.Contains(r.stderr.String(), "too_short") {
		t.Fatalf("a short ZMQ secret must be refused with its rule, err=%v stderr=%q", err, r.stderr.String())
	}
	if strings.Contains(r.stderr.String(), "too-short") {
		t.Fatal("the refusal echoes the value")
	}
	r.tty, r.pass = true, []string{strings.Repeat("z", 40), strings.Repeat("z", 40)}
	if err := r.do(t, "set", "zmqcli_auth_token"); err != nil {
		t.Fatalf("hidden prompt: %v", err)
	}
	r.pass = []string{"one", "two"}
	if err := r.do(t, "set", "openrouter_api_key"); err == nil {
		t.Fatal("differing entries must be refused")
	}
}

func TestSecretsRevealOnlyToPrivateFileOrTerminal(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "reveal-me\n"
	_ = r.do(t, "set", "openrouter_api_key", "--stdin")

	if err := r.do(t, "reveal", "openrouter_api_key"); err == nil {
		t.Fatal("reveal printed to a non-terminal stdout")
	}
	out := filepath.Join(t.TempDir(), "k")
	if err := r.do(t, "reveal", "openrouter_api_key", "--to-file", out); err != nil {
		t.Fatalf("reveal --to-file: %v (%s)", err, r.stderr.String())
	}
	if got, _ := os.ReadFile(out); string(got) != "reveal-me" {
		t.Fatalf("file = %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
	}
	if err := r.do(t, "reveal", "openrouter_api_key", "--to-file", out); err == nil {
		t.Fatal("reveal overwrote an existing file")
	}
	if strings.Contains(r.stdout.String(), "reveal-me") {
		t.Fatal("reveal --to-file printed the value")
	}
	r.tty = true
	if err := r.do(t, "reveal", "openrouter_api_key"); err != nil || !strings.Contains(r.stdout.String(), "reveal-me") {
		t.Fatalf("a terminal may show it: %v", err)
	}
}

func TestSecretsRemoveRefusesReferencedUnlessForced(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "v\n"
	_ = r.do(t, "set", "openrouter_api_key", "--stdin")
	r.stdin = "v\n"
	_ = r.do(t, "set", "unused_one", "--stdin")
	if err := r.do(t, "rm", "openrouter_api_key"); err == nil {
		t.Fatal("a referenced secret was removed")
	}
	if err := r.do(t, "rm", "unused_one"); err != nil {
		t.Fatal(err)
	}
	if err := r.do(t, "rm", "openrouter_api_key", "--force"); err != nil {
		t.Fatal(err)
	}
	if r.value(t, "openrouter_api_key") != "" || r.value(t, "unused_one") != "" {
		t.Fatal("secrets survived removal")
	}
}

func TestSecretsAdoptMovesLiteralsIntoTheStore(t *testing.T) {
	body := strings.Replace(fixtureAgentConfig, "!secret openrouter_api_key", "sk-hand-edited", 1)
	r := newSecretsRun(t, body)
	if err := r.do(t, "adopt"); err != nil {
		t.Fatalf("adopt: %v (%s)", err, r.stderr.String())
	}
	cfg := readFileOrFailCmd(t, filepath.Join(r.dir, "config.yaml"))
	if strings.Contains(cfg, "sk-hand-edited") || !strings.Contains(cfg, "api_key: !secret openrouter_api_key") {
		t.Fatalf("config after adopt:\n%s", cfg)
	}
	if r.value(t, "openrouter_api_key") != "sk-hand-edited" {
		t.Fatal("the literal was not stored")
	}
	entries, _ := os.ReadDir(r.dir)
	sawBackup := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			sawBackup = true
			if !strings.HasSuffix(e.Name(), ".age") {
				t.Fatalf("adopt left a plaintext backup of the literal: %s", e.Name())
			}
		}
	}
	if !sawBackup {
		t.Fatal("adopt must keep an (encrypted) backup of the previous file")
	}
	if strings.Contains(r.stdout.String(), "sk-hand-edited") {
		t.Fatal("adopt printed the value")
	}
	if err := r.do(t, "adopt"); err != nil || !strings.Contains(r.stdout.String(), "nothing to adopt") {
		t.Fatalf("a second adopt must be a no-op: %v", err)
	}
}

func TestSecretsExportImportRoundTrip(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "precious\n"
	_ = r.do(t, "set", "openrouter_api_key", "--stdin")
	export := filepath.Join(t.TempDir(), "key.age")
	r.tty, r.pass = true, []string{"pass phrase", "pass phrase"}
	if err := r.do(t, "export-key", "--to", export); err != nil {
		t.Fatalf("export: %v (%s)", err, r.stderr.String())
	}
	raw := readFileOrFailCmd(t, export)
	if strings.Contains(raw, "AGE-SECRET-KEY") {
		t.Fatal("the export holds the key in plaintext")
	}
	if err := os.Remove(filepath.Join(r.dir, secretstore.KeyFileName)); err != nil {
		t.Fatal(err)
	}
	if err := r.do(t, "list"); err == nil {
		t.Fatal("a missing key must be an error, never a new key")
	}
	r.pass = []string{"wrong"}
	if err := r.do(t, "import-key", export); err == nil {
		t.Fatal("a wrong passphrase imported a key")
	}
	r.pass = []string{"pass phrase"}
	if err := r.do(t, "import-key", export); err != nil {
		t.Fatalf("import: %v (%s)", err, r.stderr.String())
	}
	if r.value(t, "openrouter_api_key") != "precious" {
		t.Fatal("the store does not open after import")
	}
}

func TestSecretsMoveKeyToExternalFileAndBack(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "v\n"
	_ = r.do(t, "set", "openrouter_api_key", "--stdin")
	ext := filepath.Join(t.TempDir(), "ext.key")
	if err := r.do(t, "move-key", "--to", "file", "--path", ext); err != nil {
		t.Fatalf("move to file: %v (%s)", err, r.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(r.dir, secretstore.KeyFileName)); err == nil {
		t.Fatal("the old agent_dir key copy survived")
	}
	cfg := readFileOrFailCmd(t, filepath.Join(r.dir, "config.yaml"))
	if !strings.Contains(cfg, "store_key: file") || !strings.Contains(cfg, ext) {
		t.Fatalf("config not updated:\n%s", cfg)
	}
	if err := r.do(t, "list"); err != nil {
		t.Fatalf("the store must open through the external file: %v (%s)", err, r.stderr.String())
	}
	if err := r.do(t, "move-key", "--to", "agent_dir"); err != nil {
		t.Fatalf("move back: %v (%s)", err, r.stderr.String())
	}
	if _, err := os.Stat(ext); err != nil {
		t.Fatal("an external key file must never be deleted")
	}
	if r.value(t, "openrouter_api_key") != "v" {
		t.Fatal("the store does not open after moving back")
	}
}

func TestSecretsForgetKeyWithoutAVaultEntryChangesNothing(t *testing.T) {
	r := newSecretsRun(t, fixtureAgentConfig)
	r.stdin = "v\n"
	_ = r.do(t, "set", "openrouter_api_key", "--stdin")
	if err := r.do(t, "forget-key", "--yes"); err != nil || !strings.Contains(r.stdout.String(), "no vault entry") {
		t.Fatalf("forget-key on an agent_dir key: %v / %q", err, r.stdout.String())
	}
	if _, err := os.Stat(filepath.Join(r.dir, secretstore.KeyFileName)); err != nil {
		t.Fatal("forget-key deleted an agent-directory key")
	}
}

func readFileOrFailCmd(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

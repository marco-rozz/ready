// Tests for the guided-init wizard: cwd independence, doctor-aliased
// directories, per-provider outputs, exact generated policies, non-TTY and
// echo contracts, no-clobber publication, permissions, repair flows and
// verification declines (tasks A.13–A.15).
package initwizard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// testProviderVocabulary names the interview answers used across tests
// (naming satisfies goconst's literal-repetition rule).
const (
	testOllama             = "ollama"
	testOpenCode           = "opencode"
	testProviderOpenRouter = "openrouter"
)

// fixedRandomHex is the injected deterministic entropy stub of every test.
func fixedRandomHex(n int) (string, error) {
	out := make([]byte, 0, n*2)
	for len(out) < n*2 {
		out = append(out, 'a'+byte(len(out)%6)) // deterministic hex letters
	}

	return string(out[:n*2]), nil
}

// baseOptions wires the interview streams (stdin is assumed non-TTY so the
// TTY-specific reader paths are replaced in tests by piped streams — but
// the TTY gate refuses; fresh-init tests set StdinTTY and drive prompts
// through Stdin).
func baseOptions(stdin string, stdinTTY bool, out *strings.Builder) Options {
	return Options{
		Stdin:          strings.NewReader(stdin),
		Stdout:         out,
		Stderr:         out,
		StdinTTY:       stdinTTY,
		RandomHex:      fixedRandomHex,
		ReaderOverride: newPlainReader(strings.NewReader(stdin)),
	}
}

// freshInterviewAnswer is the full deterministic answer sequence for an
// ollama provider (provider, Enter on local deployment, endpoint and model).
const freshInterviewAnswer = "ollama\n\n\n\n"

func TestRun_FreshCreatesMinimalConfig(t *testing.T) {
	agentDir := t.TempDir() + "/agent必须" // aliasing stays a selection layer concern
	opts := baseOptions(freshInterviewAnswer, true, &strings.Builder{})
	opts.ExplicitAgentDir = agentDir
	opts.ReadyGuideCheck = func() bool { return true }

	outcome, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !outcome.Committed {
		t.Fatal("fresh init must report a committed config")
	}

	data, err := os.ReadFile(filepath.Join(agentDir, "config.yaml"))
	if err != nil {
		t.Fatalf("config.yaml missing: %v", err)
	}

	for _, want := range []string{
		fmt.Sprintf("config_version: %d", config.TargetConfigVersion),
		"agent:\n  id: ",
		"default: ollama::gemma4:31b",
		"provider: ollama",
		"api: ollama",
		"base_url: http://127.0.0.1:11434",
		"enabled: true",
		"bind: tcp://127.0.0.1:7600",
		"auth_token: !secret zmqcli_auth_token",
		"default_profile: default",
		"deny: [config-manager]",
		"admin:",
		"inject_skills: [ready-guide]",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("generated config missing %q:\n%s", want, data)
		}
	}

	// The chat secret lives in the store, never in YAML.
	stored := storedSecret(t, agentDir, "zmqcli_auth_token")
	if len(stored) != generateZMQTokenByteLength*2 || strings.Contains(string(data), stored) {
		t.Errorf("chat secret must be generated into the store only (len=%d)", len(stored))
	}
	for _, leftover := range []string{"zmqcli.token", "secrets"} {
		if _, statErr := os.Stat(filepath.Join(agentDir, leftover)); statErr == nil {
			t.Errorf("init must not create %s", leftover)
		}
	}
}

func TestRun_NonTTYFailsBeforeFreshPublication(t *testing.T) {
	agentDir := t.TempDir() + "/agent-ttyless"
	opts := baseOptions(freshInterviewAnswer, false, &strings.Builder{})
	opts.ExplicitAgentDir = agentDir

	outcome, err := Run(context.Background(), opts)
	if err == nil || !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Run on piped stdin = %v, want ErrNotTerminal", err)
	}
	if outcome.Committed {
		t.Fatal("nothing may be committed on a TTY refusal")
	}
	if _, statErr := os.Stat(agentDir); !os.IsNotExist(statErr) {
		t.Fatalf("agent dir exists after TTY refusal: %v", statErr)
	}
}

func TestRun_EOFMidInterviewKeepsStateClean(t *testing.T) {
	agentDir := t.TempDir() + "/agent-eof"
	opts := baseOptions("", true, &strings.Builder{}) // immediate EOF
	opts.ExplicitAgentDir = agentDir

	outcome, err := Run(context.Background(), opts)
	if err == nil || !errors.Is(err, ErrEOF) {
		t.Fatalf("Run = %v, want ErrEOF", err)
	}
	if outcome.Committed {
		t.Fatal("EOF mid-interview must commit nothing")
	}
	if _, statErr := os.Stat(filepath.Join(agentDir, "config.yaml")); !os.IsNotExist(statErr) {
		t.Fatal("config must not exist after an EOF interview")
	}
}

// A fresh init never clobbers an existing config: the existing-config
// branch is the bounded repair menu, and a fresh refusal is impossible.
func TestRun_ExistingConfigEntersRepairNotFresh(t *testing.T) {
	agentDir := t.TempDir() + "/agent-repair"
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}

	seeded := readSeedConfig(t)
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte(seeded), 0o600); err != nil {
		t.Fatal(err)
	}

	// Everything empty → EOF during the menu; the config must survive.
	opts := baseOptions("", true, &strings.Builder{})
	opts.ExplicitAgentDir = agentDir

	outcome, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected EOF mid-menu to abort")
	}
	if outcome.Committed {
		t.Fatal("a declined/EOF repair must report an uncommitted state")
	}
	if got := readFileOrFail(t, filepath.Join(agentDir, "config.yaml")); got != seeded {
		t.Fatalf("existing config was modified:\n%s", got)
	}
}

// readSeedConfig is a minimal but fully valid current-schema config used as
// the repair seat.
func readSeedConfig(t *testing.T) string {
	t.Helper()

	return fmt.Sprintf("config_version: %d\nagent:\n  id: %s\n  name: test\nllm:\n  default: ollama::gemma4:31b\n  providers:\n    ollama:\n      provider: ollama\n      api: ollama\n      base_url: http://127.0.0.1:11434\nchannels:\n  zmqcli:\n    enabled: false\nskills: {}\n", config.TargetConfigVersion, testAgentID)
}

// testAgentID is the identity of every fixture agent.
const testAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

// openStore opens the Secret store of a fixture agent directory.
func openStore(t *testing.T, agentDir string) *secretstore.Store {
	t.Helper()
	store, err := secretstore.New(secretstore.Options{
		Dir: agentDir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, Log: zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}

	return store
}

// storedSecret reads one Secret value of a fixture agent.
func storedSecret(t *testing.T, agentDir, name string) string {
	t.Helper()
	p, err := openStore(t, agentDir).View(t.Context())
	if err != nil {
		t.Fatalf("view store: %v", err)
	}

	return p.Secrets[name].Value
}

func readFileOrFail(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(data)
}

// The invalid inputs rule: an opencode answer of an unsupported API is
// rejected by the interview validation without any publication.
func TestValidateProviderChoice_RejectsUnsupportedAPI(t *testing.T) {
	choice := ProviderChoice{Provider: config.ProviderOpenCode, Instance: testOpenCode, API: "gemini", BaseURL: "https://opencode.ai/zen/v1", ModelID: "claude-x", APIKey: "k"}
	if err := validateProviderChoice(choice); err == nil {
		t.Fatal("expected an unsupported opencode API to be rejected")
	}
}

func TestRun_FreshStoresCredentialsEncrypted(t *testing.T) {
	agentDir := t.TempDir() + "/agent"
	opts := baseOptions("openrouter\n\nmy-model\nsuper-secret-key\n", true, &strings.Builder{})
	opts.ExplicitAgentDir = agentDir
	opts.ReadyGuideCheck = func() bool { return true }

	outcome, err := Run(context.Background(), opts)
	if err != nil || !outcome.Committed {
		t.Fatalf("Run: committed=%v err=%v", outcome.Committed, err)
	}
	if got := storedSecret(t, agentDir, "openrouter_api_key"); got != "super-secret-key" {
		t.Fatalf("provider key not stored exactly (len=%d)", len(got))
	}
	cfgData := readFileOrFail(t, filepath.Join(agentDir, "config.yaml"))
	if !strings.Contains(cfgData, "api_key: !secret openrouter_api_key") || strings.Contains(cfgData, "super-secret-key") {
		t.Errorf("config must hold the reference only:\n%s", cfgData)
	}
	raw := readFileOrFail(t, filepath.Join(agentDir, secretstore.StoreFileName))
	if strings.Contains(raw, "super-secret-key") {
		t.Fatal("the store holds the key in plaintext")
	}
	for _, f := range []string{secretstore.StoreFileName, secretstore.KeyFileName} {
		if _, statErr := os.Stat(filepath.Join(agentDir, f)); statErr != nil {
			t.Errorf("%s missing: %v", f, statErr)
		}
	}
}

// A retried init over an orphan store keeps its identity and key.
func TestRun_FreshReusesAnOrphanStore(t *testing.T) {
	agentDir := t.TempDir() + "/agent"
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan, err := secretstore.New(secretstore.Options{Dir: agentDir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, AgentID: testAgentID, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	if err = orphan.Update(t.Context(), func(p *secretstore.Payload) error {
		p.Secrets["keep"] = secretstore.Secret{Value: "v"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	keyBefore := readFileOrFail(t, filepath.Join(agentDir, secretstore.KeyFileName))

	opts := baseOptions(freshInterviewAnswer, true, &strings.Builder{})
	opts.ExplicitAgentDir = agentDir
	opts.ReadyGuideCheck = func() bool { return true }
	if outcome, runErr := Run(context.Background(), opts); runErr != nil || !outcome.Committed {
		t.Fatalf("Run: committed=%v err=%v", outcome.Committed, runErr)
	}
	if readFileOrFail(t, filepath.Join(agentDir, secretstore.KeyFileName)) != keyBefore {
		t.Fatal("init replaced the key of an existing store")
	}
	if !strings.Contains(readFileOrFail(t, filepath.Join(agentDir, "config.yaml")), testAgentID) {
		t.Fatal("the config must reuse the store's agent id")
	}
	if storedSecret(t, agentDir, "keep") != "v" {
		t.Fatal("existing secrets were lost")
	}
}

// The generated config is a valid current-schema config for every provider.
func TestValidateCandidate_AcceptsGeneratedConfigs(t *testing.T) {
	cases := map[string]ProviderChoice{
		testOllama + ":2": {Provider: config.ProviderOllama, Instance: testOllama, API: testOllama, BaseURL: "http://127.0.0.1:11434", ModelID: "gemma4:31b"},
		"openrouter":      {Provider: config.ProviderOpenRouter, Instance: "openrouter", API: "chat_completions", BaseURL: "https://openrouter.ai/api/v1", ModelID: "google/gemma-3-27b-it", APIKey: "sk-test"},
		"opencode":        {Provider: config.ProviderOpenCode, Instance: testOpenCode, API: "anthropic", BaseURL: "https://opencode.ai/zen/v1", ModelID: "claude-x", APIKey: "oc-test"},
		"codex":           {Provider: config.ProviderOpenAICodex, Instance: "openai-codex", API: "responses", BaseURL: "", ModelID: "gpt-5.2-codex"},
	}
	for name, choice := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateProviderChoice(choice); err != nil {
				t.Fatalf("validateProviderChoice: %v", err)
			}
			candidate := generateConfig(choice, testAgentID, "host", true)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if _, err := config.ValidateCandidate(context.Background(), candidate, path, config.ProfileOverrideInputs{}, nil); err != nil {
				t.Fatalf("ValidateCandidate rejected:\n%s\nerr: %v", candidate, err)
			}
		})
	}
}

func TestNewIdentityRewritesStoreThenConfig(t *testing.T) {
	var out strings.Builder
	opts := baseOptions(freshInterviewAnswer, true, &out)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	opts.ReadyGuideCheck = func() bool { return true }
	if o, err := Run(context.Background(), opts); err != nil || !o.Committed {
		t.Fatalf("setup: %v", err)
	}
	before := readFileOrFail(t, filepath.Join(opts.ExplicitAgentDir, "config.yaml"))
	zmqBefore := storedSecret(t, opts.ExplicitAgentDir, "zmqcli_auth_token")

	again := baseOptions("", false, &out)
	again.ExplicitAgentDir, again.NewIdentity = opts.ExplicitAgentDir, true
	if _, err := Run(context.Background(), again); err != nil {
		t.Fatalf("new identity: %v", err)
	}
	cfg, err := config.Load(context.Background(), filepath.Join(opts.ExplicitAgentDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	oldID := between(before, "id: ", "\n")
	if cfg.Agent.ID == oldID || cfg.Agent.ID == "" {
		t.Fatalf("the identity did not change: %q", cfg.Agent.ID)
	}
	store, err := cfg.OpenSecretStore(zerolog.Nop(), true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.View(context.Background())
	if err != nil || p.AgentID != cfg.Agent.ID {
		t.Fatalf("the store is not bound to the new id: %v", err)
	}
	if p.Secrets["zmqcli_auth_token"].Value != zmqBefore {
		t.Fatal("secrets must survive a new identity")
	}
}

// A crash between the store rewrite and the config rewrite is completed by
// re-running the command.
func TestNewIdentityCompletesAnInterruptedRun(t *testing.T) {
	var out strings.Builder
	opts := baseOptions(freshInterviewAnswer, true, &out)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	opts.ReadyGuideCheck = func() bool { return true }
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(opts.ExplicitAgentDir, "config.yaml")
	cfg, _ := config.Load(context.Background(), cfgPath)
	store, _ := cfg.OpenSecretStore(zerolog.Nop(), false)
	const halfway = "0190bbbb-0000-7000-8000-000000000009"
	if err := store.Rebind(context.Background(), halfway); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(context.Background(), cfgPath); err != nil {
		t.Fatalf("config alone must still load: %v", err)
	}

	again := baseOptions("", false, &out)
	again.ExplicitAgentDir, again.NewIdentity = opts.ExplicitAgentDir, true
	if _, err := Run(context.Background(), again); err != nil {
		t.Fatalf("completing run: %v", err)
	}
	done, err := config.Load(context.Background(), cfgPath)
	if err != nil || done.Agent.ID != halfway {
		t.Fatalf("the config must adopt the store's id: %v %q", err, done.Agent.ID)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	rest := s[i+len(a):]
	j := strings.Index(rest, b)
	if j < 0 {
		return rest
	}

	return rest[:j]
}

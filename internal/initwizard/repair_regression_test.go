//nolint:goconst // literal fixtures are clearer inline than behind constants
package initwizard

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

const testPrimaryInstance = "primary"

// repairFixture writes data as the agent's config and returns the repair
// inputs. The agent's Secret store is created lazily by the first write.
func repairFixture(t *testing.T, data string) (config.Resolution, config.SourceState, repairOccurrences) {
	t.Helper()
	dir := t.TempDir()
	res := config.Resolution{AgentDir: dir, Path: filepath.Join(dir, config.ConfigFileName)}
	if err := os.WriteFile(res.Path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := config.ReadConfigSource(t.Context(), res.Path)
	if err != nil {
		t.Fatal(err)
	}
	occ, err := inspectForRepair(t.Context(), res, source)
	if err != nil {
		t.Fatal(err)
	}

	return res, source, occ
}

// seedSecret puts a Secret into the fixture agent's store.
func seedSecret(t *testing.T, res config.Resolution, occ repairOccurrences, name, value string) {
	t.Helper()
	store, err := repairStore(res, occ)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(t.Context(), func(p *secretstore.Payload) error {
		p.Secrets[name] = secretstore.Secret{Value: value}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func openRouterChoice() ProviderChoice {
	return ProviderChoice{Provider: testProviderOpenRouter, Instance: testProviderOpenRouter, API: apiChatCompletions, BaseURL: defaultEndpoints[testProviderOpenRouter], ModelID: "model", APIKey: "old-key"}
}

func TestRepairRotateZMQWritesTheStoreOnly(t *testing.T) {
	res, source, occ := repairFixture(t, string(generateConfig(openRouterChoice(), testAgentID, "host", true)))
	seedSecret(t, res, occ, zmqSecretName(), strings.Repeat("a", 64))
	before := readFileOrFail(t, res.Path)

	result, err := rotateZMQ(t.Context(), res, source, occ, fixedRandomHex)
	if err != nil || !result.Committed {
		t.Fatalf("rotate: committed=%v err=%v", result.Committed, err)
	}
	want, _ := fixedRandomHex(generateZMQTokenByteLength)
	if got := storedSecret(t, res.AgentDir, zmqSecretName()); got != want {
		t.Fatalf("stored secret = %q, want the freshly generated one", got)
	}
	if readFileOrFail(t, res.Path) != before {
		t.Fatal("rotating a referenced secret must not touch config.yaml")
	}
	for _, f := range []string{"zmqcli.token", "zmqcli.token-2"} {
		if _, statErr := os.Stat(filepath.Join(res.AgentDir, f)); statErr == nil {
			t.Fatalf("repair created the legacy file %s", f)
		}
	}
}

func TestRepairFixKeyReplacesTheReferencedSecret(t *testing.T) {
	res, source, occ := repairFixture(t, string(generateConfig(openRouterChoice(), testAgentID, "host", true)))
	seedSecret(t, res, occ, secretNameFor(testProviderOpenRouter), "old-key")
	before := readFileOrFail(t, res.Path)

	ui := repairUI{interview: interview{read: newPlainReader(strings.NewReader("rotated-key\n"))}, out: &strings.Builder{}}
	result, err := fixDefaultKey(t.Context(), res, source, occ, ui)
	if err != nil || !result.Committed {
		t.Fatalf("fix key: committed=%v err=%v", result.Committed, err)
	}
	if got := storedSecret(t, res.AgentDir, secretNameFor(testProviderOpenRouter)); got != "rotated-key" {
		t.Fatalf("stored key = %q", got)
	}
	if readFileOrFail(t, res.Path) != before {
		t.Fatal("fixing a referenced key must not touch config.yaml")
	}
}

func TestRepairNeverRewritesAnExternalSecretFile(t *testing.T) {
	external := filepath.Join(t.TempDir(), "key")
	const original = "external-key\n"
	if err := os.WriteFile(external, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(string(generateConfig(openRouterChoice(), testAgentID, "host", true)), "!secret openrouter_api_key", "!secret_file "+yamlQuote(external), 1)
	res, source, occ := repairFixture(t, data)
	ui := repairUI{interview: interview{read: newPlainReader(strings.NewReader("k\n"))}, out: &strings.Builder{}}
	if _, err := fixDefaultKey(t.Context(), res, source, occ, ui); !errors.Is(err, errUnsupportedDocument) {
		t.Fatalf("an external file reference must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.AgentDir, secretstore.StoreFileName)); err == nil {
		t.Fatal("a refused repair created a store")
	}
	if got := readFileOrFail(t, external); got != original {
		t.Fatal("a refused repair changed the external secret file")
	}
}

func TestRepairAddsAMissingReference(t *testing.T) {
	res, source, occ := repairFixture(t, readSeedConfig(t))
	ui := repairUI{interview: interview{read: newPlainReader(strings.NewReader("new-key\n"))}, out: &strings.Builder{}}
	result, err := fixDefaultKey(context.Background(), res, source, occ, ui)
	if err != nil || !result.Committed || result.BackupPath == "" {
		t.Fatalf("fix key without a reference: %+v, %v", result, err)
	}
	cfg, err := config.Load(t.Context(), res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Providers[testOllama].APIKey.Name != secretNameFor(testOllama) {
		t.Fatalf("reference = %+v", cfg.LLM.Providers[testOllama].APIKey)
	}
	if storedSecret(t, res.AgentDir, secretNameFor(testOllama)) != "new-key" {
		t.Fatal("value not stored")
	}
}

func TestRepairMissingIdentityIsTheOnlyOption(t *testing.T) {
	seed := strings.Replace(readSeedConfig(t), "agent:\n  id: "+testAgentID+"\n  name: test\n", "", 1)
	res, source, occ := repairFixture(t, seed)
	if !occ.MissingIdentity {
		t.Fatal("a config without agent.id must be reported")
	}
	if !strings.Contains(repairMenuText(occ), "agent identity") || strings.Contains(repairMenuText(occ), "Change the AI provider") {
		t.Fatalf("menu must offer the identity repair only:\n%s", repairMenuText(occ))
	}
	result, err := addIdentity(t.Context(), res, source, occ)
	if err != nil || !result.Committed {
		t.Fatalf("add identity: %+v, %v", result, err)
	}
	cfg, err := config.Load(t.Context(), res.Path)
	if err != nil || cfg.Agent.ID == "" {
		t.Fatalf("identity not added: %+v, %v", cfg, err)
	}
}

func TestRepairRunPrintsCommittedEndpointAndChatCommand(t *testing.T) {
	choice := ProviderChoice{Provider: testOllama, Instance: testOllama, API: testOllama, BaseURL: defaultEndpoints[testOllama], ModelID: "model"}
	data := strings.Replace(string(generateConfig(choice, testAgentID, "host", true)), defaultZMQBind, "tcp://127.0.0.1:8765", 1)
	res, _, _ := repairFixture(t, data)
	var output strings.Builder
	opts := baseOptions("3\n", true, &output)
	opts.ExplicitAgentDir = res.AgentDir
	result, err := Run(t.Context(), opts)
	if err != nil || !result.Committed {
		t.Fatalf("repair: committed=%v, err=%v", result.Committed, err)
	}
	if !strings.Contains(output.String(), "tcp://127.0.0.1:8765") || !strings.Contains(output.String(), " chat") {
		t.Fatalf("repair guidance does not name the committed endpoint and the chat command:\n%s", output.String())
	}
	if strings.Contains(output.String(), "--token-file") {
		t.Fatal("the legacy --token-file hint must be gone")
	}
}

func TestRepairReportsCommittedFailureAndWarnings(t *testing.T) {
	var output strings.Builder
	res := config.Resolution{Path: filepath.Join(t.TempDir(), "config.yaml")}
	reportRepairOutcome(t.Context(), Options{Stdout: &output}, res, repairRunsByOperator{Committed: true, Warnings: []string{"retention warning"}}, errors.New("sync failed"))
	for _, expected := range []string{"retention warning", "already committed", "Do not repeat the write"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing committed outcome detail %q", expected)
		}
	}
}

func TestRepairPreservesInstanceAndDependentReferences(t *testing.T) {
	seed := strings.ReplaceAll(readSeedConfig(t), testOllama, "primary")
	seed = strings.Replace(seed, "provider: primary", "provider: ollama", 1)
	seed = strings.Replace(seed, "api: primary", "api: ollama", 1)
	seed = strings.Replace(seed, "  providers:", "  failover:\n    - primary::fallback\n  providers:", 1)
	res, source, occ := repairFixture(t, seed)
	ui := repairUI{interview: interview{read: newPlainReader(strings.NewReader("y\nfallback-new\n"))}, out: &strings.Builder{}}
	choice := ProviderChoice{Provider: testProviderOpenRouter, Instance: testProviderOpenRouter, API: apiChatCompletions, BaseURL: defaultEndpoints[testProviderOpenRouter], ModelID: "new-model", APIKey: "new-key"}
	result, err := replaceDefaultProvider(t.Context(), res, source, occ, ui, func(_ io.Writer) (ProviderChoice, error) { return choice, nil })
	if err != nil || !result.Committed {
		t.Fatalf("replace provider: committed=%v, err=%v", result.Committed, err)
	}
	cfg, err := config.Load(t.Context(), res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Default != "primary::new-model" || cfg.LLM.Failover[0] != "primary::fallback-new" || cfg.LLM.Providers["primary"].Provider != testProviderOpenRouter {
		t.Fatal("replacement lost the provider instance identity")
	}
	if storedSecret(t, res.AgentDir, secretNameFor("primary")) != "new-key" {
		t.Fatal("the new key was not stored under the instance's canonical name")
	}
}

func TestRepairProviderTransitions(t *testing.T) {
	choices := []ProviderChoice{
		{Provider: config.ProviderOpenAICodex, Instance: testPrimaryInstance, API: apiResponses, ModelID: "codex-model"},
		{Provider: testProviderOpenRouter, Instance: testPrimaryInstance, API: apiChatCompletions, BaseURL: defaultEndpoints[testProviderOpenRouter], ModelID: "remote-model", APIKey: "remote-key"},
		{Provider: testOllama, Instance: testPrimaryInstance, API: testOllama, BaseURL: defaultEndpoints[testOllama], ModelID: "local-model"},
		{Provider: testOllama, Instance: testPrimaryInstance, API: testOllama, BaseURL: defaultOllamaCloudEndpoint, ModelID: "cloud-model", APIKey: "cloud-key", OllamaCloud: true},
	}
	for idx, before := range choices {
		after := choices[(idx+1)%len(choices)]
		t.Run(before.Provider+"-to-"+after.Provider, func(t *testing.T) {
			res, source, occ := repairFixture(t, string(generateConfig(before, testAgentID, "host", true)))
			ui := repairUI{interview: interview{read: newPlainReader(strings.NewReader(""))}, out: &strings.Builder{}}
			result, err := replaceDefaultProvider(t.Context(), res, source, occ, ui, func(_ io.Writer) (ProviderChoice, error) { return after, nil })
			if err != nil || !result.Committed {
				t.Fatalf("transition: committed=%v, err=%v", result.Committed, err)
			}
			cfg, err := config.Load(t.Context(), res.Path)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.LLM.Providers[before.Instance]
			if p.Provider != after.Provider || cfg.LLM.Default != before.Instance+"::"+after.ModelID {
				t.Fatal("provider transition changed the instance identity")
			}
			if after.needsAPIKey() != p.APIKey.IsSet() {
				t.Fatalf("key reference presence = %v, want %v", p.APIKey.IsSet(), after.needsAPIKey())
			}
			if after.needsAPIKey() && storedSecret(t, res.AgentDir, secretNameFor(before.Instance)) != after.APIKey {
				t.Fatal("replacement did not store the selected deployment credential")
			}
			if after.Provider == config.ProviderOpenAICodex {
				if _, nodeErr := locateValueNode([]byte(readFileOrFail(t, res.Path)), []string{segLLM, segProviders, before.Instance, "base_url"}); nodeErr == nil {
					t.Fatal("Codex retained an authored configurable endpoint")
				}
			}
		})
	}
}

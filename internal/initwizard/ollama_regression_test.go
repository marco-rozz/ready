package initwizard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/config"
	"github.com/marco-rozz/ready/internal/secretstore"
)

func TestRunMissingLibraryStillSavesConfig(t *testing.T) {
	var output strings.Builder
	opts := baseOptions(freshInterviewAnswer, true, &output)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	opts.ReadyGuideCheck = func() bool { return false }
	outcome, err := Run(t.Context(), opts)
	if err != nil || !outcome.Committed {
		t.Fatalf("missing library prevented saving the interview: committed=%v err=%v", outcome.Committed, err)
	}
	data := readFileOrFail(t, filepath.Join(opts.ExplicitAgentDir, config.ConfigFileName))
	if strings.Contains(data, "inject_skills: [ready-guide]") || !strings.Contains(output.String(), "warning:") {
		t.Fatal("missing guide must be reported without injecting an unavailable skill")
	}
}

func TestRunOllamaCloudSavesKeyInTheStore(t *testing.T) {
	var output strings.Builder
	opts := baseOptions("ollama\ncloud\n\ngpt-oss:120b\ncloud-test-key\nn\n", true, &output)
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	opts.ReadyGuideCheck = func() bool { return true }
	outcome, err := Run(t.Context(), opts)
	if err != nil || !outcome.Committed {
		t.Fatalf("cloud init did not save: committed=%v err=%v", outcome.Committed, err)
	}
	cfg, err := config.Load(t.Context(), filepath.Join(opts.ExplicitAgentDir, config.ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.LLM.Providers[testOllama]
	if p.BaseURL != "https://ollama.com" || p.APIKey.Name != secretNameFor(testOllama) {
		t.Fatalf("cloud endpoint or key reference missing: endpoint=%s ref=%+v", p.BaseURL, p.APIKey)
	}
	key := storedSecret(t, opts.ExplicitAgentDir, secretNameFor(testOllama))
	if key != "cloud-test-key" {
		t.Fatal("cloud credential was not saved correctly")
	}
	if strings.Contains(output.String(), key) {
		t.Fatal("cloud credential leaked into output")
	}
	info, err := os.Stat(filepath.Join(opts.ExplicitAgentDir, secretstore.StoreFileName))
	if err != nil || (runtime.GOOS != testWindowsOS && info.Mode().Perm()&0o077 != 0) {
		t.Fatal("the secret store must be private")
	}
}

func TestInterviewOllamaDeployments(t *testing.T) {
	for _, tc := range []struct {
		name, answers, endpoint string
		cloud                   bool
	}{
		{"local", "ollama\nlocal\n\n\n", defaultEndpoints[testOllama], false},
		{"cloud", "ollama\ncloud\n\n\nkey\n", defaultOllamaCloudEndpoint, true},
		{"custom-cloud", "ollama\ncloud\nhttps://api.ollama.com\n\nkey\n", "https://api.ollama.com", true},
		{"invalid-choice-retries", "ollama\ninvalid\ncloud\n\n\nkey\n", defaultOllamaCloudEndpoint, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output strings.Builder
			choice, err := runInterview(t.Context(), baseOptions(tc.answers, true, &output))
			if err != nil {
				t.Fatal(err)
			}
			if choice.BaseURL != tc.endpoint || choice.OllamaCloud != tc.cloud || choice.ModelID != defaultModels[testOllama] {
				t.Fatal("deployment or accepted defaults were lost")
			}
			if strings.Contains(output.String(), "API key (input hidden") != tc.cloud {
				t.Fatal("API key must be requested for cloud deployment")
			}
		})
	}
}

func TestOllamaCloudMissingKeyDoesNotPublish(t *testing.T) {
	opts := baseOptions("ollama\ncloud\n\n\n\n", true, &strings.Builder{})
	opts.ExplicitAgentDir = filepath.Join(t.TempDir(), "agent")
	outcome, err := Run(t.Context(), opts)
	if err == nil || outcome.Committed {
		t.Fatal("cloud without an API key must fail before publication")
	}
	if _, err := os.Stat(opts.ExplicitAgentDir); !os.IsNotExist(err) {
		t.Fatal("incomplete cloud interview created agent state")
	}
}

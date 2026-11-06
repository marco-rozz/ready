// Starter configuration: one provider, no TODO marker, explicit
// default/edit/admin policies, optional ready-guide injection and editable
// logging, skill and conversation defaults. Generation is a fixed template — byte
// shape is part of the contract, so a structured marshal is deliberately
// avoided; ValidateCandidate is the proof the bytes are a valid config.
package initwizard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/marco-rozz/ready/internal/config"
)

// defaultZMQBind is the generated loopback bind.
const defaultZMQBind = "tcp://127.0.0.1:7600"

// secretNameFor is the canonical Secret name of a provider instance's API key.
func secretNameFor(instance string) string {
	name, _ := config.CanonicalSecretName([]string{"llm", "providers", instance, "api_key"})

	return name
}

// zmqSecretName is the canonical Secret name of the ZMQ shared secret.
func zmqSecretName() string {
	name, _ := config.CanonicalSecretName([]string{"channels", "zmqcli", "auth_token"})

	return name
}

// generateConfig renders the starter config for the interview answers. The
// file holds references only — the values live in the agent's Secret store.
// Codex never carries a credential reference (OAuth Credentials stay in the
// store, entered by `ready login`) and never carries base_url (the fixed
// ChatGPT traffic endpoint is the provider's own default).
func generateConfig(choice ProviderChoice, id, name string, withGuide bool) []byte {
	var providerBlock strings.Builder
	providerBlock.WriteString("    " + choice.Instance + ":\n")
	providerBlock.WriteString("      provider: " + choice.Provider + "\n")
	providerBlock.WriteString("      api: " + choice.API + "\n")
	if choice.needsAPIKey() {
		providerBlock.WriteString("      api_key: " + config.TagSecret + " " + secretNameFor(choice.Instance) + "\n")
	}
	if choice.Provider != config.ProviderOpenAICodex {
		providerBlock.WriteString("      base_url: " + choice.BaseURL + "\n")
	}
	guideSkills := "[]"
	if withGuide {
		guideSkills = "[ready-guide]"
	}

	tmpl := `config_version: %d
agent:
  id: %s
  name: %s
llm:
  default: %s::%s
  providers:
%s
channels:
  zmqcli:
    enabled: true
    bind: %s
    auth_token: !secret %s
    formatter_skill: humanizer-formatter
    bootstrap: ["home-topology"]
default_profile: default
builtin_tools:
  profiles:
    default:
      deny: [%s]
    edit:
      deny: [%s]
    admin:
      allow: ["*"]
profiles:
  admin:
    inject_skills: %s
# Starter defaults: change these later in admin chat or in this file.
logging:
  level: info
  format: json
  file_path: logs/ready.log
  file_rotation:
    max_segment_size: 10MB
    retention_days: 7
    max_total_size: 100MB
skills:
  path: skills/
  sync: auto
  hot_reload: true
sessions:
  path: sessions/
  reset:
    mode: daily
    daily_at: "04:00"
    notify: true
  retention:
    retention_days: 30
    max_total_size: 200MB
compaction:
  enabled: true
`

	return []byte(fmt.Sprintf(tmpl,
		config.TargetConfigVersion, id, yamlQuote(name),
		choice.Instance, choice.ModelID,
		strings.TrimRight(providerBlock.String(), "\n"),
		defaultZMQBind, zmqSecretName(),
		config.ConfigManagerToolKey, config.ConfigManagerToolKey,
		guideSkills,
	))
}

// yamlQuote renders s as a double-quoted YAML scalar (the display name is
// free text, so it is always quoted).
func yamlQuote(s string) string {
	return strconv.Quote(s)
}

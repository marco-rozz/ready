//nolint:goconst // literal fixtures are clearer inline than behind constants
package config

import (
	"reflect"
	"strings"
	"testing"
)

// isCredentialFieldName reports whether a YAML field name plausibly carries
// a credential value: the exact names api_key/token/secret/password or any
// name ending in _token, _secret or _password. It is the tripwire for the
// coverage contract: any new field matching this predicate must be typed
// SecretRef and join sensitivePatterns in the same change.
func isCredentialFieldName(name string) bool {
	switch name {
	case "api_key", "token", "secret", "password":
		return true
	}

	return strings.HasSuffix(name, "_token") || strings.HasSuffix(name, "_secret") || strings.HasSuffix(name, "_password")
}

// testModelRef is the shared minimal model reference in these tests.
const testModelRef = "p::m"

// testAuthHeader is the repeated Authorization header spelling.
const testAuthHeader = "Authorization"

// testKeyRef is the shared Secret reference used by fixtures that need a
// present sensitive field.
var testKeyRef = SecretRef{Kind: SecretRefName, Name: "test_key"} //nolint:gochecknoglobals // immutable test fixture

func secretRefType() reflect.Type { return reflect.TypeFor[SecretRef]() }

// TestSensitiveCoverage walks the whole Config struct through its yaml tags.
// Every SecretRef field (or map of SecretRef) must be classified by
// sensitivePatterns, and every credential-named field must be a SecretRef —
// a plain string or map[string]string there would accept a literal secret.
func TestSensitiveCoverage(t *testing.T) { //nolint:paralleltest // package-level reflection walk
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			yamlName := strings.Split(field.Tag.Get("yaml"), ",")[0]
			if yamlName == "-" || yamlName == "" {
				continue
			}
			path := yamlName
			if prefix != "" {
				path = prefix + "." + yamlName
			}

			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}

			switch {
			case fieldType == secretRefType():
				if !sensitivePathExistsForStructPath(path) {
					t.Errorf("SecretRef field %s is not classified in sensitivePatterns", path)
				}

			case fieldType.Kind() == reflect.Map && fieldType.Elem() == secretRefType():
				if !sensitivePathExistsForStructPath(path + ".x") {
					t.Errorf("SecretRef map %s is not classified in sensitivePatterns", path)
				}

			case fieldType.Kind() == reflect.Map && fieldType.Elem().Kind() == reflect.String:
				if strings.EqualFold(camelToSnake(field.Name), "headers") {
					t.Errorf("header map %s must be map[string]SecretRef: every header value is a Secret", path)
				}
				if isCredentialFieldName(yamlName) {
					t.Errorf("map field %s looks like a credential but is not a SecretRef map", path)
				}

			case fieldType.Kind() == reflect.String:
				if isCredentialFieldName(yamlName) {
					t.Errorf("credential-bearing field %s must be typed SecretRef, not string", path)
				}

			default:
				walk(path, fieldType)
			}
		}
	}

	walk("", reflect.TypeFor[Config]())
}

// sensitivePathExistsForStructPath checks the dotted struct path against the
// classification by translating it to the yaml path naming convention: the
// struct field name prefix (which struct owners like HomeAssistant.WebSocket
// collapse in yaml) is scrubbed by testing all suffixes of the path.
func sensitivePathExistsForStructPath(path string) bool {
	segs := strings.Split(path, ".")
	for i := range segs {
		if IsSensitivePath(segs[i:]) {
			return true
		}
		// A map-valued field (providers, mcp_servers) is addressed by key in
		// the classification: try a wildcard-friendly spelling too.
		if i+1 < len(segs) {
			withKey := append([]string{segs[i], "k"}, segs[i+1:]...)
			if IsSensitivePath(withKey) {
				return true
			}
		}
	}

	return false
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			_, _ = b.WriteRune('_')
		}
		_, _ = b.WriteRune(r)
	}

	return strings.ToLower(b.String())
}

func TestIsSensitivePath(t *testing.T) {
	t.Parallel()

	classified := []string{
		segLLM + ".providers.gemma." + segAPIKey,
		segChannels + ".telegram." + segToken,
		segChannels + "." + segZmqcli + "." + segAuthToken,
		segHomeAssistant + "." + segWebsocket + "." + segToken,
		segMCPServers + ".home_assistant." + segOAuth + "." + segClientSecret,
		segMCPServers + ".home_assistant." + segHeaders + ".Authorization",
		segMCPServers + ".home_assistant." + segHeaders + ".X-API-Key",
	}
	for _, p := range classified {
		if !IsSensitivePath(strings.Split(p, ".")) {
			t.Errorf("expected %s to be classified sensitive", p)
		}
	}

	notClassified := []string{
		"channels.telegram.allow_from",
		"home_assistant.websocket.url",
		"mcp_servers.home_assistant.url",
		"mcp_servers.home_assistant.tool_prefix",
	}
	for _, p := range notClassified {
		if IsSensitivePath(strings.Split(p, ".")) {
			t.Errorf("expected %s to be clear (not classified sensitive)", p)
		}
	}
}

func TestSecretReferencesWalksAllSubsystems(t *testing.T) {
	t.Parallel()

	ref := func(n string) SecretRef { return SecretRef{Kind: SecretRefName, Name: n} }
	cfg := &Config{Agent: testAgent}
	cfg.LLM.Providers = map[string]LLMProviderConfig{"p": {APIKey: ref("p_api_key")}, "bare": {}}
	cfg.Channels.Telegram.Token = ref("telegram_token")
	cfg.Channels.ZMQCLI.AuthToken = ref("zmqcli_auth_token")
	cfg.HomeAssistant.WebSocket.Token = ref("home_assistant_token")
	cfg.MCPServers = map[string]MCPServerConfig{
		"s": {
			Headers: map[string]SecretRef{testAuthHeader: ref("mcp_s_authorization")},
			OAuth:   &MCPOAuthConfig{ClientSecret: ref("mcp_s_client_secret")},
		},
	}

	got := map[string]string{}
	for _, r := range cfg.SecretReferences() {
		if !IsSensitivePath(r.Path) {
			t.Errorf("reference at unclassified path %s", r.DottedPath())
		}
		got[r.DottedPath()] = r.Ref.Name
	}
	want := map[string]string{ //nolint:gosec // reference names, not credentials
		"llm.providers.p.api_key":                 "p_api_key",
		"channels.telegram.token":                 "telegram_token",
		"channels.zmqcli.auth_token":              "zmqcli_auth_token",
		"home_assistant.websocket.token":          "home_assistant_token",
		"mcp_servers.s.oauth.client_secret":       "mcp_s_client_secret",
		"mcp_servers.s.headers." + testAuthHeader: "mcp_s_authorization",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SecretReferences = %v, want %v", got, want)
	}
}

func TestValidateTypicalValidConfigStillPasses(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agent: testAgent}
	cfg.LLM.Default = testModelRef
	cfg.LLM.Providers = map[string]LLMProviderConfig{"p": {Provider: ProviderOllama, API: ProviderOllama}}
	cfg.Channels.ZMQCLI.Enabled = true
	cfg.Channels.ZMQCLI.Bind = "tcp://127.0.0.1:7600"
	cfg.Channels.ZMQCLI.AuthToken = testKeyRef

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config to pass, got: %v", err)
	}
}

func TestValidateRequiresReferencesWhereEnabled(t *testing.T) {
	t.Parallel()

	base := func() *Config {
		cfg := &Config{Agent: testAgent}
		cfg.LLM.Default = testModelRef
		cfg.LLM.Providers = map[string]LLMProviderConfig{"p": {Provider: ProviderOllama, API: ProviderOllama}}

		return cfg
	}
	cases := map[string]func(*Config){
		"telegram": func(c *Config) {
			c.Channels.Telegram.Enabled = true
			c.Channels.Telegram.AllowFrom = []string{"1"}
		},
		"zmq": func(c *Config) {
			c.Channels.ZMQCLI.Enabled = true
			c.Channels.ZMQCLI.Bind = "tcp://127.0.0.1:7600"
		},
		"home assistant": func(c *Config) {
			c.HomeAssistant.Enabled = true
			c.HomeAssistant.WebSocket.URL = SecretString{Value: "ws://x"}
		},
	}
	for name, mutate := range cases {
		cfg := base()
		mutate(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "`!secret` reference") {
			t.Errorf("%s: err = %v, want a missing-reference error", name, err)
		}
	}
}

// testAgent is the identity every valid fixture carries.
var testAgent = AgentConfig{ID: testAgentID} //nolint:gochecknoglobals // immutable test fixture

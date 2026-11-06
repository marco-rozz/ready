package config

import (
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// sensitivePathPattern is one classified sensitive location. Segments match
// exact YAML mapping keys; "*" matches exactly one segment of any value.
type sensitivePathPattern struct {
	segments []string
}

// Segment vocabulary of the classification patterns (single spelling per
// package; also satisfies the goconst threshold hygiene).
const (
	segLLM           = "llm"
	segProviders     = "providers"
	segAPIKey        = "api_key"
	segChannels      = "channels"
	segTelegram      = "telegram"
	segZmqcli        = "zmqcli"
	segAuthToken     = "auth_token"
	segHomeAssistant = "home_assistant"
	segWebsocket     = "websocket"
	segToken         = "token"
	segMCPServers    = "mcp_servers"
	segOAuth         = "oauth"
	segClientSecret  = "client_secret"
	segHeaders       = "headers"
	segURL           = "url"
)

// sensitivePatterns is the code-owned classification of credential-bearing
// fields. Every one of them accepts only a Secret reference (SecretRef). A
// change that introduces a credential-bearing configuration field MUST extend
// this table and type the field SecretRef in the same change; the
// schema-coverage test in sensitive_test.go enforces it.
//
//nolint:gochecknoglobals // immutable classification table
var sensitivePatterns = []sensitivePathPattern{
	{[]string{segLLM, segProviders, "*", segAPIKey}},
	{[]string{segChannels, segTelegram, segToken}},
	{[]string{segChannels, segZmqcli, segAuthToken}},
	{[]string{segHomeAssistant, segWebsocket, segToken}},
	{[]string{segMCPServers, "*", segOAuth, segClientSecret}},
	{[]string{segMCPServers, "*", segHeaders, "*"}},
}

// IsSensitivePath reports whether the exact YAML path (mapping keys in
// document order) names a classified sensitive field, which accepts only a
// Secret reference. Classification covers disabled subsystems and every
// header spelling.
func IsSensitivePath(path []string) bool {
	for _, p := range sensitivePatterns {
		if patternMatches(p.segments, path) {
			return true
		}
	}

	return false
}

func patternMatches(pattern, path []string) bool {
	if len(pattern) != len(path) {
		return false
	}
	for i, seg := range pattern {
		if seg != "*" && seg != path[i] {
			return false
		}
	}

	return true
}

func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// SecretReference is one classified sensitive field of a decoded
// configuration with the reference it carries. It never holds a value.
type SecretReference struct {
	// Path is the exact YAML path of the classified field.
	Path []string
	// Ref is the field's reference; unset when the field is absent.
	Ref SecretRef
}

// DottedPath renders the field path in the operator-facing dotted form.
func (r SecretReference) DottedPath() string { return strings.Join(r.Path, ".") }

// SecretReferences returns every classified sensitive field of the decoded
// configuration — including disabled subsystems — that carries a reference,
// in stable dotted-path order. Unset fields are omitted: whether their
// absence is legal is semantic validation's job.
func (c *Config) SecretReferences() []SecretReference {
	var refs []SecretReference
	report := func(path []string, ref SecretRef) {
		if ref.IsSet() {
			refs = append(refs, SecretReference{Path: path, Ref: ref})
		}
	}

	for name, p := range c.LLM.Providers {
		report([]string{segLLM, segProviders, name, segAPIKey}, p.APIKey)
	}
	report([]string{segChannels, segTelegram, segToken}, c.Channels.Telegram.Token)
	report([]string{segChannels, segZmqcli, segAuthToken}, c.Channels.ZMQCLI.AuthToken)
	report([]string{segHomeAssistant, segWebsocket, segToken}, c.HomeAssistant.WebSocket.Token)
	if ref := c.HomeAssistant.WebSocket.URL.Ref(); ref != nil {
		report([]string{segHomeAssistant, segWebsocket, segURL}, *ref)
	}

	for key, srv := range c.MCPServers {
		if ref := srv.URL.Ref(); ref != nil {
			report([]string{segMCPServers, key, segURL}, *ref)
		}
		if srv.OAuth != nil {
			report([]string{segMCPServers, key, segOAuth, segClientSecret}, srv.OAuth.ClientSecret)
		}
		for h, ref := range srv.Headers {
			report([]string{segMCPServers, key, segHeaders, h}, ref)
		}
	}

	sort.Slice(refs, func(i, j int) bool { return refs[i].DottedPath() < refs[j].DottedPath() })

	return refs
}

// LiteralSecretPaths walks a raw document and returns the dotted paths of
// sensitive fields that hold a literal value instead of a Secret reference.
// It reads node tags only: values are never returned or logged.
func LiteralSecretPaths(data []byte) []string {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(n *yaml.Node, path []string)
	walk = func(n *yaml.Node, path []string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1], append(append([]string{}, path...), n.Content[i].Value))
			}
		default:
			if IsSensitivePath(path) && n.Tag != TagSecret && n.Tag != TagSecretFile && n.Tag != yamlNullTag {
				out = append(out, strings.Join(path, "."))
			}
		}
	}
	walk(&doc, nil)
	sort.Strings(out)

	return out
}

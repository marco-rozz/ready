package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlNullTag is the tag yaml.v3 gives an absent/null scalar.
const yamlNullTag = "!!null"

// YAML tags of a Secret reference.
const (
	// TagSecret references a named entry of the Secret store: `!secret <name>`.
	TagSecret = "!secret"
	// TagSecretFile references an external file by absolute path:
	// `!secret_file <path>`.
	TagSecretFile = "!secret_file"
)

// SecretRefKind distinguishes the two reference forms.
type SecretRefKind int

const (
	// SecretRefNone is the zero value: no reference is set.
	SecretRefNone SecretRefKind = iota
	// SecretRefName is a `!secret <name>` reference to the Secret store.
	SecretRefName
	// SecretRefFile is a `!secret_file <path>` reference to an external file.
	SecretRefFile
)

// ErrInvalidSecretRef is the sentinel for every refused reference form. The
// wrapping error names the field path but never echoes the scalar: a literal
// written where a reference belongs may be a real credential.
var ErrInvalidSecretRef = errors.New("invalid secret reference")

// secretNamePattern is the grammar of a Secret name.
var secretNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)

// SecretRef is the only form a sensitive configuration field may take: a
// reference to a Secret, never the value. The zero value means "unset".
type SecretRef struct {
	Kind SecretRefKind
	// Name is the Secret store name (Kind == SecretRefName).
	Name string
	// Path is the absolute external file path (Kind == SecretRefFile).
	Path string
}

// IsSet reports whether the reference is present.
func (r SecretRef) IsSet() bool { return r.Kind != SecretRefNone }

// ValidSecretName reports whether name satisfies the Secret name grammar.
func ValidSecretName(name string) bool { return secretNamePattern.MatchString(name) }

// String renders the reference as it appears in YAML. It never contains a value.
func (r SecretRef) String() string {
	switch r.Kind {
	case SecretRefName:
		return TagSecret + " " + r.Name
	case SecretRefFile:
		return TagSecretFile + " " + r.Path
	default:
		return ""
	}
}

// UnmarshalYAML decodes a tagged scalar. Because yaml.v3 would otherwise
// silently decode `k: !secret x` as the plain string "x", this method inspects
// the node tag itself and refuses every untagged or foreign-tagged scalar.
func (r *SecretRef) UnmarshalYAML(node *yaml.Node) error {
	ref, err := parseSecretRefNode(node)
	if err != nil {
		return err
	}
	*r = ref

	return nil
}

// MarshalYAML emits the tagged scalar.
func (r SecretRef) MarshalYAML() (any, error) {
	switch r.Kind {
	case SecretRefName:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: TagSecret, Value: r.Name}, nil
	case SecretRefFile:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: TagSecretFile, Value: r.Path}, nil
	default:
		return nil, nil
	}
}

// IsZero lets yaml.v3 honour omitempty for an unset reference.
func (r SecretRef) IsZero() bool { return r.Kind == SecretRefNone }

func parseSecretRefNode(node *yaml.Node) (SecretRef, error) {
	where := fmt.Sprintf("line %d", node.Line)
	if node.Kind != yaml.ScalarNode {
		return SecretRef{}, fmt.Errorf("%w: %s: a sensitive field takes a `!secret <name>` or `!secret_file <path>` scalar", ErrInvalidSecretRef, where)
	}
	switch node.Tag {
	case TagSecret:
		if !ValidSecretName(node.Value) {
			return SecretRef{}, fmt.Errorf("%w: %s: a secret name is 1-64 characters of [a-z0-9_] starting with [a-z0-9]", ErrInvalidSecretRef, where)
		}

		return SecretRef{Kind: SecretRefName, Name: node.Value}, nil
	case TagSecretFile:
		if node.Value == "" {
			return SecretRef{}, fmt.Errorf("%w: %s: %s needs a path", ErrInvalidSecretRef, where, TagSecretFile)
		}
		if !filepath.IsAbs(node.Value) {
			return SecretRef{}, fmt.Errorf("%w: %s: %s needs an absolute path", ErrInvalidSecretRef, where, TagSecretFile)
		}

		return SecretRef{Kind: SecretRefFile, Path: filepath.Clean(node.Value)}, nil
	case yamlNullTag:
		return SecretRef{}, nil
	default:
		return SecretRef{}, fmt.Errorf("%w: %s: a sensitive field holds a reference, never a value — use `!secret <name>` (see `ready secrets set`)", ErrInvalidSecretRef, where)
	}
}

// CanonicalSecretName derives the proposed Secret name for a sensitive field
// path (design S2). Callers that must avoid collisions use UniqueSecretName.
//
//nolint:gosec // G602 false positive: every index is guarded by the len(path) test in the same case
func CanonicalSecretName(path []string) (string, bool) {
	var raw string
	switch {
	case len(path) == 4 && path[0] == segLLM && path[1] == segProviders && path[3] == segAPIKey:
		raw = path[2] + "_api_key"
	case equalPaths(path, []string{segChannels, segTelegram, segToken}):
		raw = "telegram_token"
	case equalPaths(path, []string{segChannels, segZmqcli, segAuthToken}):
		raw = "zmqcli_auth_token"
	case equalPaths(path, []string{segHomeAssistant, segWebsocket, segToken}):
		raw = "home_assistant_token"
	case len(path) == 4 && path[0] == segMCPServers && path[2] == segOAuth && path[3] == segClientSecret:
		raw = "mcp_" + path[1] + "_client_secret"
	case len(path) == 4 && path[0] == segMCPServers && path[2] == segHeaders:
		raw = "mcp_" + path[1] + "_" + path[3]
	default:
		return "", false
	}

	return sanitizeSecretName(raw), true
}

// UniqueSecretName returns base, or base with a _2, _3… suffix, such that
// taken reports false for the result.
func UniqueSecretName(base string, taken func(string) bool) string {
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("_%d", i)
		candidate := truncateName(base, 64-len(suffix)) + suffix
		if !taken(candidate) {
			return candidate
		}
	}
}

// sanitizeSecretName lowercases, collapses every non-[a-z0-9] run into one
// "_", trims "_" at the ends and bounds the length.
func sanitizeSecretName(s string) string {
	var b strings.Builder
	lastUnderscore := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false

			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}

	return truncateName(strings.Trim(b.String(), "_"), 64)
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return strings.TrimRight(s[:n], "_")
}

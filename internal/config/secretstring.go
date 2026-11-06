package config

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
)

// SecretString is a plain string field that MAY carry a Secret reference.
// The schema declares per-field whether the capability exists (the field's
// type): a field of this type accepts per instance either a plain literal
// (kept in clear — most endpoints carry no credential at all) or a
// `!secret <name>` / `!secret_file /abs/path` node, resolved from the
// Secret store at boot. The operator decides per instance; a plain
// `string` field never accepts the tags (the strict decoder refuses them).
type SecretString struct {
	// Value is the plain literal, or the resolved value after Resolve ran.
	// Empty while a reference is pending.
	Value string
	// ref is the parsed reference when the node carried one, nil otherwise.
	ref *SecretRef
	// pending reports whether a reference is present but not yet resolved.
	pending bool
}

// IsReference reports whether the field carries a `!secret` reference
// (pending or resolved).
func (s SecretString) IsReference() bool { return s.ref != nil }

// Pending reports whether a reference is present but not yet resolved:
// the value is absent or non-conforming in the store (or external file).
func (s SecretString) Pending() bool { return s.pending }

// Ref exposes the carried reference for the store census; nil when the
// field holds a plain literal.
func (s SecretString) Ref() *SecretRef { return s.ref }

// LogValue renders the field for logging: the plain literal when the
// operator wrote one, the reference form (never the resolved value) when
// the field carries a reference — a referenced URL may embed a token in
// its query string and must never reach a log line.
func (s SecretString) LogValue() string {
	if s.ref != nil {
		return s.ref.String()
	}

	return s.Value
}

// UnmarshalYAML decodes per instance: a `!secret`/`!secret_file` node keeps
// its reference (never the tag's content as a string — the silent tag drop
// that used to turn `url: !secret x` into the literal "x"); a null node is
// the zero value; any other scalar is the plain value.
func (s *SecretString) UnmarshalYAML(node *yaml.Node) error {
	switch node.Tag {
	case TagSecret, TagSecretFile:
		ref, err := parseSecretRefNode(node)
		if err != nil {
			return err
		}

		*s = SecretString{ref: &ref, pending: true}

		return nil
	case yamlNullTag:
		*s = SecretString{}

		return nil
	default:
		if node.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: a secret-capable string field takes a scalar value or a `!secret` reference", node.Line)
		}

		*s = SecretString{Value: node.Value}
	}

	return nil
}

// Resolve resolves the field in place when it carries a reference, using
// the same store-backed source as the classified sensitive fields.
// pending=true means the reference has no usable value yet: the owning
// component starts (or keeps running) only after the value is entered and
// `ready serve` restarts. A plain literal resolves to itself.
func (s *SecretString) Resolve(ctx context.Context, c *Config, field string) (pending bool, err error) {
	if s.ref == nil {
		return false, nil
	}

	value, ok, err := c.ResolveSecret(ctx, *s.ref, field)
	if err != nil {
		return false, err
	}

	s.pending = !ok
	if ok {
		s.Value = value
	}

	return !ok, nil
}

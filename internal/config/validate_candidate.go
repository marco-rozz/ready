package config

import (
	"context"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ProfileOverrideInputs captures the boot-time profile override inputs —
// the --profile flag value and the READY_DEFAULT_PROFILE environment value — so
// private candidate validation can resolve the candidate's configured profile
// under the original precedence instead of freezing the running daemon's
// active profile name. Both fields may be empty.
type ProfileOverrideInputs struct {
	// Flag is the --profile flag value at daemon boot ("" when absent).
	Flag string
	// Env is the READY_DEFAULT_PROFILE value at daemon boot ("" when absent).
	Env string
}

// ValidateCandidate decodes and validates candidate configuration bytes
// exactly as ordinary loading of the elected file would — same decoder,
// defaults, runtime-home calculation, semantic rules and serve-local
// enabled-channel/profile checks — without writing any temporary
// configuration, running migrations, probing a network, authorizing OAuth or
// starting runtime components.
//
// The elected path is the original elected config file path: relative
// runtime paths keep their boot meaning because decoding resolves them
// against that file's directory, not against a temporary copy. Validation is
// structural: it never reads a Secret store, a Store key or a secret file.
//
// serveLocalChecks, when non-nil, receives the decoded candidate for the
// caller's serve-local checks (enabled-channel and profile checks); it must
// be side-effect free.
//
// The returned *Config is a decoded view of the candidate for diagnostics —
// it is NOT the running daemon's configuration and activating it requires a
// committed save plus a daemon restart.
func ValidateCandidate(
	ctx context.Context,
	candidate []byte,
	electedPath string,
	overrides ProfileOverrideInputs,
	serveLocalChecks func(*Config) error,
) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("validate candidate config: %w", err)
	}

	cfg, err := decodeConfigTarget(ctx, candidate, electedPath, TargetConfigVersion)
	if err != nil {
		return nil, err
	}

	if err = cfg.validate(); err != nil {
		return nil, err
	}

	// The candidate's configured profile resolves under the ORIGINAL
	// precedence inputs (flag > env > candidate key), never against the
	// running daemon's active profile name.
	if _, _, err = cfg.ResolveProfile(overrides.Flag, overrides.Env); err != nil {
		return nil, fmt.Errorf("invalid candidate config %s: %w", electedPath, err)
	}

	if serveLocalChecks != nil {
		if err = serveLocalChecks(cfg); err != nil {
			return nil, fmt.Errorf("invalid candidate config %s: %w", electedPath, err)
		}
	}

	return cfg, nil
}

// FindNodeLine locates the 1-based line of the scalar at the given path in
// raw bytes (0 when the path cannot be located). It re-parses the document
// read-only; aliases resolve to their anchor position, which is the honest
// source location.
func FindNodeLine(data []byte, path []string) int {
	return findNodeLine(data, path)
}

// findNodeLine implements FindNodeLine.
func findNodeLine(data []byte, path []string) int {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return 0
	}
	node := &doc
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, seg := range path {
		if node.Kind != yaml.MappingNode {
			return 0
		}
		found := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == seg {
				node = node.Content[i+1]
				found = true

				break
			}
		}
		if !found {
			return 0
		}
	}

	return node.Line
}

// DecodeForRepair decodes a document for the guided-init repair: every decode
// failure (old or future schema, broken YAML, a literal where a reference
// belongs, superseded shapes) refuses with the ordinary loading error. It
// never writes, migrates or talks to the network.
func DecodeForRepair(ctx context.Context, data []byte, path string) (*Config, error) {
	return decodeConfigTarget(ctx, data, path, TargetConfigVersion)
}

// ValidateForRepair runs the full structural validation of a decoded
// document for the repair flow.
func (c *Config) ValidateForRepair() error {
	return c.validate()
}

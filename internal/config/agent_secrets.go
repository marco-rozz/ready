package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// maxAgentNameLen bounds agent.name, in characters.
const maxAgentNameLen = 64

// AgentConfig is the agent identity block.
type AgentConfig struct {
	// ID is a required, immutable UUIDv7. It keys the Store key's vault entry
	// and binds the Secret store to this configuration.
	ID string `yaml:"id"`
	// Name is an optional free-text label (≤64 characters); the hostname is
	// shown when it is absent.
	Name string `yaml:"name"`
}

// SecretsConfig configures the Store key location.
type SecretsConfig struct {
	// StoreKey is agent_dir (default), os_vault or file.
	StoreKey string `yaml:"store_key"`
	// StoreKeyFile is the absolute external key file (store_key: file only).
	StoreKeyFile string `yaml:"store_key_file"`
}

// ErrAgentIDMissing reports a configuration without agent.id. Boot and
// `ready secrets` refuse it and point at the init repair.
var ErrAgentIDMissing = errors.New("agent.id is missing: run `ready init` to repair the agent identity")

// RequireAgentID returns the validated agent.id, or ErrAgentIDMissing.
func (c *Config) RequireAgentID() (string, error) {
	if c.Agent.ID == "" {
		return "", ErrAgentIDMissing
	}

	return c.Agent.ID, nil
}

// AgentDisplayName is agent.name, falling back to the host name.
func (c *Config) AgentDisplayName() string {
	if c.Agent.Name != "" {
		return c.Agent.Name
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}

	return ""
}

// StoreKeySpec maps the secrets block onto the store's key location.
func (c *Config) StoreKeySpec() secretstore.KeySpec { return SecretsSpec(c.Secrets, c.Agent.ID) }

// validateAgentAndSecrets is the structural check of the identity and key
// location blocks plus every `!secret_file` reference. It performs no secret
// I/O. agent.id is required and must be a UUID; a configuration without one
// still decodes (DecodeForRepair) so `ready init` can repair it.
func (c *Config) validateAgentAndSecrets() error {
	if _, err := c.RequireAgentID(); err != nil {
		return err
	}
	if _, err := uuid.Parse(c.Agent.ID); err != nil {
		return errors.New("agent.id must be a UUID")
	}
	if utf8.RuneCountInString(c.Agent.Name) > maxAgentNameLen {
		return fmt.Errorf("agent.name must be at most %d characters", maxAgentNameLen)
	}
	if err := c.StoreKeySpec().Validate(); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	for _, r := range c.SecretReferences() {
		if r.Ref.Kind != SecretRefFile || c.RuntimeHome == "" {
			continue
		}
		if within(c.RuntimeHome, r.Ref.Path) {
			return fmt.Errorf("%s: a !secret_file inside the agent directory is not allowed; use `!secret <name>` (see `ready secrets set`)", r.DottedPath())
		}
	}

	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// SecretSource resolves a Secret reference to its value for one component.
// It is the only place a reference becomes a value.
type SecretSource struct {
	store *secretstore.Store
	// stat-cached by the store; file references are read on every call.
}

// Store returns the underlying Secret store (nil when the agent has none).
func (s *SecretSource) Store() *secretstore.Store {
	if s == nil {
		return nil
	}

	return s.store
}

// NewSecretSource builds a source over the agent's Store. store may be nil
// when the agent has no Store (every `!secret` is then absent).
func NewSecretSource(store *secretstore.Store) *SecretSource { return &SecretSource{store: store} }

// OpenSecretStore opens the agent's Secret store from a decoded configuration
// (no I/O until used). readOnly forbids creating the store or key.
func (c *Config) OpenSecretStore(log zerolog.Logger, readOnly bool) (*secretstore.Store, error) {
	id, err := c.RequireAgentID()
	if err != nil {
		return nil, err
	}
	s, err := secretstore.New(secretstore.Options{Dir: c.RuntimeHome, Key: c.StoreKeySpec(), AgentID: id, ReadOnly: readOnly, Log: log})
	if err != nil {
		return nil, fmt.Errorf("failed to open the secret store: %w", err)
	}

	return s, nil
}

// Lookup returns the value behind ref. ok=false means the value is absent or
// non-conforming — the reference is pending. An unreadable store is an error.
func (s *SecretSource) Lookup(ctx context.Context, ref SecretRef, field string) (value string, ok bool, err error) {
	switch ref.Kind {
	case SecretRefName:
		if s == nil || s.store == nil {
			return "", false, nil
		}
		p, err := s.store.View(ctx)
		if errors.Is(err, secretstore.ErrNoStore) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("failed to read secret %q: %w", ref.Name, err)
		}
		sec, found := p.Secrets[ref.Name]
		if !found {
			return "", false, nil
		}
		if cerr := secretstore.CheckValue([]string{field}, sec.Value); cerr != nil {
			return "", false, nil //nolint:nilerr // a non-conforming value behaves as pending, never as a failure
		}

		return sec.Value, true, nil
	case SecretRefFile:
		v, ok, err := secretstore.ReadFileSecret(ref.Path)
		if err != nil || !ok || secretstore.CheckValue([]string{field}, v) != nil {
			return "", false, err
		}

		return v, true, nil
	default:
		return "", false, nil
	}
}

// ErrSecretPending reports a reference whose value is absent or
// non-conforming. It never carries a value.
var ErrSecretPending = errors.New("secret is pending")

// ResolveAPIKey returns the provider's API key, or "" when the provider
// declares no api_key reference (ollama, openai-codex). A declared reference
// without a usable value wraps ErrSecretPending and names the Secret.
func (l *LLMConfig) ResolveAPIKey(ctx context.Context, providerName string) (string, error) {
	p, ok := l.Providers[providerName]
	if !ok || !p.APIKey.IsSet() {
		return "", nil
	}
	field := "llm.providers." + providerName + ".api_key"
	v, found, err := l.Secrets.Lookup(ctx, p.APIKey, field)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s references %s", ErrSecretPending, field, p.APIKey.String())
	}

	return v, nil
}

// ResolveHeaders resolves every header reference of one MCP server. Headers
// whose Secret is pending are left out and returned by name, so the server's
// tools can be skipped with a precise reason instead of sending a request
// with a missing credential.
func (m *MCPServerConfig) ResolveHeaders(ctx context.Context, src *SecretSource, serverKey string) (pending []string, err error) {
	resolved := make(map[string]string, len(m.Headers))
	for name, ref := range m.Headers {
		field := "mcp_servers." + serverKey + ".headers." + name
		v, ok, lerr := src.Lookup(ctx, ref, field)
		if lerr != nil {
			return nil, lerr
		}
		if !ok {
			pending = append(pending, field)

			continue
		}
		resolved[name] = v
	}
	m.ResolvedHeaders = resolved
	sort.Strings(pending)

	return pending, nil
}

// ResolveSecret resolves one reference of this configuration by its dotted
// field path. ok=false means pending (absent, non-conforming, or a missing
// external file); an unreadable store is an error.
func (c *Config) ResolveSecret(ctx context.Context, ref SecretRef, field string) (value string, ok bool, err error) {
	return c.SecretValues.Lookup(ctx, ref, field)
}

// SecretStoreReferences renders the configuration's references in the form
// the Secret store derives pending state from.
func (c *Config) SecretStoreReferences() []secretstore.Reference {
	refs := c.SecretReferences()
	out := make([]secretstore.Reference, 0, len(refs))
	for _, r := range refs {
		item := secretstore.Reference{Field: r.DottedPath()}
		switch r.Ref.Kind {
		case SecretRefName:
			item.Name = r.Ref.Name
		case SecretRefFile:
			item.File = r.Ref.Path
		default:
			continue
		}
		out = append(out, item)
	}

	return out
}

// SetStoreKeyLocation rewrites the `secrets:` block of a configuration
// document so it names the given Store key location (and file, for
// `file`). It edits text conservatively — every other byte of the document is
// kept — and is the one config edit `ready secrets move-key` performs.
func SetStoreKeyLocation(data []byte, location, file string) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	block := []string{"secrets:", "  store_key: " + location}
	if file != "" {
		block = append(block, "  store_key_file: "+file)
	}
	start := -1
	for i, l := range lines {
		if strings.TrimRight(l, " \t") == "secrets:" {
			start = i

			break
		}
		if strings.HasPrefix(l, "secrets:") {
			return nil, errors.New("the secrets block is written inline; edit it by hand")
		}
	}
	if start < 0 {
		text := strings.TrimRight(string(data), "\n")

		return []byte(text + "\n" + strings.Join(block, "\n") + "\n"), nil
	}
	end := start + 1
	for end < len(lines) && (strings.HasPrefix(lines[end], " ") || strings.HasPrefix(lines[end], "\t") || strings.TrimSpace(lines[end]) == "") {
		end++
	}
	// Keep unrelated children (comments, future keys); drop the two we own.
	var kept []string
	for _, l := range lines[start+1 : end] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "store_key:") || strings.HasPrefix(t, "store_key_file:") {
			continue
		}
		kept = append(kept, l)
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, block...)
	out = append(out, kept...)
	out = append(out, lines[end:]...)

	return []byte(strings.Join(out, "\n")), nil
}

package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"

	"github.com/marco-rozz/ready/internal/secretstore"
)

// SecretImporter is implemented only by the v1→v2 step. Alongside the pure
// Migration.Up, it plans the import of credential values that v1 kept inline or
// in agent-local files into the agent's Secret store. Every other (future) step
// stays pure: it must not read files or write a store.
type SecretImporter interface {
	// PlanImport rewrites the sensitive fields of doc into references and
	// reports the values to store and the plaintext files to delete after the
	// new configuration is committed. It reads files; it writes nothing.
	PlanImport(doc *yaml.Node, ic ImportContext) (ImportPlan, error)
}

// ImportContext is what an importer may know about the agent.
type ImportContext struct {
	// AgentDir is the physical agent directory (the config's directory).
	AgentDir string
	// ExistingAgentID is the agent_id of an already existing store ("" when
	// none): a retried migration reuses it instead of minting a second identity.
	ExistingAgentID string
}

// ImportItem is one value to store under one name.
type ImportItem struct {
	Name  string
	Field string
	Value string
}

// ImportPlan is the outcome of PlanImport. Values never appear in errors.
type ImportPlan struct {
	Items []ImportItem
	// Cleanup lists absolute paths to delete once the new config is committed.
	Cleanup []string
	// AgentID is the identity the rewritten document carries.
	AgentID string
}

// ErrSecretImport reports a refused import; it never carries a value.
var ErrSecretImport = errors.New("secret import refused")

// importIntoStore writes the planned values idempotently under the store lock:
// the same value under a name is fine, a different one stops the migration.
// It also records the cleanup list, so a crash after the config commit is
// finished at the next boot.
func importIntoStore(ctx context.Context, dir string, plan ImportPlan) (*secretstore.Store, error) {
	store, err := secretstore.New(secretstore.Options{
		Dir: dir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, AgentID: plan.AgentID, Log: zerolog.Nop(),
	})
	if err != nil {
		return nil, err
	}
	for _, it := range plan.Items {
		if vErr := secretstore.CheckValue([]string{it.Field}, it.Value); vErr != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrSecretImport, it.Field, vErr)
		}
	}
	err = store.Update(ctx, func(p *secretstore.Payload) error {
		for _, it := range plan.Items {
			if cur, ok := p.Secrets[it.Name]; ok && cur.Value != it.Value {
				return fmt.Errorf("%w: a different secret named %q already exists in the store", ErrSecretImport, it.Name)
			}
		}
		now := nowUTC()
		for _, it := range plan.Items {
			p.Secrets[it.Name] = secretstore.Secret{Value: it.Value, UpdatedAt: now}
		}
		for _, c := range plan.Cleanup {
			if !containsString(p.Cleanup, c) {
				p.Cleanup = append(p.Cleanup, c)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return store, nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}

	return false
}

// probeExistingAgentID returns the agent_id of an existing store, or "".
func probeExistingAgentID(ctx context.Context, dir string) (string, error) {
	probe, err := secretstore.New(secretstore.Options{
		Dir: dir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, ReadOnly: true, Log: zerolog.Nop(),
	})
	if err != nil {
		return "", err
	}
	if !probe.Exists() {
		return "", nil
	}
	p, err := probe.View(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: an existing secret store cannot be opened (%w); restore its key with `ready secrets import-key` or move the store away", ErrSecretImport, err)
	}

	return p.AgentID, nil
}

// RunSecretCleanup deletes the plaintext files a migration recorded in the
// store's cleanup list (imported agent-local files, READY's earlier plaintext
// config backups). It is retried at every boot: a failure WARNs and keeps the
// path listed, never rolls anything back. Only recorded paths and READY's own
// plaintext backup names of the config are ever removed.
func RunSecretCleanup(ctx context.Context, store *secretstore.Store, configPath string, logger zerolog.Logger) {
	payload, err := store.View(ctx)
	if err != nil {
		return
	}
	pending := append([]string(nil), payload.Cleanup...)
	for _, name := range plaintextBackups(configPath) {
		if !containsString(pending, name) {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		return
	}
	var done []string
	for _, path := range pending {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			logger.Warn().Err(rerr).Str("file", path).Msg("could not delete a plaintext file left by the secret migration; delete it by hand")

			continue
		}
		done = append(done, path)
	}
	if len(payload.Cleanup) == 0 {
		return
	}
	_ = store.Update(ctx, func(p *secretstore.Payload) error {
		kept := p.Cleanup[:0]
		for _, c := range p.Cleanup {
			if !containsString(done, c) {
				kept = append(kept, c)
			}
		}
		p.Cleanup = kept

		return nil
	})
}

// plaintextBackups lists READY's own plaintext backups of the config
// (`<config>.bak.<stamp>.<digits>`, not `.age`).
func plaintextBackups(configPath string) []string {
	dir := filepath.Dir(configPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	prefix := filepath.Base(configPath) + ".bak."
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || strings.HasSuffix(name, backupEncryptedSuffix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if len(suffix) <= len(backupTimeFormat)+1 || suffix[len(backupTimeFormat)] != '.' {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}

	return out
}

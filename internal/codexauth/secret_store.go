package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// credentialKeyPrefix namespaces Codex Credentials inside the Secret store;
// MCP OAuth Credentials use "mcp:".
const credentialKeyPrefix = "llm:"

// SecretStore keeps one encrypted Codex Credential per named LLM provider in
// the agent's Secret store. Per-target coordination locks (under
// <agent-dir>/oauth/locks) are always taken BEFORE the store lock.
type SecretStore struct {
	secrets *secretstore.Store
	dir     string
}

// Guarantees at compile time that SecretStore implements Store.
var _ Store = (*SecretStore)(nil)

// NewSecretStore wraps the agent's Secret store. coordDir is the directory
// whose "locks" subdirectory holds the per-target coordination locks.
func NewSecretStore(secrets *secretstore.Store, coordDir string) *SecretStore {
	return &SecretStore{secrets: secrets, dir: coordDir}
}

func credentialKey(providerKey string) string { return credentialKeyPrefix + providerKey }

// Load returns the stored Credential, or (nil, nil) for an absent login.
func (s *SecretStore) Load(ctx context.Context, providerKey string) (*Credential, error) {
	raw, gen, ok, err := s.secrets.Credential(ctx, credentialKey(providerKey))
	if err != nil {
		return nil, fmt.Errorf("load codex credential for %q: %w", providerKey, err)
	}
	if !ok {
		return nil, nil //nolint:nilnil // an absent login is a valid store result
	}
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("parse codex credential for %q: %w", providerKey, err)
	}
	cred.Generation = int64(gen) //nolint:gosec // generations are small monotonic counters

	return &cred, nil
}

// Save serializes on the target's coordination lock and bumps the generation.
func (s *SecretStore) Save(ctx context.Context, cred *Credential) error {
	return s.save(ctx, cred, nil)
}

// SaveCommitted commits cred only while the target's generation still equals
// the snapshot; divergence fails with ErrGenerationConflict.
func (s *SecretStore) SaveCommitted(ctx context.Context, cred *Credential, snapshotGen int64) error {
	expect := uint64(snapshotGen) //nolint:gosec // generations are non-negative

	return s.save(ctx, cred, &expect)
}

func (s *SecretStore) save(ctx context.Context, cred *Credential, expect *uint64) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("save codex credential: %w", err)
	}
	if cred == nil || cred.ProviderKey == "" {
		return errors.New("save codex credential: provider key is required")
	}
	lock, err := s.lockTarget(ctx, cred.ProviderKey, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	_, err = s.secrets.PutCredential(ctx, credentialKey(cred.ProviderKey), expect, func(gen uint64) (json.RawMessage, error) {
		cred.Generation = int64(gen) //nolint:gosec // generations are small monotonic counters

		return json.Marshal(cred) //nolint:gosec // the Credential is sealed into the encrypted Secret store by design
	})
	if errors.Is(err, secretstore.ErrGenerationConflict) {
		return fmt.Errorf("%w: %w", ErrGenerationConflict, err)
	}
	if err != nil {
		return fmt.Errorf("save codex credential for %q: %w", cred.ProviderKey, err)
	}

	return nil
}

// Delete removes the Credential and leaves a tombstone.
func (s *SecretStore) Delete(ctx context.Context, providerKey string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete codex credential: %w", err)
	}
	lock, err := s.lockTarget(ctx, providerKey, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	if err := s.secrets.RemoveCredential(ctx, credentialKey(providerKey)); err != nil {
		return fmt.Errorf("delete codex credential for %q: %w", providerKey, err)
	}

	return nil
}

// Snapshot returns the record's generation or its tombstone's (0 when neither).
func (s *SecretStore) Snapshot(ctx context.Context, providerKey string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("snapshot codex credential: %w", err)
	}
	gen, err := s.secrets.Generation(ctx, credentialKey(providerKey))
	if err != nil {
		return 0, fmt.Errorf("snapshot codex credential for %q: %w", providerKey, err)
	}

	return int64(gen), nil //nolint:gosec // generations are small monotonic counters
}

func (s *SecretStore) lockTarget(ctx context.Context, providerKey string, shared bool) (*daemonlock.Lock, error) {
	path := filepath.Join(s.dir, locksDir, filenameFor(providerKey)+".lock")
	lock, err := daemonlock.WaitForLock(ctx, path, shared, targetLockWait)
	if errors.Is(err, daemonlock.ErrOccupied) {
		return nil, fmt.Errorf("%w: %s", ErrTargetLockBusy, providerKey)
	}

	return lock, err
}

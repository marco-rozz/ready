package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/marco-rozz/ready/internal/daemonlock"
	"github.com/marco-rozz/ready/internal/secretstore"
)

// credentialKeyPrefix namespaces MCP OAuth Credentials inside the Secret store.
const credentialKeyPrefix = "mcp:"

// unsafeServerKeyChars matches any character not safe to use verbatim in a
// filename. It sanitizes the per-target coordination lock name, so a server
// key can never escape the locks directory.
var unsafeServerKeyChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeServerKey returns a filesystem-safe filename stem for serverKey.
func sanitizeServerKey(serverKey string) string {
	sanitized := unsafeServerKeyChars.ReplaceAllString(serverKey, "_")
	if sanitized == "" {
		sanitized = "_"
	}

	return sanitized
}

// SecretStore is the Store implementation over the agent's Secret store: one
// encrypted Credential per MCP server, with a per-entry generation and
// tombstone. Per-target coordination locks (under <agent-dir>/oauth/locks)
// guard long network operations and are always taken BEFORE the store lock,
// never inside it.
type SecretStore struct {
	secrets *secretstore.Store
	// dir is the oauth coordination directory holding the per-target locks.
	dir string
}

// Guarantees at compile time that SecretStore implements Store.
var _ Store = (*SecretStore)(nil)

// NewSecretStore wraps the agent's Secret store. coordDir is the directory
// whose "locks" subdirectory holds the per-target coordination locks.
func NewSecretStore(secrets *secretstore.Store, coordDir string) *SecretStore {
	return &SecretStore{secrets: secrets, dir: coordDir}
}

func credentialKey(serverKey string) string { return credentialKeyPrefix + serverKey }

// Load implements Store: the record as it currently lies in the store, with
// its generation, or (nil, nil) when absent.
func (s *SecretStore) Load(ctx context.Context, serverKey string) (*Credential, error) {
	raw, gen, ok, err := s.secrets.Credential(ctx, credentialKey(serverKey))
	if err != nil {
		return nil, fmt.Errorf("failed to load oauth credential for %q: %w", serverKey, err)
	}
	if !ok {
		return nil, nil //nolint:nilnil // absent record is a valid, non-error outcome (Store.Load contract)
	}
	var cred Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, fmt.Errorf("failed to parse oauth credential for %q: %w", serverKey, err)
	}
	cred.Generation = int64(gen) //nolint:gosec // generations are small monotonic counters

	return &cred, nil
}

// Save implements Store: serializes on the target's coordination lock and
// bumps the record's generation.
func (s *SecretStore) Save(ctx context.Context, cred *Credential) error {
	return s.save(ctx, cred, nil)
}

// SaveCommitted commits cred only when the target's generation still equals
// the snapshot taken before the caller's interactive work; on divergence it
// fails with ErrGenerationConflict instead of overwriting newer state.
func (s *SecretStore) SaveCommitted(ctx context.Context, cred *Credential, snapshotGen int64) error {
	expect := uint64(snapshotGen) //nolint:gosec // generations are non-negative

	return s.save(ctx, cred, &expect)
}

func (s *SecretStore) save(ctx context.Context, cred *Credential, expect *uint64) error {
	if cred == nil || cred.ServerKey == "" {
		return errors.New("failed to save oauth credential: server key is required") // programmer error, not user-facing config
	}
	lock, err := s.lockTarget(ctx, cred.ServerKey, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	_, err = s.secrets.PutCredential(ctx, credentialKey(cred.ServerKey), expect, func(gen uint64) (json.RawMessage, error) {
		cred.Generation = int64(gen)    //nolint:gosec // generations are small monotonic counters
		raw, mErr := json.Marshal(cred) //nolint:gosec // the Credential is sealed into the encrypted Secret store by design
		if mErr != nil {
			return nil, fmt.Errorf("failed to marshal oauth credential for %q: %w", cred.ServerKey, mErr)
		}

		return raw, nil
	})
	if errors.Is(err, secretstore.ErrGenerationConflict) {
		return fmt.Errorf("%w: %w", ErrGenerationConflict, err)
	}
	if err != nil {
		return fmt.Errorf("failed to save oauth credential for %q: %w", cred.ServerKey, err)
	}

	return nil
}

// Delete implements Store: it serializes on the target's coordination lock
// and leaves a tombstone, so stale refresh/login work can neither resurrect
// the record nor serve its cached generation as current.
func (s *SecretStore) Delete(ctx context.Context, serverKey string) error {
	lock, err := s.lockTarget(ctx, serverKey, false)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	if err := s.secrets.RemoveCredential(ctx, credentialKey(serverKey)); err != nil {
		return fmt.Errorf("failed to delete oauth credential for %q: %w", serverKey, err)
	}

	return nil
}

// Snapshot returns the target's current generation: the stored record's, or
// the tombstone generation of a forgotten record (0 when neither exists).
func (s *SecretStore) Snapshot(ctx context.Context, serverKey string) (int64, error) {
	gen, err := s.secrets.Generation(ctx, credentialKey(serverKey))
	if err != nil {
		return 0, fmt.Errorf("failed to snapshot oauth credential for %q: %w", serverKey, err)
	}

	return int64(gen), nil //nolint:gosec // generations are small monotonic counters
}

// lockTarget acquires the target's coordination lock, polling non-blocking
// acquisitions until the bounded budget or the caller's context expires.
func (s *SecretStore) lockTarget(ctx context.Context, serverKey string, shared bool) (*daemonlock.Lock, error) {
	path := filepath.Join(s.dir, locksDir, sanitizeServerKey(serverKey)+".lock")
	lock, err := daemonlock.WaitForLock(ctx, path, shared, targetLockWait)
	if errors.Is(err, daemonlock.ErrOccupied) {
		return nil, fmt.Errorf("%w: %s", ErrTargetLockBusy, serverKey)
	}

	return lock, err
}

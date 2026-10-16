package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// unsafeServerKeyChars matches any character not safe to use verbatim in a
// filename. Anything outside this allowlist (including "/", "\", and "..")
// is replaced, so a server key cannot be used to escape dir via path
// traversal.
var unsafeServerKeyChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// sanitizeServerKey returns a filesystem-safe filename stem for serverKey.
func sanitizeServerKey(serverKey string) string {
	sanitized := unsafeServerKeyChars.ReplaceAllString(serverKey, "_")
	if sanitized == "" {
		sanitized = "_"
	}

	return sanitized
}

// FileStore is the file-backed Store implementation: one JSON
// record per server under dir, mode 0600 in a 0700 directory, written to a
// temp file and os.Rename'd into place (atomic on POSIX).
type FileStore struct {
	dir string
	mu  sync.RWMutex
}

// Guarantees at compile time that FileStore implements Store.
var _ Store = (*FileStore)(nil)

// NewFileStore creates (or reuses) a 0700 directory at path and returns a
// Store backed by it.
func NewFileStore(ctx context.Context, path string) (*FileStore, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrStoreUnavailable, ctx.Err())
	default:
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to resolve oauth store path: %w", ErrStoreUnavailable, err)
	}

	if err := os.MkdirAll(absPath, 0o700); err != nil {
		return nil, fmt.Errorf("%w: failed to create oauth store directory: %w", ErrStoreUnavailable, err)
	}

	// criticalities-remediation (LOW secret-handling): MkdirAll does not tighten the
	// permissions of a PRE-EXISTING directory — enforce 0700 so an operator
	// directory created with looser mode cannot expose stored credentials.
	if info, statErr := os.Stat(absPath); statErr == nil && info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(absPath, 0o700)
	}

	return &FileStore{dir: absPath}, nil
}

func (s *FileStore) pathFor(serverKey string) string {
	return filepath.Join(s.dir, sanitizeServerKey(serverKey)+".json")
}

// Load implements Store.
func (s *FileStore) Load(_ context.Context, serverKey string) (*Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := os.ReadFile(s.pathFor(serverKey))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // absent record is a valid, non-error outcome (Store.Load contract)
		}

		return nil, fmt.Errorf("failed to load oauth credential for %q: %w", serverKey, err)
	}

	var cred Credential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, fmt.Errorf("failed to parse oauth credential for %q: %w", serverKey, err)
	}

	return &cred, nil
}

// Save implements Store.
func (s *FileStore) Save(_ context.Context, cred *Credential) error {
	if cred == nil || cred.ServerKey == "" {
		return fmt.Errorf("failed to save oauth credential: server key is required") // programmer error, not user-facing config
	}

	data, err := json.MarshalIndent(cred, "", "  ") //nolint:gosec // credential is written to disk by design (0600 in a 0700 dir), not logged
	if err != nil {
		return fmt.Errorf("failed to marshal oauth credential for %q: %w", cred.ServerKey, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.pathFor(cred.ServerKey)
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write temporary oauth credential for %q: %w", cred.ServerKey, err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to commit oauth credential for %q: %w", cred.ServerKey, err)
	}

	return nil
}

// Delete implements Store.
func (s *FileStore) Delete(_ context.Context, serverKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(s.pathFor(serverKey)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("failed to delete oauth credential for %q: %w", serverKey, err)
	}

	return nil
}

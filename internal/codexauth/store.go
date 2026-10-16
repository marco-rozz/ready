package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrStoreUnavailable = errors.New("codex oauth credential store unavailable")

// Store persists one Codex credential per named LLM provider.
type Store interface {
	Load(ctx context.Context, providerKey string) (*Credential, error)
	Save(ctx context.Context, cred *Credential) error
	Delete(ctx context.Context, providerKey string) error
}

// FileStore uses the same directory and permission guarantees as MCP OAuth,
// but its llm- prefix and reversible base64url key make namespace collisions
// and path traversal impossible.
type FileStore struct {
	dir string
	mu  sync.RWMutex
}

var _ Store = (*FileStore)(nil)

func NewFileStore(ctx context.Context, path string) (*FileStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStoreUnavailable, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve path: %w", ErrStoreUnavailable, err)
	}
	if err := os.MkdirAll(absPath, 0o700); err != nil {
		return nil, fmt.Errorf("%w: create directory: %w", ErrStoreUnavailable, err)
	}
	if info, statErr := os.Stat(absPath); statErr == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(absPath, 0o700); err != nil {
			return nil, fmt.Errorf("%w: restrict directory permissions: %w", ErrStoreUnavailable, err)
		}
	}

	return &FileStore{dir: absPath}, nil
}

func filenameFor(providerKey string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(providerKey))

	return "llm-" + encoded + ".json"
}

func (s *FileStore) pathFor(providerKey string) string {
	return filepath.Join(s.dir, filenameFor(providerKey))
}

func (s *FileStore) Load(ctx context.Context, providerKey string) (*Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load codex credential: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.pathFor(providerKey))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // an absent login is a valid store result
		}

		return nil, fmt.Errorf("load codex credential for %q: %w", providerKey, err)
	}
	var cred Credential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, fmt.Errorf("parse codex credential for %q: %w", providerKey, err)
	}

	return &cred, nil
}

func (s *FileStore) Save(ctx context.Context, cred *Credential) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("save codex credential: %w", err)
	}
	if cred == nil || cred.ProviderKey == "" {
		return fmt.Errorf("save codex credential: provider key is required")
	}
	if cred.Version == 0 {
		cred.Version = credentialVersion
	}
	data, err := json.MarshalIndent(cred, "", "  ") //nolint:gosec // encrypted transport secrets are intentionally persisted in a 0600 file
	if err != nil {
		return fmt.Errorf("marshal codex credential for %q: %w", cred.ProviderKey, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.pathFor(cred.ProviderKey)
	tmp, err := os.CreateTemp(s.dir, ".llm-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary codex credential for %q: %w", cred.ProviderKey, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("restrict temporary codex credential for %q: %w", cred.ProviderKey, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write temporary codex credential for %q: %w", cred.ProviderKey, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("sync temporary codex credential for %q: %w", cred.ProviderKey, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary codex credential for %q: %w", cred.ProviderKey, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit codex credential for %q: %w", cred.ProviderKey, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict codex credential for %q: %w", cred.ProviderKey, err)
	}

	return nil
}

func (s *FileStore) Delete(ctx context.Context, providerKey string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("delete codex credential: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.pathFor(providerKey)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete codex credential for %q: %w", providerKey, err)
	}

	return nil
}

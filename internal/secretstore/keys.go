package secretstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"filippo.io/age"
)

// File names under the agent directory.
const (
	StoreFileName = "secrets.age"
	prevSuffix    = ".prev"
	KeyFileName   = "secrets.key"
	lockFileName  = ".secrets.lock"

	vaultService = "ready"
)

var (
	// ErrKeyNotFound reports that the configured Store key location holds no key.
	ErrKeyNotFound = errors.New("secret store: store key not found")
	// ErrKeyReadOnly reports an attempt to write a key to an external file.
	ErrKeyReadOnly = errors.New("secret store: the store key file is read-only for READY")
	// ErrVaultUnsupported reports os_vault on a platform without a vault backend.
	ErrVaultUnsupported = errors.New("secret store: os_vault is only available on macOS and Windows; use store_key: file")
	// ErrKeyExists refuses to overwrite an existing key.
	ErrKeyExists = errors.New("secret store: a store key already exists at this location")
)

// KeyLocation says where the Store key lives.
type KeyLocation string

const (
	// KeyAgentDir keeps the key beside the store in <agent-dir>/secrets.key.
	KeyAgentDir KeyLocation = "agent_dir"
	// KeyOSVault keeps it in the macOS Keychain or Windows Credential Manager.
	KeyOSVault KeyLocation = "os_vault"
	// KeyFile reads it from an external absolute file (e.g. a systemd credential).
	KeyFile KeyLocation = "file"
)

// KeySpec is the configured Store key location.
type KeySpec struct {
	Location KeyLocation
	// File is the absolute path for KeyFile.
	File string
	// AgentID keys the vault entry (account / target) for KeyOSVault.
	AgentID string
}

// Validate checks the location structurally; it performs no I/O.
func (k KeySpec) Validate() error {
	switch k.normalized() {
	case KeyAgentDir:
		if k.File != "" {
			return errors.New("secrets.store_key_file is only valid with store_key: file")
		}
	case KeyOSVault:
		if !vaultSupported {
			return ErrVaultUnsupported
		}
		if k.File != "" {
			return errors.New("secrets.store_key_file is only valid with store_key: file")
		}
		if !safeToken(k.AgentID) {
			return errors.New("store_key: os_vault needs a valid agent.id")
		}
	case KeyFile:
		if !filepath.IsAbs(k.File) {
			return errors.New("secrets.store_key_file must be an absolute path")
		}
	default:
		return fmt.Errorf("secrets.store_key %q is not one of agent_dir, os_vault, file", k.Location)
	}

	return nil
}

func (k KeySpec) normalized() KeyLocation {
	if k.Location == "" {
		return KeyAgentDir
	}

	return k.Location
}

var safeTokenPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// safeToken accepts only characters that need no quoting in a `security -i`
// command line (UUIDs, bech32 age keys).
func safeToken(s string) bool { return safeTokenPattern.MatchString(s) }

// keyBackend is one Store key location.
type keyBackend interface {
	load(ctx context.Context) (*age.X25519Identity, error)
	// save installs a key; it refuses to overwrite one.
	save(ctx context.Context, id *age.X25519Identity) error
	remove(ctx context.Context) error
}

func newBackend(dir string, k KeySpec) (keyBackend, error) {
	if err := k.Validate(); err != nil {
		return nil, err
	}
	switch k.normalized() {
	case KeyOSVault:
		return vaultKey{account: k.AgentID}, nil
	case KeyFile:
		return fileKey{path: k.File}, nil
	default:
		return dirKey{path: filepath.Join(dir, KeyFileName)}, nil
	}
}

// ---- agent_dir

type dirKey struct{ path string }

func (d dirKey) load(ctx context.Context) (*age.X25519Identity, error) {
	return readKeyFile(ctx, d.path)
}

func (d dirKey) save(_ context.Context, id *age.X25519Identity) error {
	if _, err := os.Lstat(d.path); err == nil {
		return ErrKeyExists
	}
	f, err := createPrivate(d.path)
	if err != nil {
		return fmt.Errorf("failed to create store key file: %w", err)
	}
	_, err = f.WriteString(id.String() + "\n")
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(d.path)

		return fmt.Errorf("failed to write store key file: %w", err)
	}

	return nil
}

func (d dirKey) remove(context.Context) error {
	if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove store key file: %w", err)
	}

	return nil
}

// ---- file (external, read-only)

type fileKey struct{ path string }

func (f fileKey) load(ctx context.Context) (*age.X25519Identity, error) {
	return readKeyFile(ctx, f.path)
}
func (fileKey) save(context.Context, *age.X25519Identity) error { return ErrKeyReadOnly }

// remove never touches an external file.
func (fileKey) remove(context.Context) error { return nil }

func readKeyFile(ctx context.Context, path string) (*age.X25519Identity, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read store key: %w", err)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read store key: %w", err)
	}

	return ParseIdentity(string(raw))
}

// ---- os_vault

type vaultKey struct{ account string }

func (v vaultKey) load(ctx context.Context) (*age.X25519Identity, error) {
	s, err := vaultGet(ctx, v.account)
	if err != nil {
		return nil, err
	}

	return ParseIdentity(s)
}

func (v vaultKey) save(ctx context.Context, id *age.X25519Identity) error {
	if _, err := vaultGet(ctx, v.account); err == nil {
		return ErrKeyExists
	} else if !errors.Is(err, ErrKeyNotFound) {
		return err
	}

	return vaultSet(ctx, v.account, id.String())
}

func (v vaultKey) remove(ctx context.Context) error { return vaultDelete(ctx, v.account) }

package secretstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// WriteNewPrivateFile creates path exclusively (never over an existing file),
// user-only (0600, or a user-only DACL on Windows), writes data and syncs it.
// It is the only way `ready secrets reveal --to-file` writes a value.
func WriteNewPrivateFile(path string, data []byte) error {
	f, err := createPrivate(path)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", path, err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(path)

		return fmt.Errorf("failed to write %s: %w", path, err)
	}

	return nil
}

// Identity loads the Store key from its configured location.
func (s *Store) Identity(ctx context.Context) (*age.X25519Identity, error) {
	return s.backend.load(ctx)
}

// VerifyKey checks that id decrypts the existing store (nil when there is no
// store yet: nothing to contradict).
func (s *Store) VerifyKey(id *age.X25519Identity) error {
	if !s.Exists() {
		return nil
	}
	if _, err := s.readFile(id, s.path()); err != nil {
		if _, perr := s.readFile(id, s.prevPath()); perr != nil {
			return fmt.Errorf("this key does not decrypt the store: %w", err)
		}
	}

	return nil
}

// InstallKey puts id at the configured location after verifying it against the
// existing store. It never overwrites a key already there.
func (s *Store) InstallKey(ctx context.Context, id *age.X25519Identity) error {
	if err := s.VerifyKey(id); err != nil {
		return err
	}

	return s.backend.save(ctx, id)
}

// RemoveKey deletes the key at spec's location: the agent_dir file or the vault
// entry. An external `file` is never touched.
func RemoveKey(ctx context.Context, dir string, spec KeySpec) error {
	b, err := newBackend(dir, spec)
	if err != nil {
		return err
	}

	return b.remove(ctx)
}

// WrapIdentity encrypts the textual Store key under a passphrase (age scrypt,
// ASCII-armored): the portable export format.
func WrapIdentity(id *age.X25519Identity, passphrase string) ([]byte, error) {
	rec, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("invalid passphrase: %w", err)
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, rec)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap the key: %w", err)
	}
	if _, err = io.WriteString(w, id.String()+"\n"); err != nil {
		return nil, fmt.Errorf("failed to wrap the key: %w", err)
	}
	if err = errors.Join(w.Close(), aw.Close()); err != nil {
		return nil, fmt.Errorf("failed to wrap the key: %w", err)
	}

	return buf.Bytes(), nil
}

// IsWrapped reports whether data is a passphrase-wrapped export.
func IsWrapped(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(data), []byte(armor.Header))
}

// UnwrapIdentity reverses WrapIdentity.
func UnwrapIdentity(data []byte, passphrase string) (*age.X25519Identity, error) {
	ident, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("invalid passphrase: %w", err)
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(data)), ident)
	if err != nil {
		return nil, fmt.Errorf("cannot unwrap the key (wrong passphrase?): %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("cannot unwrap the key: %w", err)
	}

	return ParseIdentity(string(plain))
}

// InstallKeyAt installs id at dest for the store living in dir and confirms
// the store decrypts through that new location. It is the install step of
// `move-key`; the caller commits the configuration and removes the old copy.
func InstallKeyAt(ctx context.Context, dir string, dest KeySpec, agentID string, id *age.X25519Identity) error {
	dst, err := New(Options{Dir: dir, Key: dest, AgentID: agentID})
	if err != nil {
		return err
	}
	if err = dst.InstallKey(ctx, id); err != nil && !errors.Is(err, ErrKeyReadOnly) {
		return err
	}
	loaded, err := dst.Identity(ctx)
	if err != nil {
		return fmt.Errorf("the key is not readable at its new location: %w", err)
	}

	return dst.VerifyKey(loaded)
}

// Seal encrypts data to the store's own recipient: the encrypted config backup
// a migration (or `ready secrets adopt`) leaves behind. `age -d -i secrets.key`
// restores it.
func (s *Store) Seal(ctx context.Context, data []byte) ([]byte, error) {
	id, err := s.backend.load(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot open the store key: %w", err)
	}

	return encrypt(id, data)
}

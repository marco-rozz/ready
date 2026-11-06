package secretstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a private temporary file in the
// same directory: create (user-only), write, fsync, atomic replace, directory
// fsync. A crash leaves either the old or the new file, never a torn one.
func writeFileAtomic(path string, data []byte) error {
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("failed to create temporary name: %w", err)
	}
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(nonce[:]))

	f, err := createPrivate(tmp)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("failed to write temporary file: %w", err)
	}
	if err = replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("failed to replace %s: %w", filepath.Base(path), err)
	}
	if err = syncDir(dir); err != nil {
		return fmt.Errorf("failed to sync directory: %w", err)
	}

	return nil
}

// ensurePrivateDir creates dir (user-only) when missing.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}

	return nil
}

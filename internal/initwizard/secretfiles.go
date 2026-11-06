// Private credential files of design D4: the ZMQ shared secret and the
// provider API key are stored agent-locally under the strongest permissions
// available per platform — POSIX 0700 directories / 0600 files, or on
// Windows a protected DACL granting access only to the current user's SID
// with no inherited ACEs. Every target is created exclusively (no clobber,
// no following links); creation failures are hard failures of publication.
package initwizard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// randomTokenHex renders n random bytes as lowercase hex (crypto/rand; the
// test seam overrides Options.RandomHex).
func randomTokenHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random secret: %w", err)
	}

	return hex.EncodeToString(b), nil
}

// generateZMQTokenByteLength sizes the fresh ZMQ token: 32 random bytes →
// 64 hex characters, twice the 32-character shared-secret minimum.
const generateZMQTokenByteLength = 32

// ensureAgentDir creates the missing agent directory (only init ever does
// so) with 0700. An existing directory is untouched; a non-directory target
// (link, file, special) is a hard refusal.
func ensureAgentDir(ctx context.Context, dir string) error {
	info, lerr := os.Lstat(dir)
	switch {
	case lerr == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0:
		return nil
	case lerr == nil:
		return fmt.Errorf("agent directory %s must be a real directory (found %s)", dir, describeHolder(info))
	case errors.Is(lerr, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create agent directory %s: %w", dir, err)
		}
		if err := applyDirPermissionsForSecurity(ctx, dir); err != nil {
			return err
		}

		return nil
	default:
		return fmt.Errorf("agent directory %s: %w", dir, lerr)
	}
}

// describeHolder renders a non-directory Lstat result safely (kind only).
func describeHolder(info os.FileInfo) string {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "a symlink"
	case info.Mode()&os.ModeNamedPipe != 0:
		return "a named pipe"
	case info.Mode()&os.ModeSocket != 0:
		return "a socket"
	case info.Mode()&os.ModeDevice != 0:
		return "a device"
	default:
		return "a non-directory" // covers regular files and unknown kinds
	}
}

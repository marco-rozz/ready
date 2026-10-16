//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package logging

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking flock on f.
func lockFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s", ErrLogDirLocked, f.Name())
		}

		return fmt.Errorf("failed to lock %s: %w", f.Name(), err)
	}

	return nil
}

// unlockFile releases the flock on f.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

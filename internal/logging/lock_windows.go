//go:build windows

package logging

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, fail-immediately LockFileEx on the first byte
// of f.
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)

	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return fmt.Errorf("%w: %s", ErrLogDirLocked, f.Name())
		}

		return fmt.Errorf("failed to lock %s: %w", f.Name(), err)
	}

	return nil
}

// unlockFile releases the LockFileEx range on f.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

//go:build windows

package daemonlock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock performs the non-blocking lock on f: shared requests a shared
// mode (multiple holders allowed), exclusive admits none.
func tryLock(f *os.File, shared bool) (bool, error) {
	return lockWindows(f, shared)
}

// lockWindows takes a non-blocking LockFileEx on the lock file's handle:
// shared when requested (no LOCKFILE_EXCLUSIVE_LOCK), exclusive otherwise.
func lockWindows(f *os.File, shared bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("LockFileEx: %w", err)
	}

	return true, nil
}

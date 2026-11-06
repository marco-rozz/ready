//go:build unix

package daemonlock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock performs the non-blocking lock on f: shared requests a shared
// mode (multiple holders allowed), exclusive admits none.
func tryLock(f *os.File, shared bool) (bool, error) {
	return lockUnix(f, shared)
}

// lockUnix takes a non-blocking flock on the lock file's descriptor.
func lockUnix(f *os.File, shared bool) (bool, error) {
	how := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		how = unix.LOCK_SH | unix.LOCK_NB
	}
	err := unix.Flock(int(f.Fd()), how)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("flock: %w", err)
	}

	return true, nil
}

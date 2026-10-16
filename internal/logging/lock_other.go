//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package logging

import "os"

// lockFile is a no-op on platforms without flock/LockFileEx support; READY's
// supported targets (Linux, macOS, Windows) never use this file.
func lockFile(*os.File) error { return nil }

// unlockFile is a no-op counterpart of lockFile.
func unlockFile(*os.File) error { return nil }

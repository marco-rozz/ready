package logging

import (
	"errors"
	"io"
	"io/fs"
	"os"
)

// ErrLogDirLocked is returned at startup when another process holds the
// segment directory lock (log-file-rotation the design doc).
var ErrLogDirLocked = errors.New("log directory is locked by another process")

// logFile is the narrow file handle RotatingFile needs. It is an interface
// (not *os.File) so tests can inject short writes and ENOSPC.
type logFile interface {
	io.Writer
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// fileSystem is the filesystem surface of RotatingFile.
type fileSystem interface {
	OpenFile(name string, flag int, perm fs.FileMode) (logFile, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Remove(name string) error
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(path string, perm fs.FileMode) error
	Chmod(name string, mode fs.FileMode) error
	// Lock takes an exclusive, non-blocking OS advisory lock on name
	// (created 0600 when absent). A lock held elsewhere yields
	// ErrLogDirLocked. Closing the returned Closer releases the lock.
	Lock(name string) (io.Closer, error)
}

// osFS is the production fileSystem backed by package os.
type osFS struct{}

// Guarantees at compile time that osFS implements fileSystem.
var _ fileSystem = osFS{}

func (osFS) OpenFile(name string, flag int, perm fs.FileMode) (logFile, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

func (osFS) Remove(name string) error { return os.Remove(name) }

func (osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

func (osFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

func (osFS) Chmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }

func (osFS) Lock(name string) (io.Closer, error) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err = lockFile(f); err != nil {
		_ = f.Close()

		return nil, err
	}

	return fileLock{f: f}, nil
}

// fileLock releases the advisory lock by closing its file (the OS drops
// flock/LockFileEx locks with the last handle). The lock file itself is never
// deleted: removing it would race with a concurrent opener.
type fileLock struct {
	f *os.File
}

func (l fileLock) Close() error {
	_ = unlockFile(l.f)

	return l.f.Close()
}

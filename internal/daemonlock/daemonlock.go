// Package daemonlock implements the exclusive OS-backed locks of the
// per-user-install-agent-isolation contract: daemon ownership of a physical
// agent directory and the cross-process maintenance gate shared by serve,
// mutating login and installer purge.
//
// Both locks are stable files inside the agent directory held by an
// advisory OS lock (flock on Unix, LockFileEx on Windows). A normal release
// closes the handle and NEVER unlinks the file: its existence carries no
// ownership meaning, so a crashed process leaves nothing to clean up and no
// recorded PID is ever trusted.
package daemonlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrOccupied reports that another process already holds the requested lock.
var ErrOccupied = errors.New("agent directory is owned by another ready process")

// ErrUnsupported reports that OS-level locking is unavailable on this
// platform; ownership MUST fail closed (never "proceed without a lock").
var ErrUnsupported = errors.New("OS file locking is unsupported on this platform")

// Stable lock-file names inside the agent directory. They are never removed
// on ordinary release; only a deliberate full purge deletes them (as part of
// the directory contents).
const (
	// OwnershipFile guards exclusive daemon ownership of the directory.
	OwnershipFile = ".daemon.lock"
	// MaintenanceFile is the shared/exclusive maintenance gate: serve and
	// mutating login hold it shared; installer purge takes it exclusive for
	// the entire destructive operation.
	MaintenanceFile = ".maintenance.lock"
)

// Lock is one held OS lock. Release drops it; the file stays.
type Lock struct {
	f    *os.File
	path string
}

// Path is the lock file's path (diagnostics only — its existence proves
// nothing about ownership).
func (l *Lock) Path() string { return l.path }

// Release closes the handle, dropping the OS lock. The stable file is never
// unlinked on ordinary release.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}

	err := l.f.Close()
	l.f = nil

	return err
}

// AcquireOwnership takes the exclusive non-blocking daemon-ownership lock on
// one agent directory. A held lock fails immediately with ErrOccupied —
// before any config migration, log, sync, retention, network setup or worker
// start. Unsupported locking fails closed. The agent directory must already
// exist (selection validates it; the default directory is created by the
// config loader chain).
func AcquireOwnership(ctx context.Context, agentDir string) (*Lock, error) {
	return acquire(ctx, agentDir, OwnershipFile, false)
}

// AcquireMaintenanceShared takes the shared maintenance gate: serve and
// mutating login hold it while running; an exclusive holder (purge) blocks
// every acquisition attempt.
func AcquireMaintenanceShared(ctx context.Context, agentDir string) (*Lock, error) {
	return acquire(ctx, agentDir, MaintenanceFile, true)
}

// AcquireMaintenanceExclusive takes the exclusive maintenance gate: installer
// purge holds it for the entire destructive operation, so no daemon start or
// mutating login can interleave with deletion.
func AcquireMaintenanceExclusive(ctx context.Context, agentDir string) (*Lock, error) {
	return acquire(ctx, agentDir, MaintenanceFile, false)
}

// LockFilePath locks an arbitrary file path directly (created 0600 when
// missing, parent directories included) — the building block for the
// per-target OAuth credential coordination locks.
func LockFilePath(ctx context.Context, path string, shared bool) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	locked, err := tryLock(f, shared)
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !locked {
		_ = f.Close()

		return nil, fmt.Errorf("%w: lock %s is held", ErrOccupied, path)
	}

	return &Lock{f: f, path: path}, nil
}

func acquire(ctx context.Context, agentDir, fileName string, shared bool) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("acquire %s lock: %w", fileName, err)
	}
	path := filepath.Join(agentDir, fileName)
	// The lock file is created inside an agent directory whose existence the
	// caller already validated; creation is 0600 like every other private
	// runtime artifact.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}

	locked, err := tryLock(f, shared)
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !locked {
		_ = f.Close()

		return nil, fmt.Errorf("%w: %s (lock %s is held)", ErrOccupied, agentDir, fileName)
	}

	return &Lock{f: f, path: path}, nil
}

// WaitForLock repeatedly attempts a non-blocking LockFilePath until the lock
// is acquired, the caller's context expires, or the timeout elapses — the
// bounded-serialization pattern used by the OAuth per-target coordination
// locks (mutations are quick writes, never human-paced).
func WaitForLock(ctx context.Context, path string, shared bool, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := LockFilePath(ctx, path, shared)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrOccupied) || ctx.Err() != nil || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("acquire lock %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

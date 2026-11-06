package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SourceState captures the elected config's state at read time: the exact
// bytes, the permission mode and, when determinable, the file identity. The
// config-manager tool records one snapshot per staging and hands it back at
// save; CommitCandidate refuses to replace a file whose current identity,
// mode or bytes no longer match the snapshot.
//
// Data is the file content — private daemon memory, never to be logged.
type SourceState struct {
	// Path is the elected config path exactly as the caller knows it; the
	// parent-directory resolution aliases are handled internally.
	Path string
	// Data is the file's exact content at capture time.
	Data []byte
	// Mode is the file's permission mode at capture time.
	Mode os.FileMode

	info os.FileInfo
}

// Matches compares protected source identity, mode and bytes without exposing
// the private content or using a credential-derived digest.
func (s SourceState) Matches(other SourceState) bool {
	return s.Path == other.Path && s.Mode == other.Mode && bytes.Equal(s.Data, other.Data) &&
		(s.info == nil || other.info == nil || os.SameFile(s.info, other.info))
}

// ErrNotRegularConfig marks a source that cannot host a serialized commit:
// the elected config must be a regular unlinked file — symlink leaves and
// multiply hard-linked sources would split the commit. ReadConfigSource
// refuses them.
var ErrNotRegularConfig = errors.New("config must be a regular unlinked file")

// ErrCommitTransient marks a positively not-committed, retryable failure:
// lock acquisition, backup/temp writes, the pre-replacement recheck or source
// reads failed before the elected file was replaced. The original bytes are
// untouched; the caller may retry against the same snapshot.
var ErrCommitTransient = errors.New("config commit was not completed")

// ErrCommitPostCommit marks a durability failure after the replacement
// succeeded (committed is true): no further write may be attempted.
var ErrCommitPostCommit = errors.New("config commit completed with a durability failure")

// ReadConfigSource captures the config file's current state using the same
// protections as migration source reads: explicit non-regular leaves, a
// directory alias resolved the same way a commit would, and the link-count
// check on the open handle.
func ReadConfigSource(ctx context.Context, path string) (SourceState, error) {
	if err := ctx.Err(); err != nil {
		return SourceState{}, fmt.Errorf("read config source: %w", err)
	}

	data, info, err := readMigrationSource(ctx, path)
	if err != nil {
		return SourceState{}, err
	}

	return SourceState{Path: path, Data: data, Mode: info.Mode(), info: info}, nil
}

// CommitReport is the metadata-only outcome of a committed or refused
// candidate replacement. It deliberately carries no configuration values.
type CommitReport struct {
	// BackupPath is the protected backup of the replaced original; empty
	// when no backup was created.
	BackupPath string
	// Committed states the replacement fact independently of any later
	// error: a committed result stays committed even when the subsequent
	// directory sync or backup pruning failed.
	Committed bool
	// Warnings carries non-fatal post-commit problems (durability,
	// retention) as safe strings.
	Warnings []string
}

// CommitCandidate atomically replaces the config referenced by the captured
// state with candidate — under the same per-config advisory lock as boot
// migrations. Under the lock it re-reads the source and refuses with
// ErrConfigSourceConflict when its identity, permission mode or bytes no
// longer match the snapshot (an external edit, chmod or replacement won);
// otherwise it writes a basename-scoped protected backup of the original, a
// flushed sibling temporary holding the candidate (source permissions
// preserved), syncs the backup directory entry, replaces atomically, syncs
// the committed directory and prunes protected backups to the newest three.
//
// Report.Committed states the replacement fact independently of a later
// error: once Committed is true any error describes only post-replacement
// uncertainty, never rollback — the write must not be retried. A positively
// not-committed failure leaves the original bytes untouched and may be
// retried against the same snapshot. Lock waits and pre-replacement
// operations honor context cancellation.
func CommitCandidate(ctx context.Context, state SourceState, candidate []byte) (CommitReport, error) {
	if err := ctx.Err(); err != nil {
		return CommitReport{}, fmt.Errorf("commit config: %w", err)
	}

	return commitCandidate(ctx, state, candidate, migrationIO{})
}

// CommitCandidateEncrypted is CommitCandidate whose backup of the previous
// file is passed through seal (a Secret-store encryption) and stored as
// `<config>.bak.<ts>.<rand>.age`. Callers use it whenever the previous bytes
// may still hold a plaintext credential (`ready secrets adopt`).
func CommitCandidateEncrypted(ctx context.Context, state SourceState, candidate []byte, seal func([]byte) ([]byte, error)) (CommitReport, error) {
	if err := ctx.Err(); err != nil {
		return CommitReport{}, fmt.Errorf("commit config: %w", err)
	}

	return commitCandidate(ctx, state, candidate, migrationIO{backup: encryptedBackup(seal)})
}

// encryptedBackup adapts a sealing function to migrationIO.backup.
func encryptedBackup(seal func([]byte) ([]byte, error)) func([]byte) ([]byte, string, error) {
	return func(source []byte) ([]byte, string, error) {
		sealed, err := seal(source)
		if err != nil {
			return nil, "", fmt.Errorf("encrypt the config backup: %w", err)
		}

		return sealed, backupEncryptedSuffix, nil
	}
}

// CommitCandidateWithFS is the injection seam for tests: identical to
// CommitCandidate with an overridden file-operations set (write/replace/
// syncDir/prune). Used directly by config package tests only.
func CommitCandidateWithFS(ctx context.Context, state SourceState, candidate []byte, fs migrationIO) (CommitReport, error) {
	if err := ctx.Err(); err != nil {
		return CommitReport{}, fmt.Errorf("commit config: %w", err)
	}

	return commitCandidate(ctx, state, candidate, fs)
}

func commitCandidate(ctx context.Context, state SourceState, candidate []byte, fs migrationIO) (CommitReport, error) {
	dir, err := filepath.EvalSymlinks(filepath.Dir(state.Path))
	if err != nil {
		return CommitReport{}, fmt.Errorf("resolve config directory: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return CommitReport{}, fmt.Errorf("resolve config directory: %w", err)
	}
	targetPath := filepath.Join(dir, filepath.Base(state.Path))

	lock, err := acquireMigrationLock(ctx, targetPath+".migration.lock")
	if err != nil {
		return CommitReport{}, err
	}
	defer lock.Close()

	source, info, err := readMigrationSource(ctx, targetPath)
	if err != nil {
		return CommitReport{}, err
	}
	if !bytes.Equal(state.Data, source) || state.Mode != info.Mode() {
		return CommitReport{}, ErrConfigSourceConflict
	}
	if state.info != nil && !os.SameFile(state.info, info) {
		return CommitReport{}, ErrConfigSourceConflict
	}

	return commitConfig(ctx, targetPath, source, candidate, info, fs)
}

// commitConfig is the shared commit core used by migrations and the
// config-manager tool: backup, sync, temp, recheck, replace, sync, prune.
// source/candidate are captured prior to the call; info describes source.
// The caller must hold the per-config advisory lock.
func commitConfig(ctx context.Context, path string, source, candidate []byte, info os.FileInfo, fs migrationIO) (CommitReport, error) {
	var report CommitReport
	// Every failure before the replacement is positively not-committed and
	// retry-safe (ErrCommitTransient); failures after the replacement carry
	// ErrCommitPostCommit under a Committed report.
	transient := func(err error, what string) (CommitReport, error) {
		return report, fmt.Errorf("%s: %w: %w", what, ErrCommitTransient, err)
	}
	if err := ctx.Err(); err != nil {
		return transient(err, "commit config")
	}
	dir := filepath.Dir(path)
	write := fs.write
	if write == nil {
		write = writeMigrationFile
	}
	backupData, backupSuffix := source, ""
	if fs.backup != nil {
		var berr error
		if backupData, backupSuffix, berr = fs.backup(source); berr != nil {
			return transient(berr, "prepare config backup")
		}
	}
	backup, err := write(ctx, path, filepath.Base(path)+".bak."+time.Now().UTC().Format(backupTimeFormat)+".*"+backupSuffix, backupData, info.Mode().Perm()&0o600)
	if err != nil {
		return transient(err, "write config backup")
	}
	report.BackupPath = backup
	// Persist the backup directory entry before publishing a new config.
	if err = syncDirWith(ctx, fs, dir); err != nil {
		return transient(err, "sync backup directory")
	}
	tmp, err := write(ctx, path, "."+filepath.Base(path)+".migration-*", candidate, info.Mode().Perm()&0o600)
	if err != nil {
		return transient(err, "write config candidate")
	}
	defer func() { _ = os.Remove(tmp) }()
	if err = os.Chmod(tmp, info.Mode().Perm()); err != nil {
		return transient(err, "preserve config permissions")
	}
	current, currentInfo, err := readMigrationSource(ctx, path)
	if err != nil {
		return transient(err, "recheck config source")
	}
	if !os.SameFile(info, currentInfo) || info.Mode() != currentInfo.Mode() || !bytes.Equal(source, current) {
		return report, ErrConfigSourceConflict
	}
	if err = ctx.Err(); err != nil {
		return transient(err, "commit config")
	}
	if err = replaceWith(ctx, fs, tmp, path); err != nil {
		return transient(err, "replace config")
	}
	report.Committed = true
	if err = syncDirWith(ctx, fs, dir); err != nil {
		return report, fmt.Errorf("sync committed config directory: %w: %w", ErrCommitPostCommit, err)
	}
	if err = pruneWith(ctx, fs, path); err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}

	return report, nil
}

func syncDirWith(ctx context.Context, fs migrationIO, dir string) error {
	if fs.syncDir != nil {
		return fs.syncDir(ctx, dir)
	}

	return syncConfigDir(ctx, dir)
}

func replaceWith(ctx context.Context, fs migrationIO, tmp, path string) error {
	if fs.replace != nil {
		return fs.replace(ctx, tmp, path)
	}

	return replaceConfig(ctx, tmp, path)
}

func pruneWith(ctx context.Context, fs migrationIO, path string) error {
	if fs.prune != nil {
		return fs.prune(ctx, path)
	}

	return pruneConfigBackups(ctx, path)
}

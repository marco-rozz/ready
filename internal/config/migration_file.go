package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const backupTimeFormat = "20060102T150405.000000000Z"

func acquireMigrationLock(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config migration lock %s: %w", path, err)
	}
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var locked bool
		locked, err = tryMigrationLock(ctx, f)
		if err != nil || locked {
			break
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-timer.C:
		}
		timer.Stop()
		if err != nil {
			break
		}
	}
	if err != nil {
		_ = f.Close()

		return nil, fmt.Errorf("lock config %s: %w", path, err)
	}
	// Close releases the OS advisory lock. The stable lock file is never removed.
	return f, nil
}

func readMigrationSource(ctx context.Context, path string) ([]byte, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("read migration source: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("config %s: migration requires a regular unlinked file; copy the config and use --config", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()
	openedInfo, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat opened config: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, nil, ErrConfigSourceConflict
	}
	if err = checkConfigLinks(ctx, f, openedInfo); err != nil {
		return nil, nil, err
	}
	// Use the same open handle whose identity/link count was checked.
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(f); err != nil {
		return nil, nil, fmt.Errorf("read config %s: %w", path, err)
	}

	return buf.Bytes(), openedInfo, nil
}

func commitMigration(ctx context.Context, path string, source, candidate []byte, info os.FileInfo, report *MigrationReport, fs migrationIO) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	dir := filepath.Dir(path)
	write := fs.write
	if write == nil {
		write = writeMigrationFile
	}
	backup, err := write(ctx, path, filepath.Base(path)+".bak."+time.Now().UTC().Format(backupTimeFormat)+".*", source, info.Mode().Perm()&0o600)
	if err != nil {
		return err
	}
	report.BackupPath = backup
	// Persist the backup directory entry before publishing a new config.
	if err = fs.syncDir(ctx, dir); err != nil {
		return fmt.Errorf("sync backup directory: %w", err)
	}
	tmp, err := write(ctx, path, "."+filepath.Base(path)+".migration-*", candidate, info.Mode().Perm()&0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err = os.Chmod(tmp, info.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve config permissions: %w", err)
	}
	current, currentInfo, err := readMigrationSource(ctx, path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, currentInfo) || info.Mode() != currentInfo.Mode() || !bytes.Equal(source, current) {
		return ErrConfigSourceConflict
	}
	if err = ctx.Err(); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	if err = fs.replace(ctx, tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	report.Committed = true
	if err = fs.syncDir(ctx, dir); err != nil {
		return fmt.Errorf("sync committed config directory: %w", err)
	}
	prune := fs.prune
	if prune == nil {
		prune = pruneConfigBackups
	}
	if err = prune(ctx, path); err != nil {
		report.Warnings = append(report.Warnings, err.Error())
	}

	return nil
}

func writeMigrationFile(ctx context.Context, sourcePath, pattern string, data []byte, mode os.FileMode) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("write migration file: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(sourcePath), pattern)
	if err != nil {
		return "", fmt.Errorf("create migration file: %w", err)
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err = protectMigrationFile(ctx, f, sourcePath, mode); err != nil {
		return "", err
	}
	if _, err = f.Write(data); err != nil {
		return "", fmt.Errorf("write migration file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("flush migration file: %w", err)
	}
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("close migration file: %w", err)
	}
	ok = true

	return name, nil
}

func pruneConfigBackups(ctx context.Context, path string) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("list config backups: %w", err)
	}
	prefix := filepath.Base(path) + ".bak."
	var names []string
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("prune config backups: %w", err)
		}
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		suffix := strings.TrimPrefix(entry.Name(), prefix)
		// CreateTemp's random decimal suffix and our exact UTC timestamp define
		// the recognized scheme, not a broad basename.bak.* glob.
		if len(suffix) <= len(backupTimeFormat)+1 {
			continue
		}
		stamp, random := suffix[:len(backupTimeFormat)], suffix[len(backupTimeFormat)+1:]
		if suffix[len(backupTimeFormat)] != '.' || len(random) < 6 || strings.Trim(random, "0123456789") != "" {
			continue
		}
		if _, parseErr := time.Parse(backupTimeFormat, stamp); parseErr != nil {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	for len(names) > 3 {
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("prune config backups: %w", err)
		}
		if err = os.Remove(filepath.Join(filepath.Dir(path), names[0])); err != nil {
			return fmt.Errorf("prune config backup: %w", err)
		}
		names = names[1:]
	}

	return nil
}

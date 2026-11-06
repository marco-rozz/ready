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

// backupEncryptedSuffix marks a config backup sealed to the Store key.
const backupEncryptedSuffix = ".age"

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
		return nil, nil, fmt.Errorf("config %s: %w: migration requires a regular unlinked file; copy the config and use --config", path, ErrNotRegularConfig)
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
	committedReport, err := commitConfig(ctx, path, source, candidate, info, fs)
	report.BackupPath = committedReport.BackupPath
	report.Committed = committedReport.Committed
	report.Warnings = append(report.Warnings, committedReport.Warnings...)

	return err
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
		stamp, random := suffix[:len(backupTimeFormat)], strings.TrimSuffix(suffix[len(backupTimeFormat)+1:], backupEncryptedSuffix)
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

//go:build !windows

package initwizard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func applyDirPermissionsForSecurity(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect directory: %w", err)
	}

	return nil
}

func openPrivateFile(ctx context.Context, path string) (*os.File, error) {
	if err := applyDirPermissionsForSecurity(ctx, filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private file: %w", err)
	}
	// Set the exact mode through the descriptor even under a restrictive
	// umask, without following a replacement of the path.
	if protectErr := file.Chmod(0o600); protectErr != nil {
		info, _ := file.Stat()
		_ = file.Close()
		if current, statErr := os.Lstat(path); statErr == nil && info != nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}

		return nil, fmt.Errorf("protect private file: %w", protectErr)
	}

	return file, nil
}

func syncDirOf(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory for sync: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	return nil
}

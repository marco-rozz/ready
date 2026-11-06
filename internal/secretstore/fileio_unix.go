//go:build !windows

package secretstore

import (
	"fmt"
	"os"
)

func createPrivate(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private file: %w", err)
	}

	return f, nil
}

func replaceFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err = d.Sync(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}

	return nil
}

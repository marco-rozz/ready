//go:build !windows

package config

import (
	"context"
	"fmt"
	"os"
)

func openCredentialFile(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create private credential: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create exclusive credential file: %w", err)
	}

	return file, nil
}

func protectCredentialDirectory(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("protect credential directory: %w", err)
	}

	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set private credential directory permissions: %w", err)
	}

	return nil
}

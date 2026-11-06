package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WritePrivateCredential creates and flushes an exclusively owned file. On
// Windows the current-user-only DACL is installed at creation, before bytes.
func WritePrivateCredential(ctx context.Context, path, value string) (func(), error) {
	f, err := openCredentialFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("create private credential: %w", err)
	}
	info, statErr := f.Stat()
	cleanup := func() {
		if current, e := os.Lstat(path); e == nil && info != nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
	}
	err = statErr
	if err == nil {
		_, err = f.WriteString(value)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		cleanup()

		return nil, fmt.Errorf("write private credential: %w", err)
	}

	return cleanup, nil
}

// CheckCredentialTarget refuses links (including hard links) and special files.
// Missing leaves are permitted; their parent must be the physical agent root.
func CheckCredentialTarget(ctx context.Context, path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("check credential target: %w", err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect credential target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegularConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open credential target: %w", err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened credential target: %w", err)
	}
	if !os.SameFile(info, opened) {
		return nil, ErrConfigSourceConflict
	}
	if err = checkConfigLinks(ctx, f, opened); err != nil {
		return nil, fmt.Errorf("check credential target links: %w", err)
	}

	return info, nil
}

// ReplaceCredentialFile uses the same platform atomic replacement as config
// commits. The caller has already committed the file reference.
func ReplaceCredentialFile(ctx context.Context, from, to string) error {
	if err := replaceConfig(ctx, from, to); err != nil {
		return fmt.Errorf("replace credential file: %w", err)
	}

	return nil
}

// EnsureCredentialDirectory refuses a linked directory and applies private
// permissions to an existing or newly created agent-local secrets directory.
func EnsureCredentialDirectory(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("prepare credential directory: %w", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create credential directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect credential directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrNotRegularConfig
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve credential directory: %w", err)
	}
	if filepath.Clean(physical) != filepath.Clean(path) {
		return ErrConfigSourceConflict
	}
	if err = protectCredentialDirectory(ctx, path); err != nil {
		return fmt.Errorf("protect credential directory: %w", err)
	}

	return nil
}

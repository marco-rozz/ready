//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func tryMigrationLock(_ context.Context, f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("flock config: %w", err)
	}

	return true, nil
}

func checkConfigLinks(_ context.Context, _ *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return fmt.Errorf("config migration requires a regular file with one hard link; copy it and use --config")
	}

	return nil
}

func protectMigrationFile(_ context.Context, f *os.File, _ string, mode os.FileMode) error {
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("protect migration file: %w", err)
	}

	return nil
}

func replaceConfig(_ context.Context, from, to string) error {
	return os.Rename(from, to)
}

func syncConfigDir(_ context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config directory: %w", err)
	}
	defer f.Close()
	if err = f.Sync(); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOTSUP) {
		return fmt.Errorf("sync config directory: %w", err)
	}

	return nil
}

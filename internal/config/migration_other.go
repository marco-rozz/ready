//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package config

import (
	"context"
	"fmt"
	"os"
)

func tryMigrationLock(_ context.Context, _ *os.File) (bool, error) {
	return false, fmt.Errorf("config migration locking is unsupported on this OS")
}

func checkConfigLinks(_ context.Context, _ *os.File, _ os.FileInfo) error {
	return fmt.Errorf("config migration link checking is unsupported on this OS")
}

func protectMigrationFile(_ context.Context, _ *os.File, _ string, _ os.FileMode) error {
	return fmt.Errorf("config migration file protection is unsupported on this OS")
}

func replaceConfig(_ context.Context, _, _ string) error {
	return fmt.Errorf("config migration replacement is unsupported on this OS")
}

func syncConfigDir(_ context.Context, _ string) error {
	return fmt.Errorf("config migration directory sync is unsupported on this OS")
}

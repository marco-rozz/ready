//go:build windows

package config

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func tryMigrationLock(_ context.Context, f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock config: %w", err)
	}
	return true, nil
}

func checkConfigLinks(_ context.Context, f *os.File, _ os.FileInfo) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return fmt.Errorf("get config link count: %w", err)
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("config migration requires one hard link; copy it and use --config")
	}
	return nil
}

func protectMigrationFile(_ context.Context, f *os.File, source string, _ os.FileMode) error {
	sd, err := windows.GetNamedSecurityInfo(source, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read config access restrictions: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read config DACL: %w", err)
	}
	if err = windows.SetNamedSecurityInfo(f.Name(), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("protect migration file: %w", err)
	}
	return nil
}

func replaceConfig(_ context.Context, from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return fmt.Errorf("encode config temporary path: %w", err)
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return fmt.Errorf("encode config destination: %w", err)
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows has no supported directory-fsync equivalent; files are flushed and
// replacement uses MOVEFILE_WRITE_THROUGH.
func syncConfigDir(_ context.Context, _ string) error { return nil }

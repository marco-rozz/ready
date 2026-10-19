//go:build windows

package ioutil

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var kernel32ProcMoveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8
)

func commitAtomicTempFile(tmpName, dst, parent string, perm fs.FileMode, overwrite bool) error {
	_ = parent // windows: directory sync is skipped/no-op.

	if !overwrite {
		// Windows rename won't overwrite. This satisfies overwrite=false.
		if err := os.Rename(tmpName, dst); err != nil {
			if _, stErr := os.Lstat(dst); stErr == nil {
				return fmt.Errorf("file already exists: %w", os.ErrExist)
			}
			return err
		}
		_ = os.Chmod(dst, perm)
		return nil
	}

	// overwrite=true: replace atomically. Do not remove dst first: that creates
	// a visible gap and can lose the destination if the final rename fails.
	var renameErr error
	for attempt := range 6 {
		renameErr = moveFileReplaceExisting(tmpName, dst)

		if renameErr == nil {
			_ = os.Chmod(dst, perm)
			return nil
		}

		time.Sleep(time.Duration(15*(attempt+1)) * time.Millisecond)
	}
	return renameErr
}

func syncDirBestEffort(dir string) error {
	_ = dir
	return nil
}

func moveFileReplaceExisting(src, dst string) error {
	srcp, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}

	r1, _, e1 := kernel32ProcMoveFileExW.Call(
		uintptr(unsafe.Pointer(srcp)),
		uintptr(unsafe.Pointer(dstp)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if r1 != 0 {
		return nil
	}
	if e1 != syscall.Errno(0) {
		return e1
	}
	return syscall.EINVAL
}

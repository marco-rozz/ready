//go:build !windows

package ioutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

func commitAtomicTempFile(tmpName, dst, parent string, perm fs.FileMode, overwrite bool) error {
	if !overwrite {
		// Unix: hardlink is atomic and won't overwrite.
		if err := os.Link(tmpName, dst); err == nil {
			_ = os.Remove(tmpName)
			_ = os.Chmod(dst, perm)
			_ = syncDirBestEffort(parent)
			return nil
		} else if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("file already exists: %w", os.ErrExist)
		} else {
			// Filesystem may not support hardlinks. Preserve overwrite=false semantics
			// via O_EXCL + copy.
			out, perr := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
			if perr != nil {
				if errors.Is(perr, os.ErrExist) {
					return fmt.Errorf("file already exists: %w", os.ErrExist)
				}
				return perr
			}
			// Ensure we don't leak the fd on early error paths.
			closeOut := func() { _ = out.Close() }

			in, ierr := os.Open(tmpName)
			if ierr != nil {
				closeOut()
				_ = os.Remove(dst)
				return ierr
			}
			defer in.Close()

			if _, cerr := io.Copy(out, in); cerr != nil {
				closeOut()
				_ = os.Remove(dst)
				return cerr
			}
			if serr := out.Sync(); serr != nil {
				closeOut()
				_ = os.Remove(dst)
				return serr
			}
			if cerr := out.Close(); cerr != nil {
				_ = os.Remove(dst)
				return cerr
			}

			// Best-effort: if umask masked bits, try to apply the requested perms.
			_ = os.Chmod(dst, perm)

			_ = os.Remove(tmpName)
			_ = syncDirBestEffort(parent)
			return nil
		}
	}

	// Overwrite=true: rename replaces atomically on Unix.
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	_ = os.Chmod(dst, perm)
	_ = syncDirBestEffort(parent)
	return nil
}

func syncDirBestEffort(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

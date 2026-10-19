package ioutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// CopyFileToExistingCtx copies src -> dst where dst is expected to already exist (typically a placeholder reserved with
// O_EXCL). "dst" is truncated and overwritten.
//
// NOTE: This is a raw IO helper; callers should resolve/enforce policy before calling.
func CopyFileToExistingCtx(ctx context.Context, src, dst string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	st, err := os.Lstat(dst)
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, fmt.Errorf("destination is not a regular file: %s", dst)
	}

	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	buf := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, er := in.Read(buf)
		if nr > 0 {
			nw, ew := out.Write(buf[:nr])
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, errors.New("short write during copy")
			}
			written += int64(nw)
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				break
			}
			return written, er
		}
	}
	if err := out.Sync(); err != nil {
		return written, err
	}
	return written, nil
}

// CopyFileCtx copies src->dst, creating dst with O_EXCL.
// It checks ctx between read iterations.
//
// NOTE: This is a raw IO helper; callers should resolve/enforce policy before calling.
func CopyFileCtx(ctx context.Context, src, dst string, perm os.FileMode) (written int64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return 0, err
	}

	// If any error happens after dst is created, remove it to avoid leaving partial files behind.
	defer func() {
		cerr := out.Close()
		if err == nil && cerr != nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	buf := make([]byte, 128*1024)

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		nr, er := in.Read(buf)
		if nr > 0 {
			nw, ew := out.Write(buf[:nr])
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, errors.New("short write during copy")
			}
			written += int64(nw)
		}
		if er != nil {
			if errors.Is(er, io.EOF) {
				break
			}
			return written, er
		}
	}

	if err := out.Sync(); err != nil {
		return written, err
	}
	return written, nil
}

package initwizard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// privateFile is the exclusive file's write/durability boundary. Tests inject
// failures after creation while retaining the real filesystem identity.
type privateFile interface {
	io.WriteCloser
	Stat() (os.FileInfo, error)
	Sync() error
}

func createExclusiveFile(ctx context.Context, path string, content []byte) error {
	return writePrivateFile(ctx, path, content, func(ctx context.Context, path string) (privateFile, error) {
		return openPrivateFile(ctx, path)
	})
}

func writePrivateFile(ctx context.Context, path string, content []byte, open func(context.Context, string) (privateFile, error)) (err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	f, err := open(ctx, path)
	if err != nil {
		return fmt.Errorf("create private file: %w", err)
	}
	info, statErr := f.Stat()
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, f.Close())
		}
		if err != nil && info != nil {
			// Do not delete a replacement installed by another actor.
			if current, e := os.Lstat(path); e == nil && os.SameFile(info, current) {
				err = errors.Join(err, os.Remove(path))
			}
		}
	}()
	if statErr != nil {
		return fmt.Errorf("identify new private file: %w", statErr)
	}
	if n, err := f.Write(content); err != nil {
		return fmt.Errorf("write private file: %w", err)
	} else if n != len(content) {
		return fmt.Errorf("write private file: %w", io.ErrShortWrite)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync private file: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return fmt.Errorf("close private file: %w", err)
	}

	return nil
}

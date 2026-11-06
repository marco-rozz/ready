package initwizard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type failingPrivateFile struct {
	*os.File
	operation string
	err       error
}

var _ privateFile = (*failingPrivateFile)(nil)

func (f *failingPrivateFile) Write(data []byte) (int, error) {
	if f.operation == "write" {
		return 0, f.err
	}

	return f.File.Write(data)
}

func (f *failingPrivateFile) Sync() error {
	if f.operation == "sync" {
		return f.err
	}

	return f.File.Sync()
}

func (f *failingPrivateFile) Close() error {
	err := f.File.Close()
	if f.operation == "close" {
		return errors.Join(err, f.err)
	}

	return err
}

func TestPrivateFileFailureRemovesOwnedFile(t *testing.T) {
	for _, operation := range []string{"write", "sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private")
			failure := errors.New("injected file failure")
			err := writePrivateFile(t.Context(), path, []byte("canary"), func(ctx context.Context, path string) (privateFile, error) {
				file, err := openPrivateFile(ctx, path)
				if err != nil {
					return nil, err
				}

				return &failingPrivateFile{File: file, operation: operation, err: failure}, nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("fault did not surface: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed file remains: %v", err)
			}
		})
	}
}

func TestConfigPublicationDoesNotClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if committed, err := writeConfigExclusive(t.Context(), path, []byte("original")); err != nil || !committed {
		t.Fatal(err)
	}
	if committed, err := writeConfigExclusive(t.Context(), path, []byte("replacement")); err == nil || committed {
		t.Fatal("existing config was overwritten")
	}
	if got := readFileOrFail(t, path); got != "original" {
		t.Fatal("config changed")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("publication left temporary files: %v", err)
	}
}

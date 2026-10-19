package ioutil

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestCopyFileCtx(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	content := []byte("hello copy\n")
	mustWriteBytes(t, src, content)

	written, err := CopyFileCtx(t.Context(), src, dst, 0o640)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(len(content)) {
		t.Fatalf("written=%d want=%d", written, len(content))
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("dst content=%q want=%q", string(got), string(content))
	}

	if runtime.GOOS != toolutil.GOOSWindows {
		st, err := os.Stat(dst)
		if err != nil {
			t.Fatalf("stat dst: %v", err)
		}
		got := st.Mode().Perm()
		want := os.FileMode(0o640)
		// Umask may remove bits; it should never add bits beyond what we requested.
		if got|want != want {
			t.Fatalf("perm=%o has bits outside requested=%o", got, want)
		}
	}
}

func TestCopyFileCtx_Errors(t *testing.T) {
	dir := t.TempDir()

	srcFile := filepath.Join(dir, "src.txt")
	mustWriteBytes(t, srcFile, []byte("hello"))
	srcMissing := filepath.Join(dir, "src-missing.txt")
	srcDir := filepath.Join(dir, "srcdir")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir srcdir: %v", err)
	}
	tests := []struct {
		name           string
		ctx            context.Context
		src            string
		dstSetup       func(t *testing.T, dst string)
		dst            string
		wantErr        bool
		wantErrIs      error
		wantDstExists  *bool
		wantDstContent []byte
	}{
		{
			name:          "canceled before start",
			ctx:           canceledContext(t.Context()),
			src:           srcFile,
			dst:           filepath.Join(dir, "dst1.txt"),
			wantErr:       true,
			wantErrIs:     context.Canceled,
			wantDstExists: new(false),
		},
		{
			name: "dst exists => O_EXCL error",
			ctx:  t.Context(),
			src:  srcFile,

			dstSetup: func(t *testing.T, dst string) {
				t.Helper()
				mustWriteBytes(t, dst, []byte("already"))
			},
			dst:            filepath.Join(dir, "dst2.txt"),
			wantErr:        true,
			wantDstExists:  new(true),
			wantDstContent: []byte("already"),
		},
		{
			name:          "src missing => error and dst not created",
			ctx:           t.Context(),
			src:           srcMissing,
			dst:           filepath.Join(dir, "dst3.txt"),
			wantErr:       true,
			wantDstExists: new(false),
		},
		{
			name:          "src is directory => error and dst is cleaned up",
			ctx:           t.Context(),
			src:           srcDir,
			dst:           filepath.Join(dir, "dst4.txt"),
			wantErr:       true,
			wantDstExists: new(false),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dstSetup != nil {
				tc.dstSetup(t, tc.dst)
			}
			_, err := CopyFileCtx(tc.ctx, tc.src, tc.dst, 0o600)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantDstExists != nil {
				_, statErr := os.Lstat(tc.dst)
				gotExists := statErr == nil
				if gotExists != *tc.wantDstExists {
					t.Fatalf("dst exists=%v want=%v (statErr=%v)", gotExists, *tc.wantDstExists, statErr)
				}
			}
			if tc.wantDstContent != nil {
				b, rerr := os.ReadFile(tc.dst)
				if rerr != nil {
					t.Fatalf("read dst: %v", rerr)
				}
				if !bytes.Equal(b, tc.wantDstContent) {
					t.Fatalf("dst content=%q want=%q", string(b), string(tc.wantDstContent))
				}
			}
		})
	}
}

func TestCopyFileToExistingCtx(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")

	mustWriteBytes(t, src, []byte("NEW"))
	mustWriteBytes(t, dst, []byte("OLD-TO-BE-TRUNCATED"))

	written, err := CopyFileToExistingCtx(t.Context(), src, dst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 3 {
		t.Fatalf("written=%d want=3", written)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "NEW" {
		t.Fatalf("dst content=%q want=%q", string(got), "NEW")
	}
}

func TestCopyFileToExistingCtx_Errors(t *testing.T) {
	dir := t.TempDir()

	src := filepath.Join(dir, "src.txt")
	mustWriteBytes(t, src, []byte("hello"))

	tests := []struct {
		name            string
		ctx             context.Context
		src             string
		dst             string
		dstSetup        func(t *testing.T, dst string)
		wantErrContains string
		wantErrIs       error
		wantDstContent  []byte
	}{
		{
			name:      "canceled before start",
			ctx:       canceledContext(t.Context()),
			src:       src,
			dst:       filepath.Join(dir, "dst1.txt"),
			wantErrIs: context.Canceled,
		},
		{
			name: "dst missing => error",
			ctx:  t.Context(),
			src:  src,
			dst:  filepath.Join(dir, "missing.txt"),
		},
		{
			name: "dst is directory => not regular file",
			ctx:  t.Context(),
			src:  src,
			dst:  filepath.Join(dir, "adir"),
			dstSetup: func(t *testing.T, dst string) {
				t.Helper()
				if err := os.Mkdir(dst, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
			wantErrContains: "not a regular file",
		},
		{
			name: "src missing => error and dst not modified",
			ctx:  t.Context(),
			src:  filepath.Join(dir, "src-missing.txt"),
			dst:  filepath.Join(dir, "dst-existing.txt"),
			dstSetup: func(t *testing.T, dst string) {
				t.Helper()
				mustWriteBytes(t, dst, []byte("KEEP"))
			},
			wantDstContent: []byte("KEEP"),
		},
		{
			name: "dst is symlink => not regular file",
			ctx:  t.Context(),
			src:  src,
			dst:  filepath.Join(dir, "dstlink.txt"),
			dstSetup: func(t *testing.T, dst string) {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("symlink tests skipped on Windows")
				}
				realDst := filepath.Join(dir, "real-dst.txt")
				mustWriteBytes(t, realDst, []byte("x"))
				mustSymlinkOrSkip(t, realDst, dst)
			},
			wantErrContains: "not a regular file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dstSetup != nil {
				tc.dstSetup(t, tc.dst)
			}
			_, err := CopyFileToExistingCtx(tc.ctx, tc.src, tc.dst)

			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
			}
			if tc.wantDstContent != nil {
				b, rerr := os.ReadFile(tc.dst)
				if rerr != nil {
					t.Fatalf("read dst: %v", rerr)
				}
				if !bytes.Equal(b, tc.wantDstContent) {
					t.Fatalf("dst content=%q want=%q", string(b), string(tc.wantDstContent))
				}
			}
		})
	}
}

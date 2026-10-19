package ioutil

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

func TestWriteFileAtomicBytes_BasicAndSymlinkParent(t *testing.T) {
	dir := t.TempDir()

	dst := filepath.Join(dir, "out.txt")
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	if err := WriteFileAtomicBytesResolved(policy, dst, []byte("hello\n"), 0o640, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("content=%q want=%q", string(b), "hello\n")
	}

	// Overwrite.
	if err := WriteFileAtomicBytesResolved(policy, dst, []byte("changed"), 0o600, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, _ = os.ReadFile(dst)
	if string(b) != "changed" {
		t.Fatalf("content=%q want=%q", string(b), "changed")
	}

	if runtime.GOOS != toolutil.GOOSWindows {
		st, err := os.Stat(dst)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// Best effort: we try to set perms on tmp before rename; should usually stick on Unix.
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("perm=%o want=%o", st.Mode().Perm(), 0o600)
		}
	}

	t.Run("symlink parent rejected (if supported)", func(t *testing.T) {
		if runtime.GOOS == toolutil.GOOSWindows {
			t.Skip("symlink tests skipped on Windows")
		}
		realParent := filepath.Join(dir, "realparent")
		if err := os.Mkdir(realParent, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		linkParent := filepath.Join(dir, "linkparent")
		mustSymlinkOrSkip(t, realParent, linkParent)

		err := WriteFileAtomicBytesResolved(policy, filepath.Join(linkParent, "x.txt"), []byte("nope"), 0o600, true)
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "symlink path component") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestWriteFileAtomicBytes_OverwriteFalseAndDestinationType(t *testing.T) {
	dir := t.TempDir()

	existing := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(existing, []byte("OLD"), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	adir := filepath.Join(dir, "adir")
	if err := os.Mkdir(adir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	tests := []struct {
		name            string
		path            string
		data            []byte
		perm            os.FileMode
		overwrite       bool
		wantErrIs       error
		wantErrContains string
		wantContent     []byte
	}{
		{
			name:        "new file with overwrite=false succeeds",
			path:        filepath.Join(dir, "new.txt"),
			data:        []byte("NEW"),
			perm:        0o600,
			overwrite:   false,
			wantContent: []byte("NEW"),
		},
		{
			name:        "existing file with overwrite=false returns ErrExist and does not modify",
			path:        existing,
			data:        []byte("SHOULD-NOT-WRITE"),
			perm:        0o600,
			overwrite:   false,
			wantErrIs:   os.ErrExist,
			wantContent: []byte("OLD"),
		},
		{
			name:            "destination is directory errors",
			path:            adir,
			data:            []byte("x"),
			perm:            0o600,
			overwrite:       true,
			wantErrContains: "directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			err = WriteFileAtomicBytesResolved(policy, tc.path, tc.data, tc.perm, tc.overwrite)
			if tc.wantErrIs != nil || tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.wantContent != nil {
				b, rerr := os.ReadFile(tc.path)
				if rerr != nil {
					t.Fatalf("read: %v", rerr)
				}
				if !bytes.Equal(b, tc.wantContent) {
					t.Fatalf("content=%q want=%q", string(b), string(tc.wantContent))
				}
			}
		})
	}
}

func TestWriteFileAtomicBytes_RefusesSymlinkDestination_EvenIfSymlinksAllowed(t *testing.T) {
	t.Parallel()

	// On Windows, symlink creation commonly requires special privileges; skip for stability.
	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("symlink test skipped on Windows")
	}

	dir := t.TempDir()

	realTxt := filepath.Join(dir, "real.txt")
	mustWriteBytes(t, realTxt, []byte("REAL"))

	link := filepath.Join(dir, "link.txt")
	mustSymlinkOrSkip(t, realTxt, link)

	p, err := fspolicy.New(dir, nil, false) // symlinks allowed by policy
	if err != nil {
		t.Fatalf("New policy: %v", err)
	}

	err = WriteFileAtomicBytesResolved(p, link, []byte("DATA"), 0o600, true)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "symlink destination") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Ensure the target wasn't modified.
	b, rerr := os.ReadFile(realTxt)
	if rerr != nil {
		t.Fatalf("read real: %v", rerr)
	}
	if string(b) != "REAL" {
		t.Fatalf("real content=%q want %q", string(b), "REAL")
	}
}

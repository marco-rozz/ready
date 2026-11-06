//go:build windows

package initwizard

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPrivateWindowsFilesAndDirectoriesHaveProtectedDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	if err := ensureAgentDir(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "private")
	file, err := openPrivateFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	expected, err := privateSecurityDescriptor(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Verify protection while the new file is still empty, before any secret.
	for _, path := range []string{dir, path} {
		actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || actual.String() != expected.String() {
			t.Fatalf("private DACL differs for %s: %v", path, err)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("file must still be empty during protection check")
	}
}

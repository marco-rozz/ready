//go:build windows

package initwizard

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

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
	expectedDACL, _, err := expected.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var expectedACE *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(expectedDACL, 0, &expectedACE); err != nil {
		t.Fatal(err)
	}
	// Verify protection while the new file is still empty, before any secret.
	// Windows may add auto-inheritance bookkeeping to the descriptor. Check
	// protection and the exact access entry rather than comparing SDDL strings.
	for _, path := range []string{dir, path} {
		actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := actual.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatalf("DACL is not protected for %s: %s (%v)", path, actual.String(), err)
		}
		dacl, defaulted, err := actual.DACL()
		if err != nil || dacl == nil || defaulted || dacl.AceCount != 1 {
			t.Fatalf("expected one explicit private ACE for %s: %s (%v)", path, actual.String(), err)
		}
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, 0, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		expectedSID := (*windows.SID)(unsafe.Pointer(&expectedACE.SidStart))
		if ace.Header != expectedACE.Header || ace.Mask != expectedACE.Mask || !sid.Equals(expectedSID) {
			t.Fatalf("private ACE differs for %s: %s", path, actual.String())
		}
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("file must still be empty during protection check")
	}
}

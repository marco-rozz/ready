//go:build windows

package config

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMigrationPreservesWindowsDACL(t *testing.T) {
	path := migrationSource(t, migrationFixture)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	_, report, err := migrationRegistry(t, testMigration{version: 2}).Prepare(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, report.BackupPath} {
		after, getErr := windows.GetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if after.String() != before.String() {
			t.Fatalf("DACL changed for %s: %s -> %s", name, before.String(), after.String())
		}
	}
}

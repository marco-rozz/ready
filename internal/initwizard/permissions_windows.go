//go:build windows

package initwizard

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateSecurityDescriptor excludes inherited ACEs and grants only the
// current user's SID access. The same descriptor protects files at creation.
func privateSecurityDescriptor(ctx context.Context) (*windows.SECURITY_DESCRIPTOR, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("identify current user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("build protected DACL: %w", err)
	}

	return sd, nil
}

func applyDirPermissionsForSecurity(ctx context.Context, dir string) error {
	sd, err := privateSecurityDescriptor(ctx)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read private DACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("protect directory: %w", err)
	}

	return nil
}

func openPrivateFile(ctx context.Context, path string) (*os.File, error) {
	sd, err := privateSecurityDescriptor(ctx)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode private file path: %w", err)
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("create private file: %w", err)
	}

	return os.NewFile(uintptr(handle), path), nil
}

// Windows does not support directory fsync. File contents are flushed.
func syncDirOf(ctx context.Context, _ string) error { return ctx.Err() }

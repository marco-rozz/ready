//go:build windows

package config

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func credentialDescriptor(ctx context.Context) (*windows.SECURITY_DESCRIPTOR, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("prepare Windows credential security: %w", err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("prepare Windows credential security: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("create private credential security descriptor: %w", err)
	}
	return sd, nil
}

func openCredentialFile(ctx context.Context, path string) (*os.File, error) {
	sd, err := credentialDescriptor(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare Windows credential security: %w", err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("prepare Windows credential security: %w", err)
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("prepare Windows credential security: %w", err)
	}
	return os.NewFile(uintptr(h), path), nil
}

func protectCredentialDirectory(ctx context.Context, path string) error {
	sd, err := credentialDescriptor(ctx)
	if err != nil {
		return fmt.Errorf("protect Windows credential directory: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("protect Windows credential directory: %w", err)
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("apply private credential directory DACL: %w", err)
	}
	return nil
}

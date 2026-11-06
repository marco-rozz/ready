//go:build windows

package secretstore

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createPrivate creates a new file whose DACL admits only the current user,
// installed at creation so no byte is ever written under a wider ACL.
func createPrivate(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("build security descriptor: %w", err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode path: %w", err)
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ, &attrs, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("create private file: %w", err)
	}

	return os.NewFile(uintptr(h), path), nil
}

// replaceFile retries briefly: a reader that holds the destination open for
// read-and-close makes MoveFileEx fail with a sharing violation.
func replaceFile(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return fmt.Errorf("encode path: %w", err)
	}
	for attempt := 0; ; attempt++ {
		err = windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil {
			return nil
		}
		busy := errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
		if !busy || attempt >= 20 {
			return fmt.Errorf("MoveFileEx: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Windows has no directory fsync; MOVEFILE_WRITE_THROUGH covers the replace.
func syncDir(string) error { return nil }

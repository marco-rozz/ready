//go:build windows

package secretstore

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// vaultSupported reports whether the OS vault backend exists on this platform.
const vaultSupported = true

const (
	credTypeGeneric        = 1
	credPersistLocalMachin = 2
)

var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW  = advapi32.NewProc("CredReadW")
	procCredWriteW = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

// credentialW mirrors the Win32 CREDENTIALW structure.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func vaultTarget(account string) (*uint16, error) {
	p, err := windows.UTF16PtrFromString(vaultService + "/" + account)
	if err != nil {
		return nil, fmt.Errorf("encode credential target: %w", err)
	}

	return p, nil
}

func vaultGet(_ context.Context, account string) (string, error) {
	target, err := vaultTarget(account)
	if err != nil {
		return "", err
	}
	var cred *credentialW
	r, _, callErr := procCredReadW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&cred)))
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return "", ErrKeyNotFound
		}

		return "", fmt.Errorf("credential manager read failed: %w", callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred))) //nolint:errcheck // CredFree has no failure mode to act on

	blob := unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)

	return string(blob), nil
}

func vaultSet(_ context.Context, account, key string) error {
	target, err := vaultTarget(account)
	if err != nil {
		return err
	}
	blob := []byte(key)
	cred := credentialW{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocalMachin,
	}
	r, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return fmt.Errorf("credential manager write failed: %w", callErr)
	}

	return nil
}

func vaultDelete(_ context.Context, account string) error {
	target, err := vaultTarget(account)
	if err != nil {
		return err
	}
	r, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil
		}

		return fmt.Errorf("credential manager delete failed: %w", callErr)
	}

	return nil
}

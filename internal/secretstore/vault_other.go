//go:build !darwin && !windows

package secretstore

import "context"

// vaultSupported is false off macOS and Windows: the os_vault location is
// refused at validation and `file` is named as the alternative.
const vaultSupported = false

func vaultGet(context.Context, string) (string, error) { return "", ErrVaultUnsupported }

func vaultSet(context.Context, string, string) error { return ErrVaultUnsupported }

func vaultDelete(context.Context, string) error { return ErrVaultUnsupported }

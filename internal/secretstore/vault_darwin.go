//go:build darwin

package secretstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	securityBin = "/usr/bin/security"
	// exitItemNotFound is `security`'s exit status for a missing item.
	exitItemNotFound = 44
)

// vaultSupported reports whether the OS vault backend exists on this platform.
const vaultSupported = true

// vaultGet reads the Store key from the login Keychain. The key is only ever
// on the child's stdout, never in argv.
func vaultGet(ctx context.Context, account string) (string, error) {
	cmd := exec.CommandContext(ctx, securityBin, "find-generic-password", "-s", vaultService, "-a", account, "-w")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == exitItemNotFound {
			return "", ErrKeyNotFound
		}

		return "", fmt.Errorf("keychain read failed: %w", err)
	}

	return strings.TrimSpace(out.String()), nil
}

// vaultSet writes the Store key through `security -i`, so the secret travels
// on stdin and never appears in the process list.
func vaultSet(ctx context.Context, account, key string) error {
	if !safeToken(account) || !safeToken(key) {
		return errors.New("keychain write refused: unexpected characters")
	}
	cmd := exec.CommandContext(ctx, securityBin, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", vaultService, account, key))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain write failed: %w", err)
	}

	return nil
}

func vaultDelete(ctx context.Context, account string) error {
	cmd := exec.CommandContext(ctx, securityBin, "delete-generic-password", "-s", vaultService, "-a", account)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == exitItemNotFound {
			return nil
		}

		return fmt.Errorf("keychain delete failed: %w", err)
	}

	return nil
}

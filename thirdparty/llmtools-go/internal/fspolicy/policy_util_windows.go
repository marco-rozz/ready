//go:build windows

package fspolicy

import (
	"errors"
	"path/filepath"
	"strings"
)

var errWindowsDriveRelativePath = errors.New(
	"windows drive-relative paths like `C:foo` are not supported; use `C:\\foo` or a relative path without a drive letter",
)

func rejectDriveRelativePath(p string) error {
	vol := filepath.VolumeName(p)
	if vol != "" && !filepath.IsAbs(p) {
		return errWindowsDriveRelativePath
	}
	return nil
}

func applySystemRootAliases(p string) string {
	if strings.TrimSpace(p) == "" {
		return p
	}
	return filepath.Clean(p)
}

func allowSystemSymlink(cur string) (resolved string, ok bool, err error) {
	_ = cur
	return "", false, nil
}

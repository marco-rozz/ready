//go:build windows

package secretstore

import "os"

// Windows ACLs are not expressed in the mode bits; files are created with a
// user-only DACL, so no warning is derived from them.
func modeIsGroupOrOtherAccessible(os.FileMode) bool { return false }

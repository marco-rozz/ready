//go:build !windows

package secretstore

import "os"

func modeIsGroupOrOtherAccessible(m os.FileMode) bool { return m.Perm()&0o077 != 0 }

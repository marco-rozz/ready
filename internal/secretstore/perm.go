package secretstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// PermissionWarnings reports store, key and previous-generation files (and any
// extra `!secret_file` targets) that other users could read. Windows files are
// created with a user-only DACL, so nothing is reported there. Missing files
// are skipped: this inspects, it never fixes.
func PermissionWarnings(dir string, extra ...string) []string {
	paths := make([]string, 0, 3+len(extra))
	paths = append(paths,
		filepath.Join(dir, StoreFileName),
		filepath.Join(dir, StoreFileName+prevSuffix),
		filepath.Join(dir, KeyFileName),
	)
	paths = append(paths, extra...)
	var out []string
	for _, p := range paths {
		if w := permissionWarning(p); w != "" {
			out = append(out, w)
		}
	}

	return out
}

func permissionWarning(path string) string {
	fi, err := os.Stat(path)
	if err != nil || !modeIsGroupOrOtherAccessible(fi.Mode()) {
		return ""
	}

	return fmt.Sprintf("%s is readable by other users (mode %04o); run chmod 600", path, fi.Mode().Perm())
}

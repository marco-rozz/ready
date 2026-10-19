package fspolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errPathMustBeAbsolute = errors.New("path must be absolute")

func ensureDirExists(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return errors.Join(err, errors.New("no such dir"))
	}
	if !st.IsDir() {
		return fmt.Errorf("path is not a directory: %s", p)
	}
	return nil
}

func ensureWithinRoots(p string, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	for _, r := range roots {
		ok, err := isPathWithinRoot(r, p)
		if err != nil {
			continue
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrOutsideAllowedRoots, p)
}

func isPathWithinRoot(root, p string) (bool, error) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false, err
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return true, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false, nil
	}
	return true, nil
}

func lexicalAbsCleanPath(p string) (string, error) {
	norm, err := normalizePath(p)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(norm)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	abs = applySystemRootAliases(abs)
	return abs, nil
}

// normalizePath:
// - trims
// - rejects empty and NUL byte
// - filepath.Clean.
func normalizePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", ErrInvalidPath
	}
	if strings.ContainsRune(p, 0) {
		return "", ErrInvalidPath
	}
	p = filepath.FromSlash(p)
	return filepath.Clean(p), nil
}

// evalSymlinksBestEffort tries filepath.EvalSymlinks on p. If p doesn't exist,
// it walks up to the nearest existing parent, resolves that, then joins the
// remainder back on.
func evalSymlinksBestEffort(p string) string {
	p = filepath.Clean(p)
	tried := p
	remainder := ""

	for range 64 {
		if resolved, err := filepath.EvalSymlinks(tried); err == nil && resolved != "" {
			resolved = filepath.Clean(resolved)
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}

		parent := filepath.Dir(tried)
		if parent == tried {
			return p
		}

		base := filepath.Base(tried)
		if remainder == "" {
			remainder = base
		} else {
			remainder = filepath.Join(base, remainder)
		}
		tried = parent
	}
	return p
}

func dedupeSorted(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := in[:1]
	for i := 1; i < len(in); i++ {
		if in[i] != in[i-1] {
			out = append(out, in[i])
		}
	}
	return out
}

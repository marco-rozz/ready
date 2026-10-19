package fspolicy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrInvalidPath is returned for empty/whitespace paths or paths containing NUL bytes.
	ErrInvalidPath = errors.New("invalid path")

	// ErrOutsideAllowedRoots indicates a path (after best-effort canonicalization)
	// is not inside any configured allowed root.
	ErrOutsideAllowedRoots = errors.New("path is outside allowed roots")

	// ErrSymlinkDisallowed indicates the policy forbids symlink traversal / operation.
	ErrSymlinkDisallowed = errors.New("symlinks are disallowed by policy")
)

// FSPolicy centralizes filesystem path resolution and hardening.
//
// Key rules:
//   - If allowedRoots is empty => allow all paths.
//   - Relative paths resolve against workBaseDir.
//   - Allowed-root checks are performed against a best-effort symlink-resolved path,
//     but ResolvePath returns a lexical absolute path so Lstat-based checks can still
//     detect symlink inputs.
//   - If blockSymlinks is true, directory traversal refuses symlink components and
//     file operations can refuse symlink files (depending on caller and method).
type FSPolicy struct {
	allowedRoots  []string
	workBaseDir   string
	blockSymlinks bool
}

// New initializes a hardened filesystem policy.
// It canonicalizes allowed roots and work base dir and validates that base dir exists.
// If workBaseDir is empty:
//   - if allowedRoots is set => defaults to allowedRoots[0]
//   - else => defaults to process CWD
func New(workBaseDir string, allowedRoots []string, blockSymlinks bool) (FSPolicy, error) {
	// Defense-in-depth: if symlinks are blocked, require that configured roots/base
	// contain no symlink components (and allow only explicit system symlinks via allowSystemSymlink).
	tmpPolicy := FSPolicy{
		allowedRoots:  allowedRoots,
		workBaseDir:   workBaseDir,
		blockSymlinks: blockSymlinks,
	}
	if blockSymlinks {
		if strings.TrimSpace(workBaseDir) != "" {
			baseLex, err := lexicalAbsCleanPath(workBaseDir)
			if err != nil {
				return FSPolicy{}, fmt.Errorf(
					"work base dir %q violates symlink policy: %w",
					workBaseDir,
					err,
				)
			}
			if err := tmpPolicy.verifyDirNoSymlinkAbs(baseLex); err != nil {
				return FSPolicy{}, fmt.Errorf(
					"work base dir %q violates symlink policy: %w",
					workBaseDir,
					err,
				)
			}
		}

		for _, r := range tmpPolicy.allowedRoots {
			if strings.TrimSpace(r) == "" {
				continue
			}
			rootLex, err := lexicalAbsCleanPath(r)
			if err != nil {
				return FSPolicy{}, fmt.Errorf("allowed root %q violates symlink policy: %w", r, err)
			}
			if err := tmpPolicy.verifyDirNoSymlinkAbs(rootLex); err != nil {
				return FSPolicy{}, fmt.Errorf("allowed root %q violates symlink policy: %w", r, err)
			}
		}
	}
	roots, err := canonicalizeAllowedRoots(allowedRoots)
	if err != nil {
		return FSPolicy{}, err
	}

	base := strings.TrimSpace(workBaseDir)
	if base == "" {
		if len(roots) > 0 {
			base = roots[0]
		} else {
			cwd, e := os.Getwd()
			if e != nil {
				return FSPolicy{}, e
			}
			base = cwd
		}
	}

	baseCanon, err := canonicalizeExistingDir(base)
	if err != nil {
		return FSPolicy{}, fmt.Errorf("invalid work base dir %q: %w", workBaseDir, err)
	}

	if err := ensureWithinRoots(baseCanon, roots); err != nil {
		return FSPolicy{}, fmt.Errorf("work base dir %q: %w", baseCanon, err)
	}

	p := FSPolicy{
		allowedRoots:  roots,
		workBaseDir:   baseCanon,
		blockSymlinks: blockSymlinks,
	}

	return p, nil
}

func (p FSPolicy) WorkBaseDir() string { return p.workBaseDir }
func (p FSPolicy) BlockSymlinks() bool { return p.blockSymlinks }
func (p FSPolicy) HasAllowedRoots() bool {
	return len(p.allowedRoots) > 0
}

// AllowedRoots returns a copy of the canonical allowed roots slice.
func (p FSPolicy) AllowedRoots() []string {
	if len(p.allowedRoots) == 0 {
		return nil
	}
	out := make([]string, len(p.allowedRoots))
	copy(out, p.allowedRoots)
	return out
}

// VerifyDirResolved verifies an already-resolved absolute directory path.
// It does NOT call ResolvePath again; callers should pass a value returned from ResolvePath
// (or otherwise already policy-checked).
//
// If BlockSymlinks is true, it refuses any symlink components in the path.
func (p FSPolicy) VerifyDirResolved(absDir string) error {
	d, err := normalizePath(absDir)
	if err != nil {
		return err
	}
	if d == "." {
		return nil
	}
	if !filepath.IsAbs(d) {
		return errPathMustBeAbsolute
	}
	d = filepath.Clean(d)
	d = applySystemRootAliases(d)

	if !p.blockSymlinks {
		st, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("stat dir error: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("not a directory: %s", d)
		}
		return nil
	}
	return p.verifyDirNoSymlinkAbs(d)
}

// EnsureDirResolved ensures an already-resolved absolute directory exists.
// It does NOT call ResolvePath again; callers should pass a value returned from ResolvePath
// (or otherwise already policy-checked).
//
// If BlockSymlinks is true, it creates missing components one-at-a-time, refusing symlink traversal.
// MaxNewDirs: 0 => unlimited.
func (p FSPolicy) EnsureDirResolved(absDir string, maxNewDirs int) (created int, err error) {
	d, err := normalizePath(absDir)
	if err != nil {
		return 0, err
	}
	if d == "." {
		return 0, nil
	}
	if !filepath.IsAbs(d) {
		return 0, errPathMustBeAbsolute
	}
	d = filepath.Clean(d)
	d = applySystemRootAliases(d)

	if !p.blockSymlinks {
		// If symlinks are allowed, we intentionally do not try to count created dirs.
		// (Counting accurately would require additional TOCTOU-prone stat logic.)
		return 0, os.MkdirAll(d, 0o755)
	}
	return p.walkDirNoSymlinkAbs(d, true, maxNewDirs)
}

// RequireExistingRegularFileResolved requires an already-resolved absolute path exists and is a regular file.
// It does NOT call ResolvePath again; callers should pass a value returned from ResolvePath
// (or otherwise already policy-checked).
//
// If BlockSymlinks is true, it refuses symlink parent components and a symlink final file.
func (p FSPolicy) RequireExistingRegularFileResolved(absPath string) (fs.FileInfo, error) {
	ap, err := normalizePath(absPath)
	if err != nil {
		return nil, err
	}
	if ap == "." {
		return nil, ErrInvalidPath
	}
	if !filepath.IsAbs(ap) {
		return nil, errPathMustBeAbsolute
	}
	ap = filepath.Clean(ap)
	ap = applySystemRootAliases(ap)
	return p.requireExistingRegularFileAbs(ap)
}

// ResolvePath resolves inputPath (absolute or relative) into an absolute lexical path.
// DefaultIfEmpty is used if inputPath is blank.
func (p FSPolicy) ResolvePath(inputPath, defaultIfEmpty string) (string, error) {
	absLex, _, err := p.resolvePathWithCheck(inputPath, defaultIfEmpty)
	return absLex, err
}

func (p FSPolicy) resolvePathWithCheck(inputPath, defaultIfEmpty string) (absLex, absCheck string, err error) {
	s := strings.TrimSpace(inputPath)
	if s == "" {
		s = strings.TrimSpace(defaultIfEmpty)
	}

	norm, err := normalizePath(s)
	if err != nil {
		return "", "", err
	}

	// Platform hardening (Windows drive-relative rejection, etc).
	if err := rejectDriveRelativePath(norm); err != nil {
		return "", "", err
	}

	// Resolve relative against base.
	if !filepath.IsAbs(norm) {
		base := strings.TrimSpace(p.workBaseDir)
		if base == "" {
			cwd, e := os.Getwd()
			if e != nil {
				return "", "", e
			}
			base = cwd
		}
		norm = filepath.Join(base, norm)
	}

	absLex, err = filepath.Abs(norm)
	if err != nil {
		return "", "", fmt.Errorf("could not resolve path: %w", err)
	}
	absLex = filepath.Clean(absLex)

	// Keep system paths coherent with canonicalization.
	absLex = applySystemRootAliases(absLex)

	// Canonicalize for allowed-root comparisons (symlinks/junctions/8.3 names).
	absCheck = evalSymlinksBestEffort(absLex)

	if err := ensureWithinRoots(absCheck, p.allowedRoots); err != nil {
		return "", "", fmt.Errorf("path %q (resolved to %q): %w", absLex, absCheck, err)
	}

	return absLex, absCheck, nil
}

func (p FSPolicy) requireExistingRegularFileAbs(absPath string) (fs.FileInfo, error) {
	if strings.TrimSpace(absPath) == "" {
		return nil, ErrInvalidPath
	}

	if p.blockSymlinks {
		parent := filepath.Dir(absPath)
		if parent != "" && parent != "." {
			if err := p.verifyDirNoSymlinkAbs(parent); err != nil {
				return nil, err
			}
		}

		st, err := os.Lstat(absPath)
		if err != nil {
			return nil, fmt.Errorf("got stat file error: %w", err)
		}
		if (st.Mode() & os.ModeSymlink) != 0 {
			return nil, fmt.Errorf("%w: refusing to operate on symlink file: %s", ErrSymlinkDisallowed, absPath)
		}
		if st.IsDir() {
			return nil, fmt.Errorf("expected file but got directory: %s", absPath)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("expected regular file: %s", absPath)
		}
		return st, nil
	}

	// Symlinks allowed: Stat follows symlinks.
	st, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat file error: %w", err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("expected file but got directory: %s", absPath)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("expected regular file: %s", absPath)
	}
	return st, nil
}

func (p FSPolicy) verifyDirNoSymlinkAbs(dir string) error {
	_, err := p.walkDirNoSymlinkAbs(dir, false, 0)
	return err
}

func (p FSPolicy) walkDirNoSymlinkAbs(dir string, createMissing bool, maxNewDirs int) (created int, err error) {
	d, err := normalizePath(dir)
	if err != nil {
		return 0, err
	}
	if d == "." {
		return 0, nil
	}
	if !filepath.IsAbs(d) {
		return 0, errPathMustBeAbsolute
	}

	vol := filepath.VolumeName(d)
	rest := d[len(vol):]

	sep := string(os.PathSeparator)
	rest = strings.TrimLeft(rest, sep)

	parts := []string{}
	if rest != "" {
		for part := range strings.SplitSeq(rest, sep) {
			if part == "" || part == "." {
				continue
			}
			parts = append(parts, part)
		}
	}

	var cur string
	if vol != "" {
		cur = vol + sep
	} else {
		cur = sep
	}

	created = 0
	for _, part := range parts {
		cur = filepath.Join(cur, part)

		st, err := os.Lstat(cur)
		if err == nil {
			if (st.Mode() & os.ModeSymlink) != 0 {
				// Allow explicit system symlinks via platform helper.
				if resolved, ok, aerr := allowSystemSymlink(cur); aerr != nil {
					return created, aerr
				} else if ok {
					cur = resolved
					continue
				}
				return created, fmt.Errorf(
					"%w: refusing to traverse symlink path component: %s",
					ErrSymlinkDisallowed,
					cur,
				)
			}
			if !st.IsDir() {
				return created, fmt.Errorf("path component is not a directory: %s", cur)
			}
			continue
		}

		if !errors.Is(err, os.ErrNotExist) {
			return created, fmt.Errorf("stat error: %w", err)
		}
		if !createMissing {
			return created, fmt.Errorf("stat error: %w", err)
		}

		if maxNewDirs > 0 && created >= maxNewDirs {
			return created, fmt.Errorf("too many parent directories to create (max %d)", maxNewDirs)
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return created, fmt.Errorf("could not make dir:%w", err)
		}
		created++
	}

	// Verify final is a directory if it already existed and we weren't creating.
	if !createMissing {
		st, err := os.Stat(d)
		if err != nil {
			return created, fmt.Errorf("stat error: %w", err)
		}
		if !st.IsDir() {
			return created, fmt.Errorf("not a directory: %s", d)
		}
	}

	return created, nil
}

func canonicalizeAllowedRoots(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		cr, err := canonicalizeExistingDir(r)
		if err != nil {
			return nil, fmt.Errorf("invalid allowed root %q: %w", r, err)
		}
		out = append(out, cr)
	}

	sort.Strings(out)
	out = dedupeSorted(out)
	return out, nil
}

func canonicalizeExistingDir(p string) (string, error) {
	abs, err := canonicalizeDir(p)
	if err != nil {
		return "", err
	}
	if err := ensureDirExists(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func canonicalizeDir(p string) (string, error) {
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("path contains NUL byte")
	}

	cleaned := filepath.Clean(filepath.FromSlash(strings.TrimSpace(p)))
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", err
	}

	abs = applySystemRootAliases(abs)
	abs = evalSymlinksBestEffort(abs)
	return abs, nil
}

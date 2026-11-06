// Package confinement enforces the agent-directory state boundary: every
// writable runtime destination (sessions, logs, skills, generated, backup and
// temporary files) must remain a descendant of the boundary root after every
// path component — including symlinks and Windows junctions already present
// at check time — has been resolved to its physical location.
package confinement

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// caseInsensitive reports whether the running filesystem folds path casing
// (Windows and macOS), in which case containment compares identities
// case-insensitively.
func caseInsensitive() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	default:
		return false
	}
}

// Root resolves the physical identity of a boundary directory: symlinks and
// junctions in every component are resolved; a missing leaf (a fresh
// directory that will be created later) resolves through its parent so
// eventual creation lands at the same identity.
func Root(dir string) (string, error) {
	dir = filepath.Clean(dir)
	resolved, err := filepath.EvalSymlinks(dir)
	if err == nil {
		return filepath.Clean(resolved), nil
	}

	if os.IsNotExist(err) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("cannot resolve the boundary root %s: %w", dir, err)
		}
		physicalParent, perr := Root(parent)
		if perr != nil {
			return "", perr
		}

		return filepath.Join(physicalParent, filepath.Base(dir)), nil
	}

	return "", fmt.Errorf("cannot resolve the boundary root %s: %w", dir, err)
}

// Check verifies that target stays inside the boundary directory logical
// root, resolving every link component that already exists. A configured
// destination escaping by absolute path, ".." traversal, symlink or
// junction is rejected before any write happens.
//
// The walk is anchored at the root: relative components are checked
// lexically (no traversal), then each component that exists is resolved to
// its physical location and the resolved walk must never leave the physical
// root. Non-existing components cannot resolve out and accumulate below the
// last physical location.
func Check(rootLogical, target string) error {
	if rootLogical == "" {
		return fmt.Errorf("confinement check without a boundary root")
	}
	rootLogical = absClean(rootLogical)
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("cannot resolve destination %s: %w", target, err)
	}

	// Lexical containment: the destination must select a descendant of the
	// root. This catches absolute destinations elsewhere, parent traversal
	// and mismatched volumes.
	rel, err := relUnderRoot(rootLogical, abs)
	if err != nil {
		return fmt.Errorf("destination %s is outside the agent directory %s", target, rootLogical)
	}

	rootPhysical, err := Root(rootLogical)
	if err != nil {
		return fmt.Errorf("cannot establish the agent-directory boundary: %w", err)
	}

	cursor := rootPhysical
	notYet := make([]string, 0, len(rel))
	for _, component := range rel {
		next := filepath.Join(cursor, component)
		info, lerr := os.Lstat(next)
		if os.IsNotExist(lerr) {
			// From here on nothing exists: future components accumulate
			// below the last physical location.
			notYet = append(notYet, component)

			continue
		}
		if lerr != nil {
			return fmt.Errorf("cannot inspect %s: %w", next, lerr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, serr := filepath.EvalSymlinks(next)
			if err != nil {
				return fmt.Errorf("cannot resolve the link at %s: %w", next, serr)
			}
			cursor = filepath.Clean(resolved)
		} else {
			cursor = next
		}
		if !within(rootPhysical, cursor) {
			return fmt.Errorf("destination %s resolves to %s, outside the agent directory %s", target, cursor, rootPhysical)
		}
		// A symlink can only be followed while it exists; the accumulated
		// future components must reset their base to the current cursor.
		for _, pending := range notYet {
			cursor = filepath.Join(cursor, pending)
		}
		notYet = notYet[:0]
	}

	_ = notYet

	return nil
}

// relUnderRoot returns target's path below root, or an error when it is not
// a descendant. Case is folded on case-insensitive filesystems so a
// case-literal difference does not produce a bogus upward relation.
func relUnderRoot(root, target string) ([]string, error) {
	if caseInsensitive() {
		root, target = strings.ToLower(root), strings.ToLower(target)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("not a descendant")
	}

	return splitPath(rel), nil
}

func splitPath(rel string) []string {
	var out []string
	for rel != "." && rel != "" {
		dir, base := filepath.Split(rel)
		base = strings.TrimSuffix(base, string(filepath.Separator))
		if base != "" && base != "." && base != ".." {
			out = append(out, base)
		}
		if dir == "" {
			break
		}
		rel = filepath.Clean(dir)
	}

	// Components were collected right-to-left.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}

	return out
}

func absClean(p string) string {
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return filepath.Clean(p)
	}

	return abs
}

// within reports whether path equals root or is a descendant of it. On
// case-folding filesystems both paths are folded to one casing first, so a
// case-literal difference ("C:\Users" vs "c:\users") does not make Rel
// report a bogus upward relation.
func within(root, path string) bool {
	if path == root {
		return true
	}
	if caseInsensitive() {
		root, path = strings.ToLower(root), strings.ToLower(path)
		if path == root {
			return true
		}
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

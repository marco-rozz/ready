package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/marco-rozz/ready/internal/confinement"
)

// knowledgeDirName is the reserved agent-directory child holding OKF bundles
// (kept equal to okf.KnowledgeDirName; config must not import the tool
// package).
const knowledgeDirName = "okfs"

const goosWindows = "windows"

// validateStateConfined rejects configured writable destinations that
// resolve outside the agent directory — by absolute path, parent traversal,
// symlink or junction. Sessions, the runtime skill copy and the enabled file
// log are the destinations an operator may move; they must remain
// descendants. Empty paths (defaults) are derived inside the runtime home
// and never escape.
func (c *Config) validateStateConfined() error {
	if c.RuntimeHome == "" {
		// Direct construction without a runtime home: no boundary to check
		// against (test seam and embedders).
		return nil
	}

	// Check resolves the physical boundary itself; the logical runtime home
	// is the root spelling destinations are compared against.
	root := c.RuntimeHome

	type confinedDest struct {
		key  string
		path string
	}
	for _, d := range []confinedDest{
		{key: "sessions.path", path: c.Sessions.Path},
		{key: "skills.path", path: c.Skills.Path},
		{key: "subagents.path", path: c.Subagents.Path},
		{key: "logging.file_path", path: c.Logging.FilePath},
	} {
		if d.path == "" {
			continue
		}
		if err := confinement.Check(root, d.path); err != nil {
			return fmt.Errorf("%s: %w", d.key, err) // user-facing config error
		}
	}

	return nil
}

// validateKnowledgeRootDisjoint keeps the reserved knowledge root
// <agent-dir>/okfs structurally apart from every configurable runtime path:
// skills.path, sessions.path and subagents.path must be neither equal to,
// inside, nor ancestors of it, and logging.file_path's directory must not be
// it or inside it (an ancestor is fine: log retention only deletes
// segment-named files in its own directory). Without this, a path such as
// "skills.path: ." would put bundles in reach of the skill manager, the
// bootstrap walk, library sync or Session retention.
func (c *Config) validateKnowledgeRootDisjoint() error {
	if c.RuntimeHome == "" {
		return nil
	}
	knowledge := filepath.Join(c.RuntimeHome, knowledgeDirName)

	for _, d := range []struct{ key, path string }{
		{"sessions.path", c.Sessions.Path},
		{"skills.path", c.Skills.Path},
		{"subagents.path", c.Subagents.Path},
	} {
		if d.path == "" {
			continue
		}
		if pathsOverlap(d.path, knowledge, true) {
			return fmt.Errorf("%s: %q is equal to, inside or contains the reserved knowledge root %q (okfs/); choose a separate directory", d.key, d.path, knowledge)
		}
	}

	if c.Logging.FilePath != "" {
		dir := logDirectory(c.Logging.FilePath)
		if pathsOverlap(dir, knowledge, false) {
			return fmt.Errorf("logging.file_path: the log directory %q is the reserved knowledge root %q (okfs/) or inside it", dir, knowledge)
		}
	}

	return nil
}

// logDirectory is the directory the rotating log writes into: the path
// itself for a directory form (trailing separator or an existing directory),
// otherwise its parent.
func logDirectory(p string) string {
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(filepath.Separator)) {
		return filepath.Clean(p)
	}
	// p is the operator-configured log path (already confined to the agent
	// directory by validateStateConfined); it is only inspected, never opened.
	if info, err := os.Stat(p); err == nil && info.IsDir() { //nolint:gosec // inspect-only stat of a configured path
		return filepath.Clean(p)
	}

	return filepath.Dir(filepath.Clean(p))
}

// pathsOverlap reports whether a and b are the same location or one is inside
// the other (both directions when either is true; only "a equal to or inside
// b" otherwise). Both the lexical and the physical (link-resolved) spellings
// are compared.
func pathsOverlap(a, b string, either bool) bool {
	spellings := func(p string) []string {
		out := []string{filepath.Clean(p)}
		if phys, err := confinement.Root(p); err == nil {
			out = append(out, phys)
		}

		return out
	}
	for _, x := range spellings(a) {
		for _, y := range spellings(b) {
			if relationWithin(x, y) || (either && relationWithin(y, x)) {
				return true
			}
		}
	}

	return false
}

// relationWithin reports whether child equals parent or lies inside it.
func relationWithin(child, parent string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == goosWindows {
		child, parent = strings.ToLower(child), strings.ToLower(parent)
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

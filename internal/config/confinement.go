package config

import (
	"fmt"

	"github.com/marco-rozz/ready/internal/confinement"
)

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

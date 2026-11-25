package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keySkills    = "skills.path"
	keySessions  = "sessions.path"
	keySubagents = "subagents.path"
	keyLogFile   = "logging.file_path"
)

func TestKnowledgeRootDisjoint(t *testing.T) {
	home := t.TempDir()
	okfs := filepath.Join(home, "okfs")
	if err := os.MkdirAll(okfs, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "alias")
	linked := os.Symlink(okfs, link) == nil

	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantKey string // empty = accepted
	}{
		{"defaults accepted", func(_ *Config) {}, ""},
		{"skills at agent root", func(c *Config) { c.Skills.Path = home }, keySkills},
		{"skills equal okfs", func(c *Config) { c.Skills.Path = okfs }, keySkills},
		{"skills inside okfs", func(c *Config) { c.Skills.Path = filepath.Join(okfs, "skills") }, keySkills},
		{"skills sibling accepted", func(c *Config) { c.Skills.Path = filepath.Join(home, "skills") }, ""},
		{"sessions inside okfs", func(c *Config) { c.Sessions.Path = filepath.Join(okfs, "sessions") + "/" }, keySessions},
		{"sessions at agent root", func(c *Config) { c.Sessions.Path = home }, keySessions},
		{"subagents inside okfs", func(c *Config) { c.Subagents.Path = filepath.Join(okfs, "agents") }, keySubagents},
		{"subagents sibling accepted", func(c *Config) { c.Subagents.Path = filepath.Join(home, "subagents") }, ""},
		{"log file at agent root accepted", func(c *Config) { c.Logging.FilePath = filepath.Join(home, "ready.log") }, ""},
		{"log dir sibling accepted", func(c *Config) { c.Logging.FilePath = filepath.Join(home, "logs") + "/" }, ""},
		{"log file inside okfs", func(c *Config) { c.Logging.FilePath = filepath.Join(okfs, "ready.log") }, keyLogFile},
		{"log dir form okfs", func(c *Config) { c.Logging.FilePath = okfs + "/" }, keyLogFile},
		{"log dir inside okfs", func(c *Config) { c.Logging.FilePath = filepath.Join(okfs, "logs") + "/" }, keyLogFile},
		{"log existing dir okfs", func(c *Config) { c.Logging.FilePath = okfs }, keyLogFile},
		{"skills via symlink to okfs", func(c *Config) { c.Skills.Path = link }, keySkills},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.name, "symlink") && !linked {
				t.Skip("symlinks unavailable")
			}
			c := &Config{RuntimeHome: home}
			c.Skills.Path = filepath.Join(home, "skills")
			c.Sessions.Path = filepath.Join(home, "sessions")
			tc.mutate(c)
			err := c.validateKnowledgeRootDisjoint()
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}
			if err == nil {
				t.Fatalf("expected a %s error", tc.wantKey)
			}
			if !strings.HasPrefix(err.Error(), tc.wantKey+":") || !strings.Contains(err.Error(), "okfs") {
				t.Fatalf("error must name %s and the reserved root: %v", tc.wantKey, err)
			}
		})
	}
}

func TestKnowledgeRootNoRuntimeHome(t *testing.T) {
	c := &Config{}
	c.Skills.Path = "okfs"
	if err := c.validateKnowledgeRootDisjoint(); err != nil {
		t.Fatalf("no boundary without a runtime home: %v", err)
	}
}

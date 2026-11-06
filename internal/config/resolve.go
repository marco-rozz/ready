package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OriginKind names how an agent directory was selected. It answers
// "who chose it": the explicit --agent-dir flag, or the unconditional
// per-user default.
type OriginKind string

const (
	// OriginFlag marks an explicitly passed --agent-dir. No other source is
	// ever consulted for a flagged selection.
	OriginFlag OriginKind = "flag"
	// OriginDefault marks the unconditional ~/.ready default selection.
	OriginDefault OriginKind = "default"
)

// Runtime-home layout constants. Every runtime path of a daemon run resolves
// against the selected agent directory.
const (
	// ConfigFileName is the sole configuration filename inside an agent
	// directory. No alternative filename is ever accepted.
	ConfigFileName = "config.yaml"
	// SecretKeysDirName is the agent-local directory of generated private
	// credential files (secrets/<instance>.key and friends). Init creates
	// it 0700; every file inside is 0600.
	SecretKeysDirName = "secrets"
	// dirNameReady is the default agent directory's name under the user home.
	dirNameReady = ".ready"
	// defaultSkillsDir is the default skills directory inside the agent
	// directory.
	defaultSkillsDir = "skills"
	// defaultOAuthDir is the OAuth credentials directory inside the agent
	// directory.
	defaultOAuthDir = "oauth"
	// defaultSessionsDir is the default sessions directory inside the agent
	// directory.
	defaultSessionsDir = "sessions"
)

// Resolution is the selected agent directory: its physical path, the config
// file inside it and how the directory was selected.
type Resolution struct {
	// AgentDir is the selected physical agent directory (cleaned, links in
	// every path component resolved — an alias spelling selects the same
	// physical agent and therefore the same state paths).
	AgentDir string
	// Path is the sole config file inside the agent directory.
	Path string
	// Origin records the selecting source for boot logging.
	Origin OriginKind
}

// SelectAgentDir resolves exactly one agent directory: the explicit
// --agent-dir (which must exist and be a directory — no fallback, no
// creation), else unconditionally the user's ~/.ready, independent of the
// working directory. The resolution never merges settings and never searches
// cwd or parents.
func SelectAgentDir(ctx context.Context, explicit string) (Resolution, error) {
	select {
	case <-ctx.Done():
		return Resolution{}, fmt.Errorf("failed to select agent directory: %w", ctx.Err())
	default:
	}

	if explicit != "" {
		info, err := os.Stat(explicit)
		if err != nil {
			return Resolution{}, fmt.Errorf("agent directory %s: %w", explicit, err)
		}
		if abs, aerr := filepath.Abs(explicit); aerr == nil {
			explicit = abs
		}
		if !info.IsDir() {
			return Resolution{}, fmt.Errorf("agent directory %s is not a directory", explicit)
		}
		dir, dirErr := resolvePhysicalDir(filepath.Clean(explicit), true)
		if dirErr != nil {
			return Resolution{}, dirErr
		}

		return Resolution{AgentDir: dir, Path: filepath.Join(dir, ConfigFileName), Origin: OriginFlag}, nil
	}

	defaultDir, err := InstallHome()
	if err != nil {
		return Resolution{}, fmt.Errorf("cannot resolve the default agent directory: %w (pass --agent-dir explicitly)", err)
	}

	physical, physErr := resolvePhysicalDir(defaultDir, false)
	if physErr != nil {
		return Resolution{}, physErr
	}

	return Resolution{AgentDir: physical, Path: filepath.Join(physical, ConfigFileName), Origin: OriginDefault}, nil
}

// SelectAgentDirForInit resolves the guided-init target with the same
// physical-identity semantics as SelectAgentDir (alias spellings resolve to
// one physical directory) but does NOT require the directory to exist:
// only init may bootstrap a missing agent directory. The caller creates it
// during publication. Known misspellings of config.yaml are not involved:
// the target is always the directory, never a file.
func SelectAgentDirForInit(ctx context.Context, explicit string) (Resolution, error) {
	select {
	case <-ctx.Done():
		return Resolution{}, fmt.Errorf("failed to select agent directory: %w", ctx.Err())
	default:
	}

	if explicit != "" {
		if info, err := os.Stat(explicit); err == nil && !info.IsDir() {
			return Resolution{}, fmt.Errorf("agent directory %s is not a directory", explicit)
		}
		if abs, aerr := filepath.Abs(explicit); aerr == nil {
			explicit = abs
		}
		dir, dirErr := resolvePhysicalDir(filepath.Clean(explicit), false)
		if dirErr != nil {
			return Resolution{}, dirErr
		}

		return Resolution{AgentDir: dir, Path: filepath.Join(dir, ConfigFileName), Origin: OriginFlag}, nil
	}

	defaultDir, err := InstallHome()
	if err != nil {
		return Resolution{}, fmt.Errorf("cannot resolve the default agent directory: %w (pass --agent-dir explicitly)", err)
	}

	physical, physErr := resolvePhysicalDir(defaultDir, false)
	if physErr != nil {
		return Resolution{}, physErr
	}

	return Resolution{AgentDir: physical, Path: filepath.Join(physical, ConfigFileName), Origin: OriginDefault}, nil
}

// resolvePhysicalDir resolves symlinks and junctions in every path component
// so alias spellings select one physical directory. An existing directory is
// resolved whole; for a missing leaf (the default agent directory of a fresh
// machine) the parent is resolved and the leaf rejoined, so the eventual
// creation lands at the same physical identity. Evaluation is case-normalised
// on case-insensitive filesystems (Windows/macOS), so distinct casings
// select the same identity.
func resolvePhysicalDir(dir string, mustExist bool) (string, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(resolved), nil
	} else if !mustExist && os.IsNotExist(err) {
		parent := filepath.Dir(dir)
		resolved, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			return "", fmt.Errorf("agent directory %s: %w", dir, perr)
		}

		return filepath.Join(filepath.Clean(resolved), filepath.Base(dir)), nil
	} else if err != nil {
		return "", fmt.Errorf("agent directory %s: %w", dir, err)
	}

	return filepath.Clean(dir), nil
}

// RejectLinkedConfig checks the sole config file leaf: a symlink or other
// non-regular file redirecting config.yaml elsewhere would relax the
// directory boundary, so a linked config leaf is rejected.
func RejectLinkedConfig(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config %s must be a regular file, not a link or special file", path)
	}

	return nil
}

// SharedLibraryDir is the installer-owned skill library shared read-only by
// every agent on this machine (outside any agent directory). An empty
// result means the user home could not be resolved (the sync then no-ops).
func SharedLibraryDir() string {
	home, err := userHome()
	if err != nil || home == "" {
		return ""
	}

	return filepath.Join(home, ".local", "share", "ready", "library", "skills")
}

// InstallHome resolves the per-user default agent directory ($HOME/.ready on
// unix, %USERPROFILE%\.ready on Windows) lazily — callers that never need it
// never pay for (or fail on) the OS user-home lookup.
func InstallHome() (string, error) {
	home, err := userHome()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, dirNameReady), nil
}

func userHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// windows: %USERPROFILE% unset; unix: not found.
		return "", fmt.Errorf("cannot resolve the user home directory: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf("the user home directory is empty")
	}

	return home, nil
}

// ApplyRuntimeHome fills the runtime-home-dependent defaults onto a freshly
// unmarshalled config: the runtime home from the selected config path, the
// default skills and sessions directories inside it, every explicitly
// relative runtime path against it, and the OAuth credential store, fixed at
// <agent-dir>/oauth. The store path is runtime-derived state, never author
// config (the oauth.path YAML key is rejected).
func (c *Config) ApplyRuntimeHome(path string) error {
	c.ElectedPath = path
	c.RuntimeHome = filepath.Dir(filepath.Clean(path))
	c.applyRuntimeHomePaths()
	c.applySessionDefaults()

	return c.applyOAuthStore()
}

// applyRuntimeHomePaths resolves the runtime layout onto the runtime home:
// the default skills/sessions directories inside it, and every explicitly
// relative path (sessions, skills, log file, oauth) against it — an absolute
// path is honoured as-is. With an empty RuntimeHome (a Config constructed
// directly in Go, e.g. in tests) the resolution degrades to the working
// directory. The ElectedPath guard keeps configs already resolved by Load
// from being re-processed by applyDefaults.
func (c *Config) applyRuntimeHomePaths() {
	if c.runtimeHomesApplied {
		return
	}
	c.runtimeHomesApplied = true

	c.Skills.Path = resolveRelPath(c.Skills.Path, c.RuntimeHome, defaultSkillsDir)
	c.Sessions.Path = resolveRelPath(c.Sessions.Path, c.RuntimeHome, defaultSessionsDir)
	c.Logging.FilePath = resolveRelPath(c.Logging.FilePath, c.RuntimeHome, "")

	// The credential store is fixed inside the agent directory (no override):
	// every consumer — daemon, login, forget — resolves the same records, and
	// a moved agent directory keeps its credentials. A direct construction
	// without a runtime home leaves the seam to the caller.
	if c.RuntimeHome != "" {
		c.OAuth.Path = filepath.Join(c.RuntimeHome, defaultOAuthDir)
		c.LLM.OAuthPath = c.OAuth.Path
	}

	// The skill library: an explicit absolute path as-is (read-only source
	// override), a relative one resolved against the runtime home (dev
	// configs point it at ../resources/skills/). The default (empty) anchors
	// to program storage: ~/.local/share/ready/library/skills on both
	// platforms — installer-owned content shared read-only by every agent,
	// never agent state. An unresolvable user home just leaves it empty —
	// the sync then no-ops like an absent library (never a boot error).
	c.Skills.LibraryPath = resolveRelPath(c.Skills.LibraryPath, c.RuntimeHome, "")
	if c.Skills.LibraryPath == "" {
		if home, err := userHome(); err == nil && home != "" {
			c.Skills.LibraryPath = filepath.Join(home, ".local", "share", "ready", "library", "skills")
		}
	}
}

// resolveRelPath joins p against the runtime home when p is relative;
// an empty p falls back to the provided default dir inside the runtime home
// (also joined), so the default stays empty-safe. A trailing directory
// separator is preserved (the log-file directory form is sensitive to it).
func resolveRelPath(p, runtimeHome, defaultDir string) string {
	if p == "" && defaultDir != "" {
		return filepath.Join(runtimeHome, defaultDir)
	}
	if p == "" || filepath.IsAbs(p) {
		return p
	}

	trailing := strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(os.PathSeparator))
	joined := filepath.Join(runtimeHome, p)
	if trailing {
		joined += "/"
	}

	return joined
}

// applyOAuthStore keeps the runtime-resolved store in sync with the copy
// surfaced to the LLM layer, and leaves the direct-construction seam alone:
// a Config built directly in Go (ElectedPath empty) keeps the path its
// constructor set, defaulting to nothing.
func (c *Config) applyOAuthStore() error {
	if c.OAuth.Path == "" && c.ElectedPath == "" {
		// Direct construction without ApplyRuntimeHome: leave the explicit
		// defaulting to the caller.
		return nil
	}
	c.LLM.OAuthPath = c.OAuth.Path

	return nil
}

// HasOAuthTargets reports whether anything will ever consult the OAuth
// credential store: an MCP server with an oauth block or an openai-codex LLM
// provider.
func (c *Config) HasOAuthTargets() bool {
	for _, srv := range c.MCPServers {
		if srv.OAuth != nil {
			return true
		}
	}
	for _, provider := range c.LLM.Providers {
		if provider.Provider == ProviderOpenAICodex {
			return true
		}
	}

	return false
}

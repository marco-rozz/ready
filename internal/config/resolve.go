package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OriginKind names how an elected config file was found. It answers
// "who chose it": the explicit --config flag, the local ./.ready/config.yaml
// relative to the working directory, or the global config under the Install
// home.
type OriginKind string

const (
	// OriginFlag marks an explicitly passed --config path. No other source
	// is ever consulted for a flagged election.
	OriginFlag OriginKind = "flag"
	// OriginLocal marks the local ./.ready/config.yaml candidate.
	OriginLocal OriginKind = "local"
	// OriginGlobal marks the global ~/.ready/config.yaml fallback.
	OriginGlobal OriginKind = "global"
)

// Runtime-home layout constants. Every runtime path of a daemon run resolves
// against the directory containing the elected config file.
const (
	// LocalConfigRel is the cwd-relative local candidate of the resolution
	// chain (flag > local > global).
	LocalConfigRel = ".ready/config.yaml"
	// dirNameReady is the Install home's directory name under the user home.
	dirNameReady = ".ready"
	// defaultSkillsDir is the default skills directory inside the runtime
	// home.
	defaultSkillsDir = "skills"
	// defaultOAuthDir is the default OAuth credentials directory inside the
	// Install home (the deliberate carve-out from the runtime-home rule).
	defaultOAuthDir = "oauth"
	// defaultSessionsDir is the default sessions directory inside the
	// runtime home.
	defaultSessionsDir = "sessions"
)

// ConfigurationNotFound is returned by Elect when no config file could be
// elected and no explicit path was given. Callers render it as a single
// human-readable boot failure.
type ConfigurationNotFound struct {
	Candidates []string
}

func (e *ConfigurationNotFound) Error() string {
	return fmt.Sprintf("no configuration file found: searched %s", joinQuoted(e.Candidates))
}

func joinQuoted(paths []string) string {
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", p)
	}

	return out
}

// Resolution is the elected config file: its path and how it was elected.
type Resolution struct {
	// Path is the elected config file path, as the electing source named it
	// (an explicit relative flag stays relative; the chain candidates are
	// resolved relative to the working directory resp. the user home).
	Path string
	// Origin records the electing source for boot logging.
	Origin OriginKind
}

// RuntimeHome is the directory containing the elected config file. Every
// runtime path (session files, log segments, runtime skill copy) of the run
// resolves against it. There is deliberately no env or flag override for it.
func (r Resolution) RuntimeHome() string {
	return filepath.Dir(filepath.Clean(r.Path))
}

// Elect resolves exactly one configuration file, in strict order: the
// explicit path (when non-empty — no fallback, Load names it when missing),
// then the local ./.ready/config.yaml, then the global ~/.ready/config.yaml.
// No settings are ever merged: the rejected candidates have no influence on
// the run.
func Elect(ctx context.Context, explicit string) (Resolution, error) {
	select {
	case <-ctx.Done():
		return Resolution{}, fmt.Errorf("failed to elect config: %w", ctx.Err())
	default:
	}

	if explicit != "" {
		return Resolution{Path: explicit, Origin: OriginFlag}, nil
	}

	local := "./" + LocalConfigRel
	if _, err := os.Stat(local); err == nil {
		return Resolution{Path: local, Origin: OriginLocal}, nil
	} else if !os.IsNotExist(err) {
		return Resolution{}, fmt.Errorf("cannot read %s: %w", local, err)
	}

	installHome, err := InstallHome()
	if err != nil {
		return Resolution{}, fmt.Errorf("user home is undeterminable (%w); searched the local candidate %s and there is no explicit config", err, local)
	}

	global := filepath.Join(installHome, "config.yaml")
	if _, err := os.Stat(global); err == nil {
		return Resolution{Path: global, Origin: OriginGlobal}, nil
	} else if !os.IsNotExist(err) {
		return Resolution{}, fmt.Errorf("cannot read %s: %w", global, err)
	}

	return Resolution{}, &ConfigurationNotFound{Candidates: []string{local, global}}
}

// InstallHome resolves the per-user Install home ($HOME/.ready on unix,
// %USERPROFILE%\.ready on Windows) lazily — callers that never need it never
// pay for (or fail on) the OS user-home lookup.
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
// unmarshalled config: the runtime home from the elected config path, the
// default skills and sessions directories inside it, every explicitly
// relative runtime path against it, and the OAuth credential store (the one
// deliberate carve-out: its default anchors to the Install home). Errors
// only when the OAuth default is actually needed and the Install home cannot
// be resolved — the remedy is named in the message.
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
	c.OAuth.Path = resolveRelPath(c.OAuth.Path, c.RuntimeHome, "")
	c.LLM.OAuthPath = c.OAuth.Path

	// The skill library: an explicit absolute path as-is, a relative one
	// resolved against the runtime home (dev configs point it at
	// ../resources/skills/). The default (empty) anchors to the Install
	// home; an unresolvable user home just leaves it empty — the sync then
	// no-ops like an absent library (never a boot error).
	c.Skills.LibraryPath = resolveRelPath(c.Skills.LibraryPath, c.RuntimeHome, "")
	if c.Skills.LibraryPath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			c.Skills.LibraryPath = filepath.Join(home, dirNameReady, "library", "skills")
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

// applyOAuthStore resolves the OAuth credential store: an
// explicit path wins (already resolved against the runtime home by
// applyRuntimeHomePaths; an absolute path is cleaned as-is); the default
// anchors to the Install home and is only computed when something will
// consume it — with no consumer an empty path is simply left empty, and
// resolving the default for a Config built directly in Go is the caller's
// forgotten step whose symptom (an empty store path error at first use)
// names the remedy.
func (c *Config) applyOAuthStore() error {
	if c.OAuth.Path == "" && c.ElectedPath == "" {
		// Direct construction without ApplyRuntimeHome: leave the explicit
		// defaulting to the caller.
		return nil
	}
	if c.OAuth.Path != "" {
		c.LLM.OAuthPath = c.OAuth.Path

		return nil
	}
	if !c.HasOAuthTargets() {
		return nil
	}

	installHome, err := InstallHome()
	if err != nil {
		return fmt.Errorf("cannot resolve the default OAuth credential store: %w — set oauth.path explicitly to proceed without an Install home", err)
	}
	c.OAuth.Path = filepath.Join(installHome, defaultOAuthDir)
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

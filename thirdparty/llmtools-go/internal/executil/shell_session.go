package executil

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

type envEntry struct {
	key string
	val string
}

type ShellSession struct {
	id      string
	workdir string
	env     map[string]string
	mu      sync.RWMutex
	closed  bool
}

// EffectiveEnv returns the current process environment merged with overrides.
// It is equivalent to session-less ShellSession.GetEffectiveEnv.
func EffectiveEnv(overrides map[string]string) ([]string, error) {
	return EffectiveEnvWithBase(nil, overrides)
}

// EffectiveEnvWithBase returns the current process environment merged with base env and overrides.
func EffectiveEnvWithBase(baseEnv, overrides map[string]string) ([]string, error) {
	// Nil receiver is safe: ShellSession.GetEffectiveEnvWithBase checks sess != nil before reading session state.
	return (*ShellSession)(nil).GetEffectiveEnvWithBase(baseEnv, overrides)
}

func (sess *ShellSession) GetID() string {
	return sess.id
}

func (sess *ShellSession) SetWorkDir(workdir string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.workdir = workdir
}

func (sess *ShellSession) AddToEnv(additionalEnv map[string]string) error {
	if len(additionalEnv) == 0 {
		return nil
	}

	if err := ValidateEnvMap(additionalEnv); err != nil {
		return err
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed {
		return errors.New("session is closed")
	}

	if sess.env == nil {
		sess.env = map[string]string{}
	}
	if runtime.GOOS == toolutil.GOOSWindows {
		// Rebuild session env to canonical keys to avoid case-insensitive duplicates
		// causing nondeterministic behavior.
		canon := make(map[string]string, len(sess.env)+len(additionalEnv))
		for k, v := range sess.env {
			kk := strings.ToUpper(strings.TrimSpace(k))
			if kk == "" {
				continue
			}
			canon[kk] = v
		}
		for k, v := range additionalEnv {
			kk := strings.ToUpper(strings.TrimSpace(k))
			if kk == "" {
				continue
			}
			canon[kk] = v
		}
		sess.env = canon
	} else {
		maps.Copy(sess.env, additionalEnv)
	}

	return nil
}

func (sess *ShellSession) GetEffectiveEnv(overrides map[string]string) ([]string, error) {
	return sess.GetEffectiveEnvWithBase(nil, overrides)
}

func (sess *ShellSession) GetEffectiveEnvWithBase(baseEnv, overrides map[string]string) ([]string, error) {
	// Start with current process env.
	envMap := map[string]envEntry{}

	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			kk := strings.TrimSpace(k)
			if kk == "" || strings.ContainsRune(kk, '\x00') || strings.ContainsRune(v, '\x00') ||
				strings.Contains(kk, "=") {
				continue
			}
			ck := canonicalEnvKey(kk)
			envMap[ck] = envEntry{key: kk, val: v}
		}
	}

	applyEnv := func(m map[string]string, validate bool, label string) error {
		for k, v := range m {
			if validate {
				if err := validateEnvKV(k, v); err != nil {
					return fmt.Errorf("invalid %s env %q: %w", label, k, err)
				}
			}
			kk := strings.TrimSpace(k)
			if kk == "" {
				continue
			}
			ck := canonicalEnvKey(kk)
			envMap[ck] = envEntry{key: kk, val: v}
		}
		return nil
	}

	// Then tool base env.
	if len(baseEnv) != 0 {
		if err := applyEnv(baseEnv, true, "base"); err != nil {
			return nil, err
		}
	}

	// Apply platform-specific fresh environment overlays.
	//
	// Windows:
	//   Reads Machine/User env from registry so MSI-installed tools become
	//   visible without restarting the assistant process.
	//
	// macOS:
	//   Reads /etc/paths and /etc/paths.d so installer-added PATH entries become
	//   visible without recreating ExecTool.
	applyPlatformEnvOverlay(envMap)

	// Then session env.
	if sess != nil {
		sess.mu.RLock()
		closed := sess.closed
		snap := maps.Clone(sess.env)
		sess.mu.RUnlock()
		if closed {
			return nil, errors.New("session is closed")
		}
		if err := applyEnv(snap, true, "session"); err != nil {
			return nil, err
		}
	}

	// Then per-call overrides.
	if len(overrides) != 0 {
		if err := applyEnv(overrides, true, "override"); err != nil {
			return nil, err
		}
	}

	// Stable ordering (deterministic across runs).
	entries := make([]envEntry, 0, len(envMap))
	for _, e := range envMap {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		// Compare canonical keys for stable ordering across Windows/unix.
		return canonicalEnvKey(entries[i].key) < canonicalEnvKey(entries[j].key)
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.key+"="+e.val)
	}
	return out, nil
}

func (sess *ShellSession) GetEffectiveWorkdir(inputWorkDir, defaultWorkDir string) (string, error) {
	if sess == nil {
		return "", errors.New("invalid session")
	}
	sess.mu.RLock()
	wd := sess.workdir
	closed := sess.closed
	sess.mu.RUnlock()
	if closed {
		return "", errors.New("session is closed")
	}

	var checkWorkDir string
	if strings.TrimSpace(inputWorkDir) != "" { //nolint:gocritic // Dont want this to be a switch.
		checkWorkDir = inputWorkDir
	} else if strings.TrimSpace(wd) != "" {
		checkWorkDir = wd
	} else if strings.TrimSpace(defaultWorkDir) != "" {
		checkWorkDir = defaultWorkDir
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		checkWorkDir = cwd
	}
	if checkWorkDir == "" {
		return "", errors.New("got invalid workdir")
	}
	return checkWorkDir, nil
}

func ValidateEnvMap(m map[string]string) error {
	seen := map[string]string{}
	for k, v := range m {
		if err := validateEnvKV(k, v); err != nil {
			return fmt.Errorf("env %q: %w", k, err)
		}
		kk := strings.TrimSpace(k)
		ck := canonicalEnvKey(kk)
		if prev, ok := seen[ck]; ok {
			return fmt.Errorf("duplicate env name %q conflicts with %q", kk, prev)
		}
		seen[ck] = kk
	}
	return nil
}

func validateEnvKV(k, v string) error {
	kk := strings.TrimSpace(k)
	if kk == "" {
		return errors.New("empty name")
	}

	if strings.ContainsRune(kk, '\x00') || strings.ContainsRune(v, '\x00') {
		return errors.New("contains NUL byte")
	}
	if strings.Contains(kk, "=") {
		return errors.New("name contains '='")
	}
	return nil
}

func canonicalEnvKey(k string) string {
	if runtime.GOOS == toolutil.GOOSWindows {
		return strings.ToUpper(k)
	}
	return k
}

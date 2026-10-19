package executil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

// LookPathInEnv is like exec.LookPath, but searches PATH/PATHEXT from env.
// If env is nil, it falls back to exec.LookPath.
//
// Empty PATH elements are intentionally not treated as ".". For tool execution
// this avoids current-directory executable hijacking.
func LookPathInEnv(file string, env []string) (string, error) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", errors.New("empty executable name")
	}
	if env == nil {
		return exec.LookPath(file)
	}

	pathVal, _ := EnvListValue(env, "PATH")
	if runtime.GOOS == toolutil.GOOSWindows {
		pathextVal, _ := EnvListValue(env, "PATHEXT")
		return lookPathWindowsEnv(file, pathVal, pathextVal)
	}
	return lookPathUnixEnv(file, pathVal)
}

// EnvListValue returns the value for key in an exec-style environment list.
// On Windows, key matching is case-insensitive.
func EnvListValue(env []string, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}

	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if runtime.GOOS == toolutil.GOOSWindows {
			if strings.EqualFold(k, key) {
				return v, true
			}
			continue
		}
		if k == key {
			return v, true
		}
	}
	return "", false
}

func lookPathUnixEnv(file, pathVal string) (string, error) {
	if strings.ContainsRune(file, os.PathSeparator) {
		if isUnixExecutable(file) {
			return file, nil
		}
		return "", fmt.Errorf("executable not found: %s", file)
	}

	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, file)
		if isUnixExecutable(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("executable %q not found on PATH", file)
}

func isUnixExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if st.IsDir() {
		return false
	}
	return st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func lookPathWindowsEnv(file, pathVal, pathextVal string) (string, error) {
	pathexts := parseWindowsPathExt(pathextVal)

	if windowsPathLike(file) {
		return findWindowsExecutableCandidate(file, pathexts)
	}

	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			continue
		}
		candidateBase := filepath.Join(dir, file)
		if p, err := findWindowsExecutableCandidate(candidateBase, pathexts); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("executable %q not found on PATH", file)
}

func windowsPathLike(s string) bool {
	return filepath.IsAbs(s) || filepath.VolumeName(s) != "" || strings.ContainsAny(s, `/\`)
}

func findWindowsExecutableCandidate(base string, pathexts []string) (string, error) {
	if filepath.Ext(base) != "" {
		if fileExistsNotDir(base) {
			return base, nil
		}
		return "", fmt.Errorf("executable not found: %s", base)
	}

	for _, ext := range pathexts {
		candidate := base + ext
		if fileExistsNotDir(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("executable not found: %s", base)
}

func parseWindowsPathExt(pathextVal string) []string {
	if strings.TrimSpace(pathextVal) == "" {
		pathextVal = ".COM;.EXE;.BAT;.CMD"
	}

	seen := map[string]struct{}{}
	out := make([]string, 0, 8)
	for ext := range strings.SplitSeq(pathextVal, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		key := strings.ToUpper(ext)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ext)
	}
	if len(out) == 0 {
		return []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	return out
}

func fileExistsNotDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

//go:build !windows

package executil

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	darwinEtcPaths  = "/etc/paths"
	darwinEtcPathsD = "/etc/paths.d"
	defaultPathKey  = "PATH"
)

// applyPlatformEnvOverlay appends macOS system path-helper paths to PATH.
//
// "macOS" login shells usually get PATH through /usr/libexec/path_helper, which
// reads /etc/paths and /etc/paths.d. If an installer adds a new file there after
// ExecTool was created, the cached base env can be stale. Reading these tiny
// files once per tool invocation makes installer-added paths visible without
// running another login shell.
func applyPlatformEnvOverlay(envMap map[string]envEntry) {
	if runtime.GOOS != toolutil.GOOSDarwin {
		// No generic overlay.
		// Linux and other Unix platforms do not have a single safe, cheap,
		// side-effect-free source equivalent to the Windows environment registry.
		return
	}

	if envMap == nil {
		return
	}

	applyDarwinPathHelperOverlay(envMap, readDarwinPathHelperEntriesFrom(darwinEtcPaths, darwinEtcPathsD))
}

func applyDarwinPathHelperOverlay(envMap map[string]envEntry, pathHelperEntries []string) {
	if envMap == nil {
		return
	}
	if len(pathHelperEntries) == 0 {
		return
	}

	currentPath, currentPathKey, _ := getEnvEntryValue(envMap, "PATH")
	values := make([]string, 0, 1+len(pathHelperEntries))
	values = append(values, currentPath)
	values = append(values, pathHelperEntries...)

	mergedPath := mergePathList(":", false, values)
	if strings.TrimSpace(mergedPath) == "" {
		return
	}

	key := currentPathKey
	if key == "" {
		key = defaultPathKey
	}
	setEnvEntryValue(envMap, key, mergedPath)
}

func readDarwinPathHelperEntriesFrom(pathsFile, pathsDir string) []string {
	out := make([]string, 0, 16)
	out = appendPathFileEntries(out, pathsFile)

	entries, err := os.ReadDir(pathsDir)
	if err != nil {
		return out
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		out = appendPathFileEntries(out, filepath.Join(pathsDir, name))
	}

	return out
}

func appendPathFileEntries(out []string, path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}

	for line := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		x := strings.TrimSpace(line)
		if x == "" || strings.HasPrefix(x, "#") || strings.ContainsRune(x, '\x00') {
			continue
		}
		out = append(out, x)
	}

	return out
}

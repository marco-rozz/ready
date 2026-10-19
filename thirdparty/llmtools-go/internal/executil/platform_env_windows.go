//go:build windows

package executil

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	windowsMachineEnvKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	windowsUserEnvKey    = `Environment`
)

type windowsRegistryEnvValue struct {
	key    string
	val    string
	expand bool
}

// applyPlatformEnvOverlay updates envMap with fresh Windows Machine/User
// environment values from the registry.
//
// This fixes the common MSI-installer case:
//
//  1. Assistant process starts.
//  2. Go MSI updates Machine/User Path in the registry.
//  3. Assistant process env remains stale.
//  4. Next tool invocation should still find go.exe.
//
// The overlay is best-effort. Registry read failures must not prevent command
// execution.
func applyPlatformEnvOverlay(envMap map[string]envEntry) {
	if envMap == nil {
		return
	}

	applyWindowsRegistryEnvOverlay(
		envMap,
		readWindowsRegistryEnv(registry.LOCAL_MACHINE, windowsMachineEnvKey),
		readWindowsRegistryEnv(registry.CURRENT_USER, windowsUserEnvKey),
	)
}

func applyWindowsRegistryEnvOverlay(
	envMap map[string]envEntry,
	machineRaw, userRaw map[string]windowsRegistryEnvValue,
) {
	if len(machineRaw) == 0 && len(userRaw) == 0 {
		return
	}

	machine := expandWindowsRegistryEnv(machineRaw, envMap, machineRaw, userRaw, false)
	user := expandWindowsRegistryEnv(userRaw, envMap, machineRaw, userRaw, true)

	// Apply non-Path values. Machine first, then User.
	applyWindowsRegistryEnvValues(envMap, machine)
	applyWindowsRegistryEnvValues(envMap, user)

	// Path is special. Replacing the current Path outright can drop
	// process-specific entries. Instead, preserve current effective Path and
	// append fresh Machine/User registry entries, deduplicated case-insensitively.
	currentPath, currentPathKey, _ := getEnvEntryValue(envMap, "Path")
	machinePath, machinePathKey := lookupWindowsExpandedEnv(machine, "Path")
	userPath, userPathKey := lookupWindowsExpandedEnv(user, "Path")

	mergedPath := mergePathValues(";", true, currentPath, machinePath, userPath)
	if strings.TrimSpace(mergedPath) == "" {
		return
	}

	key := currentPathKey
	if key == "" {
		key = machinePathKey
	}
	if key == "" {
		key = userPathKey
	}
	if key == "" {
		key = "Path"
	}

	setEnvEntryValue(envMap, key, mergedPath)
}

func readWindowsRegistryEnv(root registry.Key, path string) map[string]windowsRegistryEnvValue {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()

	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil
	}

	out := make(map[string]windowsRegistryEnvValue, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "=") {
			continue
		}

		val, typ, err := k.GetStringValue(name)
		if err != nil {
			continue
		}
		if typ != registry.SZ && typ != registry.EXPAND_SZ {
			continue
		}
		if strings.ContainsRune(val, '\x00') {
			continue
		}

		out[canonicalEnvKey(name)] = windowsRegistryEnvValue{
			key:    name,
			val:    val,
			expand: typ == registry.EXPAND_SZ,
		}
	}

	return out
}

func expandWindowsRegistryEnv(
	raw map[string]windowsRegistryEnvValue,
	envMap map[string]envEntry,
	machineRaw map[string]windowsRegistryEnvValue,
	userRaw map[string]windowsRegistryEnvValue,
	includeUser bool,
) map[string]envEntry {
	if len(raw) == 0 {
		return nil
	}

	out := make(map[string]envEntry, len(raw))
	for ck, rv := range raw {
		val := expandWindowsRegistryRawValue(rv, ck, envMap, machineRaw, userRaw, includeUser, 0)
		if validateEnvKV(rv.key, val) != nil {
			continue
		}
		out[ck] = envEntry{key: rv.key, val: val}
	}
	return out
}

func expandWindowsRegistryRawValue(
	rv windowsRegistryEnvValue,
	selfKey string,
	envMap map[string]envEntry,
	machineRaw map[string]windowsRegistryEnvValue,
	userRaw map[string]windowsRegistryEnvValue,
	includeUser bool,
	depth int,
) string {
	if !rv.expand || depth > 8 {
		return rv.val
	}

	return expandWindowsPercentEnv(rv.val, func(name string) (string, bool) {
		return lookupWindowsExpansionValue(
			name,
			selfKey,
			envMap,
			machineRaw,
			userRaw,
			includeUser,
			depth+1,
		)
	})
}

func lookupWindowsExpansionValue(
	name string,
	selfKey string,
	envMap map[string]envEntry,
	machineRaw map[string]windowsRegistryEnvValue,
	userRaw map[string]windowsRegistryEnvValue,
	includeUser bool,
	depth int,
) (string, bool) {
	if strings.TrimSpace(name) == "" || depth > 8 {
		return "", false
	}

	ck := canonicalEnvKey(name)
	if ck == selfKey {
		return "", false
	}

	if includeUser {
		if rv, ok := userRaw[ck]; ok {
			return expandWindowsRegistryRawValue(rv, ck, envMap, machineRaw, userRaw, true, depth), true
		}
	}

	if rv, ok := machineRaw[ck]; ok {
		return expandWindowsRegistryRawValue(rv, ck, envMap, machineRaw, userRaw, false, depth), true
	}

	if e, ok := envMap[ck]; ok {
		return e.val, true
	}

	return "", false
}

func expandWindowsPercentEnv(s string, lookup func(name string) (string, bool)) string {
	if !strings.Contains(s, "%") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}

		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}

		end := i + 1 + j
		name := s[i+1 : end]
		if name == "" {
			b.WriteString("%%")
			i = end + 1
			continue
		}

		if val, ok := lookup(name); ok {
			b.WriteString(val)
		} else {
			b.WriteString(s[i : end+1])
		}
		i = end + 1
	}

	return b.String()
}

func applyWindowsRegistryEnvValues(envMap, values map[string]envEntry) {
	for _, e := range values {
		if strings.EqualFold(e.key, "Path") {
			continue
		}
		setEnvEntryValue(envMap, e.key, e.val)
	}
}

func lookupWindowsExpandedEnv(values map[string]envEntry, key string) (value, actualKey string) {
	if len(values) == 0 {
		return "", ""
	}
	e, ok := values[canonicalEnvKey(key)]
	if !ok {
		return "", ""
	}
	return e.val, e.key
}

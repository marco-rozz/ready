package executil

import "strings"

func getEnvEntryValue(envMap map[string]envEntry, key string) (value, actualKey string, ok bool) {
	if envMap == nil {
		return "", "", false
	}
	e, ok := envMap[canonicalEnvKey(key)]
	if !ok {
		return "", "", false
	}
	return e.val, e.key, true
}

func setEnvEntryValue(envMap map[string]envEntry, key, val string) bool {
	if envMap == nil {
		return false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	if err := validateEnvKV(key, val); err != nil {
		return false
	}
	envMap[canonicalEnvKey(key)] = envEntry{key: key, val: val}
	return true
}

func mergePathList(listSep string, caseInsensitive bool, values []string) string {
	if len(values) == 0 {
		return ""
	}
	return mergePathValues(listSep, caseInsensitive, values...)
}

func mergePathValues(listSep string, caseInsensitive bool, values ...string) string {
	seen := map[string]struct{}{}
	out := make([]string, 0)

	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		for one := range strings.SplitSeq(v, listSep) {
			x := strings.TrimSpace(one)
			if x == "" {
				continue
			}

			key := x
			if caseInsensitive {
				key = strings.ToUpper(key)
			}
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			out = append(out, x)
		}
	}

	return strings.Join(out, listSep)
}

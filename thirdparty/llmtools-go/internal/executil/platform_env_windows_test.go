//go:build windows

package executil

import "testing"

type windowsRawPair struct {
	key    string
	val    string
	expand bool
}

func TestApplyWindowsRegistryEnvOverlay(t *testing.T) {
	cases := []struct {
		name        string
		initial     map[string]string
		machineRaw  map[string]windowsRegistryEnvValue
		userRaw     map[string]windowsRegistryEnvValue
		wantEntries map[string]string
		wantPathKey string
	}{
		{
			name: "path_preserves_current_then_appends_machine_user_and_dedups_case_insensitive",
			initial: map[string]string{
				"PATH":        `C:\Existing;C:\Go\bin`,
				"USERPROFILE": `C:\Users\me`,
			},
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "Path", val: `c:\go\BIN;C:\Windows\System32`},
				windowsRawPair{key: "GOROOT", val: `C:\Program Files\Go`},
			),
			userRaw: windowsRawEnv(
				windowsRawPair{key: "Path", val: `C:\Users\me\go\bin`},
				windowsRawPair{key: "GOPATH", val: `%USERPROFILE%\go`, expand: true},
			),
			wantEntries: map[string]string{
				"Path":        `C:\Existing;C:\Go\bin;C:\Windows\System32;C:\Users\me\go\bin`,
				"GOROOT":      `C:\Program Files\Go`,
				"GOPATH":      `C:\Users\me\go`,
				"USERPROFILE": `C:\Users\me`,
			},
			wantPathKey: "PATH",
		},
		{
			name: "user_non_path_overrides_machine_non_path",
			initial: map[string]string{
				"FOO": "process",
			},
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "FOO", val: "machine"},
				windowsRawPair{key: "BAR", val: "machine-bar"},
			),
			userRaw: windowsRawEnv(
				windowsRawPair{key: "foo", val: "user"},
			),
			wantEntries: map[string]string{
				"FOO": "user",
				"BAR": "machine-bar",
			},
		},
		{
			name:    "no_existing_path_uses_registry_key_and_merges_machine_user",
			initial: map[string]string{},
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "Path", val: `C:\Machine\bin`},
			),
			userRaw: windowsRawEnv(
				windowsRawPair{key: "PATH", val: `C:\User\bin`},
			),
			wantEntries: map[string]string{
				"Path": `C:\Machine\bin;C:\User\bin`,
			},
			wantPathKey: "Path",
		},
		{
			name: "empty_raw_maps_leave_initial_unchanged",
			initial: map[string]string{
				"PATH": `C:\Existing`,
				"FOO":  "bar",
			},
			machineRaw: nil,
			userRaw:    nil,
			wantEntries: map[string]string{
				"Path": `C:\Existing`,
				"FOO":  "bar",
			},
			wantPathKey: "PATH",
		},
		{
			name: "invalid_registry_values_are_skipped",
			initial: map[string]string{
				"PATH": `C:\Existing`,
			},
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "OK", val: "1"},
				windowsRawPair{key: "BAD", val: "a\x00b"},
			),
			userRaw: nil,
			wantEntries: map[string]string{
				"Path": `C:\Existing`,
				"OK":   "1",
				"BAD":  "",
			},
			wantPathKey: "PATH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envMap := map[string]envEntry{}
			for k, v := range tc.initial {
				setEnvEntryValue(envMap, k, v)
			}

			applyWindowsRegistryEnvOverlay(envMap, tc.machineRaw, tc.userRaw)

			for key, want := range tc.wantEntries {
				got, actualKey, ok := getEnvEntryValue(envMap, key)
				if want == "" {
					if ok {
						t.Fatalf("did not expect %q, got key=%q value=%q", key, actualKey, got)
					}
					continue
				}
				if !ok {
					t.Fatalf("expected env key %q", key)
				}
				if got != want {
					t.Fatalf("%s got %q want %q", key, got, want)
				}
				if key == "Path" && tc.wantPathKey != "" && actualKey != tc.wantPathKey {
					t.Fatalf("Path actual key got %q want %q", actualKey, tc.wantPathKey)
				}
			}
		})
	}
}

func TestExpandWindowsRegistryEnv(t *testing.T) {
	cases := []struct {
		name        string
		envMap      map[string]string
		machineRaw  map[string]windowsRegistryEnvValue
		userRaw     map[string]windowsRegistryEnvValue
		raw         map[string]windowsRegistryEnvValue
		includeUser bool
		want        map[string]string
	}{
		{
			name: "machine_expansion_uses_machine_then_process",
			envMap: map[string]string{
				"USERPROFILE": `C:\Users\me`,
			},
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "BASE", val: `C:\Base`},
				windowsRawPair{key: "BIN", val: `%BASE%\bin`, expand: true},
				windowsRawPair{key: "HOME_BIN", val: `%USERPROFILE%\bin`, expand: true},
			),
			raw: windowsRawEnv(
				windowsRawPair{key: "BASE", val: `C:\Base`},
				windowsRawPair{key: "BIN", val: `%BASE%\bin`, expand: true},
				windowsRawPair{key: "HOME_BIN", val: `%USERPROFILE%\bin`, expand: true},
			),
			includeUser: false,
			want: map[string]string{
				"BASE":     `C:\Base`,
				"BIN":      `C:\Base\bin`,
				"HOME_BIN": `C:\Users\me\bin`,
			},
		},
		{
			name: "user_expansion_prefers_user_over_machine",
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "BASE", val: `C:\MachineBase`},
			),
			userRaw: windowsRawEnv(
				windowsRawPair{key: "BASE", val: `C:\UserBase`},
				windowsRawPair{key: "USERBIN", val: `%BASE%\u`, expand: true},
			),
			raw: windowsRawEnv(
				windowsRawPair{key: "BASE", val: `C:\UserBase`},
				windowsRawPair{key: "USERBIN", val: `%BASE%\u`, expand: true},
			),
			includeUser: true,
			want: map[string]string{
				"BASE":    `C:\UserBase`,
				"USERBIN": `C:\UserBase\u`,
			},
		},
		{
			name: "self_reference_is_preserved_as_unknown_reference",
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "SELF", val: `%SELF%\x`, expand: true},
			),
			raw: windowsRawEnv(
				windowsRawPair{key: "SELF", val: `%SELF%\x`, expand: true},
			),
			includeUser: false,
			want: map[string]string{
				"SELF": `%SELF%\x`,
			},
		},
		{
			name: "unknown_reference_is_preserved",
			machineRaw: windowsRawEnv(
				windowsRawPair{key: "X", val: `%DOES_NOT_EXIST%\x`, expand: true},
			),
			raw: windowsRawEnv(
				windowsRawPair{key: "X", val: `%DOES_NOT_EXIST%\x`, expand: true},
			),
			includeUser: false,
			want: map[string]string{
				"X": `%DOES_NOT_EXIST%\x`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envMap := map[string]envEntry{}
			for k, v := range tc.envMap {
				setEnvEntryValue(envMap, k, v)
			}

			got := expandWindowsRegistryEnv(tc.raw, envMap, tc.machineRaw, tc.userRaw, tc.includeUser)

			for key, want := range tc.want {
				e, ok := got[canonicalEnvKey(key)]
				if !ok {
					t.Fatalf("missing expanded key %q", key)
				}
				if e.val != want {
					t.Fatalf("%s got %q want %q", key, e.val, want)
				}
			}
		})
	}
}

func TestExpandWindowsPercentEnv(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		lookup map[string]string
		want   string
	}{
		{
			name:   "known_reference",
			in:     `%A%\bin`,
			lookup: map[string]string{"A": `C:\A`},
			want:   `C:\A\bin`,
		},
		{
			name:   "multiple_references",
			in:     `%A%;%B%`,
			lookup: map[string]string{"A": `C:\A`, "B": `C:\B`},
			want:   `C:\A;C:\B`,
		},
		{
			name:   "unknown_reference_preserved",
			in:     `%NOPE%\bin`,
			lookup: map[string]string{},
			want:   `%NOPE%\bin`,
		},
		{
			name:   "unclosed_percent_preserved",
			in:     `%A\bin`,
			lookup: map[string]string{"A": `C:\A`},
			want:   `%A\bin`,
		},
		{
			name:   "empty_percent_pair_preserved",
			in:     `a%%b`,
			lookup: map[string]string{},
			want:   `a%%b`,
		},
		{
			name:   "string_without_percent_unchanged",
			in:     `C:\Plain`,
			lookup: map[string]string{"Plain": "ignored"},
			want:   `C:\Plain`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandWindowsPercentEnv(tc.in, func(name string) (string, bool) {
				v, ok := tc.lookup[name]
				return v, ok
			})
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func windowsRawEnv(pairs ...windowsRawPair) map[string]windowsRegistryEnvValue {
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]windowsRegistryEnvValue, len(pairs))
	for _, p := range pairs {
		out[canonicalEnvKey(p.key)] = windowsRegistryEnvValue{
			key:    p.key,
			val:    p.val,
			expand: p.expand,
		}
	}
	return out
}

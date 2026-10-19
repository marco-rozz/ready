package executil

import "testing"

const emptyKey = "   "

func TestMergePathValues(t *testing.T) {
	cases := []struct {
		name            string
		sep             string
		caseInsensitive bool
		values          []string
		want            string
	}{
		{
			name:            "empty_values_skipped",
			sep:             ";",
			caseInsensitive: true,
			values:          []string{"", " ; ; "},
			want:            "",
		},
		{
			name:            "windows_case_insensitive_dedup_preserves_first",
			sep:             ";",
			caseInsensitive: true,
			values: []string{
				`C:\Go\bin;C:\Windows`,
				`c:\go\BIN;C:\Users\me\bin`,
			},
			want: `C:\Go\bin;C:\Windows;C:\Users\me\bin`,
		},
		{
			name:            "unix_case_sensitive_dedup",
			sep:             ":",
			caseInsensitive: false,
			values: []string{
				"/bin:/usr/bin",
				"/BIN:/bin:/opt/bin",
			},
			want: "/bin:/usr/bin:/BIN:/opt/bin",
		},
		{
			name:            "entries_are_trimmed",
			sep:             ":",
			caseInsensitive: false,
			values: []string{
				" /a : : /b ",
				" /a ",
			},
			want: "/a:/b",
		},
		{
			name:            "merge_path_list_compatible_shape",
			sep:             ":",
			caseInsensitive: false,
			values:          []string{"/a:/b", "/c"},
			want:            "/a:/b:/c",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergePathValues(tc.sep, tc.caseInsensitive, tc.values...)
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}

			gotList := mergePathList(tc.sep, tc.caseInsensitive, tc.values)
			if gotList != tc.want {
				t.Fatalf("mergePathList got %q want %q", gotList, tc.want)
			}
		})
	}
}

func TestEnvEntryHelpers(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		val        string
		wantSet    bool
		wantVal    string
		wantKey    string
		wantLookup bool
	}{
		{
			name:       "set_and_get_trimmed_key",
			key:        " TEST_ENV_ENTRY ",
			val:        "1",
			wantSet:    true,
			wantVal:    "1",
			wantKey:    "TEST_ENV_ENTRY",
			wantLookup: true,
		},
		{
			name:       "reject_empty_key",
			key:        emptyKey,
			val:        "1",
			wantSet:    false,
			wantLookup: false,
		},
		{
			name:       "reject_key_with_equals",
			key:        "A=B",
			val:        "1",
			wantSet:    false,
			wantLookup: false,
		},
		{
			name:       "reject_key_with_nul",
			key:        "A\x00",
			val:        "1",
			wantSet:    false,
			wantLookup: false,
		},
		{
			name:       "reject_value_with_nul",
			key:        "A",
			val:        "1\x00",
			wantSet:    false,
			wantLookup: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envMap := map[string]envEntry{}
			gotSet := setEnvEntryValue(envMap, tc.key, tc.val)
			if gotSet != tc.wantSet {
				t.Fatalf("set got %v want %v", gotSet, tc.wantSet)
			}

			gotVal, gotKey, gotOK := getEnvEntryValue(envMap, tc.wantKey)
			if gotOK != tc.wantLookup {
				t.Fatalf("lookup ok got %v want %v", gotOK, tc.wantLookup)
			}
			if tc.wantLookup {
				if gotVal != tc.wantVal {
					t.Fatalf("value got %q want %q", gotVal, tc.wantVal)
				}
				if gotKey != tc.wantKey {
					t.Fatalf("actual key got %q want %q", gotKey, tc.wantKey)
				}
			}
		})
	}
}

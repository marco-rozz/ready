//go:build !windows

package executil

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

const (
	usrBin      = "/usr/bin"
	homebrewBin = "/opt/homebrew/bin"
	usrLocalBin = "/usr/local/bin"
)

func TestDarwinPathHelperOverlay(t *testing.T) {
	cases := []struct {
		name       string
		initialKey string
		initialVal string
		entries    []string
		wantKey    string
		wantPath   string
	}{
		{
			name:       "appends_new_paths_and_dedups_case_sensitive",
			initialKey: defaultPathKey,
			initialVal: "/usr/bin:/bin",
			entries:    []string{usrLocalBin, usrBin, homebrewBin},
			wantKey:    defaultPathKey,
			wantPath:   "/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		},
		{
			name:       "creates_path_when_absent",
			initialKey: "",
			initialVal: "",
			entries:    []string{usrLocalBin, homebrewBin},
			wantKey:    defaultPathKey,
			wantPath:   "/usr/local/bin:/opt/homebrew/bin",
		},
		{
			name:       "empty_entries_leave_existing_path_unchanged",
			initialKey: defaultPathKey,
			initialVal: usrBin,
			entries:    nil,
			wantKey:    defaultPathKey,
			wantPath:   usrBin,
		},
		{
			name:       "blank_entries_are_ignored_by_merge",
			initialKey: defaultPathKey,
			initialVal: usrBin,
			entries:    []string{"", "   ", "/new/bin"},
			wantKey:    defaultPathKey,
			wantPath:   "/usr/bin:/new/bin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envMap := map[string]envEntry{}
			if tc.initialKey != "" {
				setEnvEntryValue(envMap, tc.initialKey, tc.initialVal)
			}

			applyDarwinPathHelperOverlay(envMap, tc.entries)

			gotPath, gotKey, ok := getEnvEntryValue(envMap, defaultPathKey)
			if !ok {
				t.Fatalf("expected PATH to exist")
			}
			if gotKey != tc.wantKey {
				t.Fatalf("PATH key got %q want %q", gotKey, tc.wantKey)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("PATH got %q want %q", gotPath, tc.wantPath)
			}
		})
	}
}

func TestReadDarwinPathHelperEntriesFrom(t *testing.T) {
	td := t.TempDir()
	pathsFile := filepath.Join(td, "paths")
	pathsDir := filepath.Join(td, "paths.d")

	if err := os.Mkdir(pathsDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	mustWriteFile(t, pathsFile, "/usr/bin\n# comment\n\n/bin\x00bad\n/bin\n")
	mustWriteFile(t, filepath.Join(pathsDir, "20-b"), "/b\n")
	mustWriteFile(t, filepath.Join(pathsDir, "10-a"), "# comment\n/a\n\n")
	mustWriteFile(t, filepath.Join(pathsDir, ".hidden"), "/hidden\n")

	if err := os.Mkdir(filepath.Join(pathsDir, "30-dir"), 0o755); err != nil {
		t.Fatalf("Mkdir dir entry: %v", err)
	}

	got := readDarwinPathHelperEntriesFrom(pathsFile, pathsDir)
	want := []string{usrBin, "/bin", "/a", "/b"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestAppendPathFileEntries(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "paths")

	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "ignores_blank_comments_and_nul_lines",
			body: "/a\n\n# comment\n/b\x00bad\n/c\n",
			want: []string{"/a", "/c"},
		},
		{
			name: "trims_entries",
			body: "  /a  \n\t/b\t\n",
			want: []string{"/a", "/b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustWriteFile(t, path, tc.body)

			got := appendPathFileEntries(nil, path)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("entries mismatch:\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestApplyPlatformEnvOverlay_NonDarwinNoop(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("applyPlatformEnvOverlay intentionally reads real macOS path-helper files on darwin")
	}

	envMap := map[string]envEntry{}
	setEnvEntryValue(envMap, defaultPathKey, "/process/bin")

	applyPlatformEnvOverlay(envMap)

	got, _, ok := getEnvEntryValue(envMap, defaultPathKey)
	if !ok {
		t.Fatalf("expected PATH")
	}
	if got != "/process/bin" {
		t.Fatalf("PATH got %q want %q", got, "/process/bin")
	}
}

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

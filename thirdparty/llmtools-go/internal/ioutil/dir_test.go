package ioutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

const (
	dirTestTxtPattern      = "txt"
	dirTestFileAName       = "a" + "." + dirTestTxtPattern
	dirTestFileBName       = "b.log"
	dirTestSubdirName      = "subdir"
	dirTestXFileName       = "x" + "." + dirTestTxtPattern
	dirTestDirName         = "dir"
	dirTestBlankPath       = "   "
	dirTestTxtGlob         = "*." + dirTestTxtPattern
	dirTestInvalidGlob     = "["
	dirTestSyntaxErrSubstr = "syntax error in pattern"

	hardTrashUniquePathAttempts = 12
)

func TestListDirectory(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, dirTestFileBName, 1)
	mustWriteFile(t, root, dirTestFileAName, 1)
	if err := os.Mkdir(filepath.Join(root, dirTestSubdirName), 0o755); err != nil {
		t.Fatalf("failed to mkdir subdir: %v", err)
	}

	tests := []struct {
		name           string
		setup          func(t *testing.T)
		dir            string
		pattern        string
		want           []string
		wantErrIs      error
		wantIsNotExist bool
	}{
		{
			name:    "no pattern returns all entries (already sorted by function)",
			dir:     root,
			pattern: "",
			want:    []string{dirTestFileAName, dirTestFileBName, dirTestSubdirName},
		},
		{
			name:    "pattern filters entries",
			dir:     root,
			pattern: dirTestTxtGlob,
			want:    []string{dirTestFileAName},
		},
		{
			name:      "invalid glob returns filepath.ErrBadPattern",
			dir:       root,
			pattern:   dirTestInvalidGlob,
			wantErrIs: filepath.ErrBadPattern,
		},
		{
			name:           "non-existent directory returns not-exist",
			dir:            filepath.Join(root, "nope"),
			pattern:        "",
			wantIsNotExist: true,
		},
		{
			name:      "invalid path returns ErrInvalidPath",
			dir:       "a\x00b",
			pattern:   "",
			wantErrIs: ErrInvalidPath,
		},
		{
			name: "empty dir means dot (CWD)",
			setup: func(t *testing.T) {
				t.Helper()
				// Ensure deterministic: chdir into our temp dir for this subtest.
				t.Chdir(root)
			},
			dir:     "",
			pattern: "",
			want:    []string{dirTestFileAName, dirTestFileBName, dirTestSubdirName},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			dir := tc.dir
			if dir == "" {
				dir = "."
			}

			dir, err = policy.ResolvePath(dir, "")
			if err != nil {
				if tc.wantErrIs != nil {
					if !errors.Is(err, tc.wantErrIs) {
						t.Fatalf("got normalize err (got=%v)", err)
					}
				} else {
					t.Fatalf("got normalize err (got=%v)", err)
				}
				return
			}
			got, err := listDirectoryNormalized(dir, tc.pattern)

			if tc.wantErrIs != nil || tc.wantIsNotExist {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%v)", got)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
				if tc.wantIsNotExist && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("error=%v; want os.IsNotExist=true", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%#v want=%#v", got, tc.want)
			}
		})
	}
}

func TestListDirectory_DefaultPathDot(t *testing.T) {
	// Not parallel: this test changes the working directory globally.
	tmp := t.TempDir()
	t.Chdir(tmp)

	// Create some entries in the current directory.
	mustWriteFile(t, tmp, dirTestXFileName, 1)
	if err := os.Mkdir(filepath.Join(tmp, dirTestDirName), 0o755); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	policy, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	dir := "."
	dir, err = policy.ResolvePath(dir, "")
	if err != nil {
		t.Fatalf("got normalize err (got=%v)", err)
	}

	got, err := listDirectoryNormalized(dir, "")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// We only check that the created entries are present; there might be others.
	wantSet := map[string]bool{dirTestXFileName: true, dirTestDirName: true}
	for name := range wantSet {
		found := slices.Contains(got, name)
		if !found {
			t.Errorf("expected to find %q in ListDirectory output, got %v", name, got)
		}
	}
}

func TestListDirectory_Additional(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root, dirTestFileAName, 1)
	mustWriteFile(t, root, dirTestFileBName, 1)
	if err := os.Mkdir(filepath.Join(root, dirTestSubdirName), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}

	tests := []struct {
		name          string
		dir           string
		pattern       string
		want          []string
		wantErr       bool
		errContains   string
		errIsNotExist bool
	}{
		{
			name:          "nonexistent dir returns error",
			dir:           filepath.Join(root, "nope"),
			pattern:       "",
			wantErr:       true,
			errIsNotExist: true,
		},
		{
			name:        "invalid glob pattern returns error",
			dir:         root,
			pattern:     dirTestInvalidGlob,
			wantErr:     true,
			errContains: dirTestSyntaxErrSubstr,
		},
		{
			name:    "pattern matches none returns empty slice",
			dir:     root,
			pattern: "*.nope",
			want:    []string{},
		},
		{
			name:    "pattern can match directories too",
			dir:     root,
			pattern: "sub*",
			want:    []string{dirTestSubdirName},
		},
		{
			name:    "basic no pattern includes files and dirs",
			dir:     root,
			pattern: "",
			want:    []string{dirTestFileAName, dirTestFileBName, dirTestSubdirName},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir
			if dir == "" {
				dir = "."
			}
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			dir, err = policy.ResolvePath(dir, "")
			if err != nil {
				t.Fatalf("got normalization error (got=%v)", err)
			}

			got, err := listDirectoryNormalized(dir, tc.pattern)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%v)", got)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				if tc.errIsNotExist && !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("expected not-exist error, got: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			sort.Strings(got)
			sort.Strings(tc.want)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d want=%d got=%v want=%v", len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got[%d]=%q want=%q got=%v", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestListDirectoryDetailedNormalizedCoverage(t *testing.T) {
	root := t.TempDir()

	mustWriteBytes(t, filepath.Join(root, "a.txt"), []byte("a"))
	mustWriteBytes(t, filepath.Join(root, "b.log"), []byte("b"))
	mustWriteBytes(t, filepath.Join(root, ".hidden.txt"), []byte("hidden"))
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	haveLink := false
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link")); err == nil {
		haveLink = true
	}

	wantVisible := []ListDirectoryEntry{
		{Name: "a.txt", Kind: ListDirectoryKindFile},
		{Name: "b.log", Kind: ListDirectoryKindFile},
	}
	if haveLink {
		wantVisible = append(wantVisible, ListDirectoryEntry{Name: "link", Kind: ListDirectoryKindOther})
	}
	wantVisible = append(wantVisible, ListDirectoryEntry{Name: "sub", Kind: ListDirectoryKindDirectory})

	wantAll := append([]ListDirectoryEntry{{Name: ".hidden.txt", Kind: ListDirectoryKindFile}}, wantVisible...)

	tests := []struct {
		name            string
		opts            ListDirectoryOptions
		want            []ListDirectoryEntry
		wantReached     bool
		wantErrIs       error
		wantErrContains string
	}{
		{
			name: "default behavior lists visible entries sorted",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindAll,
				IncludeDotEntries: false,
				MaxEntries:        0,
			},
			want: wantVisible,
		},
		{
			name: "include dot entries keeps hidden files",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindAll,
				IncludeDotEntries: true,
				MaxEntries:        0,
			},
			want: wantAll,
		},
		{
			name: "files only excludes directories and symlinks",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindFile,
				IncludeDotEntries: false,
				MaxEntries:        0,
			},
			want: []ListDirectoryEntry{
				{Name: "a.txt", Kind: ListDirectoryKindFile},
				{Name: "b.log", Kind: ListDirectoryKindFile},
			},
		},
		{
			name: "directories only returns the immediate directory",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindDirectory,
				IncludeDotEntries: true,
				MaxEntries:        0,
			},
			want: []ListDirectoryEntry{{Name: "sub", Kind: ListDirectoryKindDirectory}},
		},
		{
			name: "max entries truncates after sorting",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindAll,
				IncludeDotEntries: false,
				MaxEntries:        2,
			},
			want:        wantVisible[:2],
			wantReached: true,
		},
		{
			name: "invalid kind errors",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKind("bogus"),
				IncludeDotEntries: false,
			},
			wantErrContains: "invalid kind",
		},
		{
			name: "negative max entries errors",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindAll,
				IncludeDotEntries: false,
				MaxEntries:        -1,
			},
			wantErrContains: "maxEntries must be >= 0",
		},
		{
			name: "invalid glob errors",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindAll,
				IncludeDotEntries: false,
				NameGlob:          "[",
			},
			wantErrIs: filepath.ErrBadPattern,
		},
	}

	if haveLink {
		tests = append(tests, struct {
			name            string
			opts            ListDirectoryOptions
			want            []ListDirectoryEntry
			wantReached     bool
			wantErrIs       error
			wantErrContains string
		}{
			name: "other kind returns symlink entries",
			opts: ListDirectoryOptions{
				Kind:              ListDirectoryKindOther,
				IncludeDotEntries: true,
				MaxEntries:        0,
			},
			want: []ListDirectoryEntry{{Name: "link", Kind: ListDirectoryKindOther}},
		})
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reached, err := ListDirectoryDetailedNormalized(root, tc.opts)

			if tc.wantErrIs != nil || tc.wantErrContains != "" {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%#v)", got)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reached != tc.wantReached {
				t.Fatalf("reachedMaxEntries=%v want=%v", reached, tc.wantReached)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%#v want=%#v", got, tc.want)
			}
		})
	}
}

func TestListDirectoryNormalized_SortsAndFiltersAndErrors(t *testing.T) {
	td := t.TempDir()

	// Create entries.
	if err := os.WriteFile(filepath.Join(td, "b.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(td, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(td, "dir1"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	cases := []struct {
		name          string
		dir           string
		pattern       string
		want          []string
		wantErrSubstr string
	}{
		{name: "empty_dir_invalid", dir: dirTestBlankPath, wantErrSubstr: "invalid"},
		{name: "nonexistent_dir_errors", dir: filepath.Join(td, "nope"), wantErrSubstr: "read dir error"},
		{name: "no_pattern_lists_all_sorted", dir: td, pattern: "", want: []string{"a.txt", "b.txt", "dir1"}},
		{name: "pattern_filters", dir: td, pattern: "*.txt", want: []string{"a.txt", "b.txt"}},
		{name: "invalid_glob_errors", dir: td, pattern: dirTestInvalidGlob, wantErrSubstr: "syntax"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listDirectoryNormalized(tc.dir, tc.pattern)
			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error")
				}
				if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantErrSubstr)) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}

	_ = runtime.GOOS
}

func TestUniquePathInDir(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	missingDir := filepath.Join(tmpDir, "missing")
	notADir := filepath.Join(tmpDir, "notadir")
	mustWriteBytes(t, notADir, []byte("x"))

	// Create a collision for the plain base name.
	base := dirTestFileAName
	mustWriteBytes(t, filepath.Join(tmpDir, base), []byte("x"))

	tests := []struct {
		name      string
		dir       string
		base      string
		wantErrIs error
		check     func(t *testing.T, got string)
	}{
		{
			name: "returns_unique_name_when_base_exists",
			dir:  tmpDir,
			base: base,
			check: func(t *testing.T, got string) {
				t.Helper()
				if filepath.Dir(got) != tmpDir {
					t.Fatalf("dir=%q want %q", filepath.Dir(got), tmpDir)
				}
				if got == filepath.Join(tmpDir, base) {
					t.Fatalf("expected a different path than the colliding base; got %q", got)
				}
				if !strings.HasPrefix(filepath.Base(got), "a.") {
					t.Fatalf("expected generated name to start with %q, got %q", "a.", filepath.Base(got))
				}
				if filepath.Ext(got) != ".txt" {
					t.Fatalf("ext=%q want .txt", filepath.Ext(got))
				}
				if _, err := os.Lstat(got); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected generated path to not exist yet; statErr=%v", err)
				}
			},
		},
		{
			name:      "rejects_empty_dir",
			dir:       dirTestBlankPath,
			base:      dirTestXFileName,
			wantErrIs: ErrInvalidPath,
		},
		{
			name:      "rejects_empty_base",
			dir:       tmpDir,
			base:      dirTestBlankPath,
			wantErrIs: ErrInvalidPath,
		},
		{
			name:      "rejects_base_path_traversal",
			dir:       tmpDir,
			base:      "../x.txt",
			wantErrIs: ErrInvalidPath,
		},
		{
			name:      "errors_if_dir_missing",
			dir:       missingDir,
			base:      dirTestXFileName,
			wantErrIs: os.ErrNotExist,
		},
		{
			name: "errors_if_dir_is_file",
			dir:  notADir,
			base: dirTestXFileName,
			check: func(t *testing.T, got string) {
				t.Helper()
				_ = got
			},
			wantErrIs: ErrInvalidDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := UniquePathInDir(tc.dir, tc.base, hardTrashUniquePathAttempts)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%q)", got)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err=%v; want errors.Is(_, %v)=true", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func listDirectoryNormalized(dir, pattern string) ([]string, error) {
	entries, _, err := ListDirectoryDetailedNormalized(dir, ListDirectoryOptions{
		NameGlob:          pattern,
		IncludeDotEntries: true,
		Kind:              ListDirectoryKindAll,
		MaxEntries:        0,
	})
	if err != nil {
		return nil, err
	}

	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.Name
	}
	return out, nil
}

package ioutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/toolutil"
)

const (
	searchTestContentPattern = "CONTENTPATTERN"
	searchTestTxtPattern     = "txt"
	searchTestHello          = "hello"
	searchTestBigPattern     = "BIGPATTERN"
)

// Structure describing the tree used in searchFiles tests.
type searchTestTree struct {
	root        string
	helloPath   string
	matchPath   string
	contentPath string
	nestedPath  string
	bigPath     string
}

func TestSearchFilesBasic(t *testing.T) {
	tree := createSearchTestTree(t)

	tests := []struct {
		name             string
		root             string
		pattern          string
		maxResults       int
		wantExactPaths   []string // compare as set if non-nil
		allowedPaths     []string // each result must be one of these (for limit tests)
		wantLen          int      // -1 means "don't check"; overrides len(wantExactPaths) when set
		wantReachedLimit *bool
		wantErr          bool
		wantErrContains  string
	}{
		{
			name:           "match by filename only",
			root:           tree.root,
			pattern:        "matchname",
			maxResults:     0,
			wantLen:        -1,
			wantExactPaths: []string{tree.matchPath},
		},
		{
			name:           "match by content only",
			root:           tree.root,
			pattern:        searchTestContentPattern,
			maxResults:     0,
			wantLen:        -1,
			wantExactPaths: []string{tree.contentPath, tree.nestedPath},
		},
		{
			name:       "maxResults limits number of matches",
			root:       tree.root,
			pattern:    searchTestContentPattern,
			maxResults: 1,
			allowedPaths: []string{
				tree.contentPath,
				tree.nestedPath,
			},
			wantLen:          1,
			wantReachedLimit: new(true),
		},
		{
			name:            "pattern is required",
			root:            tree.root,
			pattern:         "",
			maxResults:      0,
			wantErr:         true,
			wantErrContains: "pattern is required",
		},
		{
			name:       "invalid regexp pattern",
			root:       tree.root,
			pattern:    "(",
			maxResults: 0,
			wantErr:    true,
		},
		{
			name:       "non-existent root returns error",
			root:       filepath.Join(tree.root, "no_such_dir"),
			pattern:    "anything",
			maxResults: 0,
			wantErr:    true,
		},
		{
			name:             "maxResults zero treated as unlimited",
			root:             tree.root,
			pattern:          searchTestTxtPattern,
			maxResults:       0,
			wantExactPaths:   []string{tree.helloPath, tree.matchPath, tree.contentPath, tree.nestedPath},
			wantLen:          -1,
			wantReachedLimit: new(false),
		},
		{
			name:             "negative maxResults treated as unlimited",
			root:             tree.root,
			pattern:          searchTestTxtPattern,
			maxResults:       -10,
			wantExactPaths:   []string{tree.helloPath, tree.matchPath, tree.contentPath, tree.nestedPath},
			wantLen:          -1,
			wantReachedLimit: new(false),
		},
		{
			name:           "large files are not searched by content",
			root:           tree.root,
			pattern:        searchTestBigPattern,
			maxResults:     0,
			wantExactPaths: []string{},
			wantLen:        -1,
		},

		{
			name:             "maxResults larger than matches => reachedLimit=false",
			root:             tree.root,
			pattern:          searchTestContentPattern,
			maxResults:       10,
			wantExactPaths:   []string{tree.contentPath, tree.nestedPath},
			wantLen:          -1,
			wantReachedLimit: new(false),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			got, reachedLimit, err := searchFiles(t.Context(), policy, tc.root, tc.pattern, tc.maxResults)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantReachedLimit != nil && reachedLimit != *tc.wantReachedLimit {
				t.Fatalf("reachedLimit=%v want=%v", reachedLimit, *tc.wantReachedLimit)
			}

			if tc.wantLen >= 0 && len(got) != tc.wantLen {
				t.Fatalf("len(results) = %d, want %d", len(got), tc.wantLen)
			}

			if tc.wantExactPaths != nil {
				if !equalStringSets(got, tc.wantExactPaths) {
					t.Fatalf("results = %#v, want %#v (order-independent)", got, tc.wantExactPaths)
				}
			}

			if tc.allowedPaths != nil {
				for _, p := range got {
					if !slices.Contains(tc.allowedPaths, p) {
						t.Fatalf("result %q not in allowed set %#v", p, tc.allowedPaths)
					}
				}
			}
		})
	}
}

func TestSearchFilesDetailedCoverage(t *testing.T) {
	tree := createSearchCoverageTree(t)
	policy := mustTestPolicy(t)

	tests := []struct {
		name            string
		opts            SearchFilesOptions
		want            map[string]SearchFileMatchKind
		wantReached     bool
		wantErrContains string
		wantErrIs       error
	}{
		{
			name: "path search only is case-insensitive and globbed",
			opts: SearchFilesOptions{
				Root:              tree.root,
				Query:             "alpha",
				Regexp:            false,
				SearchIn:          SearchFilesSearchInPath,
				MaxResults:        0,
				IncludeDotEntries: false,
				NameGlob:          "*.txt",
				CaseSensitive:     false,
			},
			want: map[string]SearchFileMatchKind{
				tree.alpha: SearchFileMatchKindPath,
			},
		},
		{
			name: "content search includes dot entries when requested",
			opts: SearchFilesOptions{
				Root:              tree.root,
				Query:             "NEEDLE",
				Regexp:            false,
				SearchIn:          SearchFilesSearchInContent,
				MaxResults:        0,
				IncludeDotEntries: true,
				NameGlob:          "*.txt",
				CaseSensitive:     false,
			},
			want: map[string]SearchFileMatchKind{
				tree.both:    SearchFileMatchKindContent,
				tree.content: SearchFileMatchKindContent,
				tree.nested:  SearchFileMatchKindContent,
				tree.hidden:  SearchFileMatchKindContent,
			},
		},
		{
			name: "content search excludes dot entries by default",
			opts: SearchFilesOptions{
				Root:              tree.root,
				Query:             "NEEDLE",
				Regexp:            false,
				SearchIn:          SearchFilesSearchInContent,
				MaxResults:        0,
				IncludeDotEntries: false,
				NameGlob:          "*.txt",
				CaseSensitive:     false,
			},
			want: map[string]SearchFileMatchKind{
				tree.both:    SearchFileMatchKindContent,
				tree.content: SearchFileMatchKindContent,
				tree.nested:  SearchFileMatchKindContent,
			},
		},
		{
			name: "path or content combines match kinds and honors max results",
			opts: SearchFilesOptions{
				Root:              tree.root,
				Query:             "needle",
				Regexp:            false,
				SearchIn:          SearchFilesSearchInPathOrContent,
				MaxResults:        1,
				IncludeDotEntries: false,
				NameGlob:          "*.txt",
				CaseSensitive:     false,
			},
			want: map[string]SearchFileMatchKind{
				tree.both: SearchFileMatchKindPathAndContent,
			},
			wantReached: true,
		},
		{
			name: "invalid searchIn errors",
			opts: SearchFilesOptions{
				Root:     tree.root,
				Query:    "x",
				Regexp:   false,
				SearchIn: SearchFilesSearchIn("bogus"),
			},
			wantErrContains: "invalid searchIn",
		},
		{
			name: "invalid glob errors",
			opts: SearchFilesOptions{
				Root:     tree.root,
				Query:    "x",
				Regexp:   false,
				SearchIn: SearchFilesSearchInContent,
				NameGlob: "[",
			},
			wantErrIs: filepath.ErrBadPattern,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reached, err := SearchFilesDetailed(t.Context(), policy, tc.opts)

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
				t.Fatalf("reachedLimit=%v want=%v", reached, tc.wantReached)
			}
			if gotMap := searchMatchKinds(got); !reflect.DeepEqual(gotMap, tc.want) {
				t.Fatalf("got=%#v want=%#v", gotMap, tc.want)
			}
		})
	}
}

func TestSearchFilesRootDefaultUsesCWD(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "cwdfile"+"."+searchTestTxtPattern)
	writeFile(t, filePath, "some pattern content")

	t.Chdir(root)

	tests := []struct {
		name           string
		pattern        string
		maxResults     int
		wantExactPaths []string
	}{
		{
			name:           "empty root searches current directory by path",
			pattern:        "cwdfile",
			maxResults:     0,
			wantExactPaths: []string{"cwdfile.txt"},
		},
		{
			name:           "empty root searches current directory by content",
			pattern:        "pattern content",
			maxResults:     0,
			wantExactPaths: []string{"cwdfile.txt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			got, _, err := searchFiles(t.Context(), policy, "", tc.pattern, tc.maxResults)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalStringSets(got, tc.wantExactPaths) {
				t.Fatalf("results = %#v, want %#v (order-independent)", got, tc.wantExactPaths)
			}
		})
	}
}

func TestSearchFilesConcurrency(t *testing.T) {
	tree := createSearchTestTree(t)

	tests := []struct {
		name        string
		goroutines  int
		iterations  int
		searchRoot  string
		searchPat   string
		expectedSet []string
	}{
		{
			name:        "concurrent searches on same tree",
			goroutines:  8,
			iterations:  5,
			searchRoot:  tree.root,
			searchPat:   searchTestContentPattern,
			expectedSet: []string{tree.contentPath, tree.nestedPath},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var wg sync.WaitGroup
			errCh := make(chan error, tc.goroutines)
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			for i := 0; i < tc.goroutines; i++ {
				wg.Add(1)
				go func(id int) {
					defer wg.Done()
					for j := 0; j < tc.iterations; j++ {

						got, _, err := searchFiles(t.Context(), policy, tc.searchRoot, tc.searchPat, 0)
						if err != nil {
							errCh <- fmt.Errorf("goroutine %d: unexpected error: %w", id, err)
							return
						}
						if !equalStringSets(got, tc.expectedSet) {
							errCh <- fmt.Errorf("goroutine %d: unexpected results: %#v, want %#v",
								id, got, tc.expectedSet)
							return
						}
					}
				}(i)
			}

			wg.Wait()
			close(errCh)

			for err := range errCh {
				if err != nil {
					t.Fatalf("concurrent searchFiles error: %v", err)
				}
			}
		})
	}
}

func TestSearchFiles_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) (ctx context.Context, root, pattern string, want []string)
		wantErr bool
	}{
		{
			name: "context canceled stops walk",
			setup: func(t *testing.T) (context.Context, string, string, []string) {
				t.Helper()
				root := t.TempDir()
				writeFile(t, filepath.Join(root, "a"+"."+searchTestTxtPattern), searchTestHello)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx, root, searchTestHello, nil
			},
			wantErr: true,
		},
		{
			name: "skips binary and non-utf8 content matches",
			setup: func(t *testing.T) (context.Context, string, string, []string) {
				t.Helper()
				root := t.TempDir()
				mustWriteBytes(
					t,
					filepath.Join(root, "bin.dat"),
					[]byte{0x00, 'B', 'A', 'D', 'P', 'A', 'T', 'T', 'E', 'R', 'N'},
				)
				mustWriteBytes(
					t,
					filepath.Join(root, "badutf8.txt"),
					[]byte{0xff, 0xfe, 'B', 'A', 'D', 'P', 'A', 'T', 'T', 'E', 'R', 'N'},
				)
				return t.Context(), root, "BADPATTERN", []string{}
			},
		},
		{
			name: "path match still works for binary files",
			setup: func(t *testing.T) (context.Context, string, string, []string) {
				t.Helper()
				root := t.TempDir()
				p := filepath.Join(root, "match-by-path.bin")
				mustWriteBytes(t, p, []byte{0x00, 0x01, 0x02})
				return t.Context(), root, "match-by-path", []string{p}
			},
		},
		{
			name: "directory path matches are not returned (files only)",
			setup: func(t *testing.T) (context.Context, string, string, []string) {
				t.Helper()
				root := t.TempDir()
				if err := os.Mkdir(filepath.Join(root, "matchdir"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return t.Context(), root, "matchdir", []string{}
			},
		},
		{
			name: "unreadable file content is ignored",
			setup: func(t *testing.T) (context.Context, string, string, []string) {
				t.Helper()
				if runtime.GOOS == toolutil.GOOSWindows {
					t.Skip("chmod semantics differ on Windows")
				}
				root := t.TempDir()
				p := filepath.Join(root, "secret.txt")
				writeFile(t, p, "SOMEPATTERN")

				if err := os.Chmod(p, 0); err != nil {
					t.Fatalf("chmod: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

				// If the environment still allows reads (root/ACLs), skip to avoid flakes.
				if _, err := os.ReadFile(p); err == nil {
					t.Skip("file remained readable after chmod 0 (likely privileged user / ACLs); skipping")
				}
				return t.Context(), root, "SOMEPATTERN", []string{}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root, pattern, want := tc.setup(t)
			policy, err := fspolicy.New("", nil, true)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			got, _, err := searchFiles(ctx, policy, root, pattern, 0)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !equalStringSets(got, want) {
				t.Fatalf("got=%#v want=%#v (order-independent)", got, want)
			}
		})
	}
}

func TestSearchFiles_AllowedRoots_SkipsSymlinkFileThatResolvesOutside(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == toolutil.GOOSWindows {
		t.Skip("symlink test skipped on Windows")
	}

	root := t.TempDir()
	outside := t.TempDir()

	outsideFile := filepath.Join(outside, "outside.txt")
	mustWriteBytes(t, outsideFile, []byte("OUTSIDE-CONTENT"))

	linkInRoot := filepath.Join(root, "link"+"."+searchTestTxtPattern)
	mustSymlinkOrSkip(t, outsideFile, linkInRoot)

	p, err := fspolicy.New(root, []string{root}, false) // allow symlinks, but restrict roots
	if err != nil {
		t.Fatalf("New policy: %v", err)
	}

	got, _, err := searchFiles(t.Context(), p, root, "link\\."+searchTestTxtPattern, 0)
	if err != nil {
		t.Fatalf("searchFiles error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no results (symlink resolves outside allowed roots), got=%v", got)
	}
}

func TestSearchFiles_RelativeRoot_ReturnsPathsPrefixedWithOriginalRootArg(t *testing.T) {
	// Not parallel: uses t.Chdir.
	td := t.TempDir()
	t.Chdir(td)

	sub := filepath.Join(td, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWriteBytes(t, filepath.Join(sub, "a"+"."+searchTestTxtPattern), []byte(searchTestHello))

	p, err := fspolicy.New("", nil, true)
	if err != nil {
		t.Fatalf("New policy: %v", err)
	}

	got, _, err := searchFiles(t.Context(), p, "sub", "a\\."+searchTestTxtPattern, 0)
	if err != nil {
		t.Fatalf("searchFiles error: %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("sub", "a.txt") {
		t.Fatalf("got=%v; want=%q", got, filepath.Join("sub", "a.txt"))
	}
}

func searchFiles(
	ctx context.Context,
	p fspolicy.FSPolicy,
	root, pattern string,
	maxResults int,
) (matchedFiles []string, reachedLimit bool, err error) {
	if pattern == "" {
		return nil, false, errors.New("pattern is required")
	}

	matches, reachedLimit, err := SearchFilesDetailed(ctx, p, SearchFilesOptions{
		Root:              root,
		Query:             pattern,
		Regexp:            true,
		SearchIn:          SearchFilesSearchInPathOrContent,
		MaxResults:        maxResults,
		IncludeDotEntries: true,
		CaseSensitive:     true,
	})
	if err != nil {
		return nil, reachedLimit, err
	}

	matchedFiles = make([]string, len(matches))
	for i, match := range matches {
		matchedFiles[i] = match.Path
	}
	return matchedFiles, reachedLimit, nil
}

// Build a deterministic directory tree for searchFiles tests.
func createSearchTestTree(t *testing.T) searchTestTree {
	t.Helper()

	root := t.TempDir()

	helloPath := filepath.Join(root, searchTestHello+"."+searchTestTxtPattern)
	writeFile(t, helloPath, searchTestHello+" world")

	matchPath := filepath.Join(root, "matchname"+"."+searchTestTxtPattern)
	writeFile(t, matchPath, "this file path will match; its content will not")

	contentPath := filepath.Join(root, "content"+"."+searchTestTxtPattern)
	writeFile(t, contentPath, "some "+searchTestContentPattern+" in this file.")

	subdir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("failed to create subdir %q: %v", subdir, err)
	}

	nestedPath := filepath.Join(subdir, "nested"+"."+searchTestTxtPattern)
	writeFile(t, nestedPath, "nested "+searchTestContentPattern+" plus more text.")

	// Big file (>10MB) whose content contains BIGPATTERN but should not be read
	// by searchFiles because of the size guard.
	bigPath := filepath.Join(subdir, "bigfile.bin")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("failed to create big file %q: %v", bigPath, err)
	}
	if _, err := f.WriteString(searchTestBigPattern + " at the beginning of a big file."); err != nil {
		t.Fatalf("failed to write to big file %q: %v", bigPath, err)
	}
	if err := f.Truncate(1*1024*1024 + 1); err != nil {
		t.Fatalf("failed to truncate big file %q: %v", bigPath, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close big file %q: %v", bigPath, err)
	}

	return searchTestTree{
		root:        root,
		helloPath:   helloPath,
		matchPath:   matchPath,
		contentPath: contentPath,
		nestedPath:  nestedPath,
		bigPath:     bigPath,
	}
}

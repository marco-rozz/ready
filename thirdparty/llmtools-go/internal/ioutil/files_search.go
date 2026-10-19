package ioutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
)

var errSearchLimitReached = errors.New("search limit reached")

type SearchFilesSearchIn string

const (
	SearchFilesSearchInPath          SearchFilesSearchIn = "path"
	SearchFilesSearchInContent       SearchFilesSearchIn = "content"
	SearchFilesSearchInPathOrContent SearchFilesSearchIn = "pathOrContent"
)

type SearchFileMatchKind string

const (
	SearchFileMatchKindPath           SearchFileMatchKind = "path"
	SearchFileMatchKindContent        SearchFileMatchKind = "content"
	SearchFileMatchKindPathAndContent SearchFileMatchKind = "pathAndContent"
)

type SearchFilesOptions struct {
	Root              string
	Query             string
	Regexp            bool
	SearchIn          SearchFilesSearchIn
	MaxResults        int
	MaxContentBytes   int64
	IncludeDotEntries bool
	NameGlob          string
	CaseSensitive     bool
}

type SearchFileMatch struct {
	Path      string              `json:"path"`
	MatchKind SearchFileMatchKind `json:"matchKind"`
}

// SearchFilesDetailed walks root recursively and returns up to MaxResults file matches.
func SearchFilesDetailed(
	ctx context.Context,
	p fspolicy.FSPolicy,
	opts SearchFilesOptions,
) (matchedFiles []SearchFileMatch, reachedLimit bool, err error) {
	reachedLimit = false

	if opts.Query == "" {
		return nil, reachedLimit, errors.New("query is required")
	}

	searchIn := opts.SearchIn
	if searchIn == "" {
		searchIn = SearchFilesSearchInPathOrContent
	}
	if !isValidSearchFilesSearchIn(searchIn) {
		return nil, reachedLimit, fmt.Errorf("invalid searchIn %q", searchIn)
	}

	if opts.NameGlob != "" {
		if _, err := filepath.Match(opts.NameGlob, "x"); err != nil {
			return nil, reachedLimit, err
		}
	}

	// Still walk an absolute, policy-resolved root for hardening.
	rootArg := opts.Root
	if rootArg == "" {
		rootArg = "."
	}

	rootAbs, err := p.ResolvePath(rootArg, ".")
	if err != nil {
		return nil, reachedLimit, err
	}
	if err := p.VerifyDirResolved(rootAbs); err != nil {
		return nil, reachedLimit, err
	}

	walkRoot := rootAbs
	if !p.BlockSymlinks() {
		if st, lerr := os.Lstat(rootAbs); lerr == nil && (st.Mode()&os.ModeSymlink) != 0 {
			if resolved, rerr := filepath.EvalSymlinks(rootAbs); rerr == nil && resolved != "" {
				walkRoot = filepath.Clean(resolved)
			}
		}
	}
	if p.HasAllowedRoots() {
		if _, rerr := p.ResolvePath(walkRoot, ""); rerr != nil {
			return nil, reachedLimit, rerr
		}
	}

	rootReturn := filepath.Clean(rootArg)
	if rootReturn == "" {
		rootReturn = "."
	}

	matchString, err := newSearchStringMatcher(opts.Query, opts.Regexp, opts.CaseSensitive)
	if err != nil {
		return nil, reachedLimit, err
	}

	limit := opts.MaxResults
	if limit <= 0 {
		limit = int(^uint(0) >> 1)
	}

	matches := make([]SearchFileMatch, 0, min(limit, 64))

	walkFn := func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}

		if len(matches) >= limit {
			reachedLimit = true
			return errSearchLimitReached
		}

		if p.HasAllowedRoots() {
			if _, rerr := p.ResolvePath(path, ""); rerr != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}

		if p.BlockSymlinks() && d != nil && (d.Type()&os.ModeSymlink) != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if path != walkRoot && d != nil && !opts.IncludeDotEntries && isSearchHiddenName(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d == nil || d.IsDir() {
			return nil
		}

		if opts.NameGlob != "" {
			matched, matchErr := filepath.Match(opts.NameGlob, d.Name())
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				return nil
			}
		}

		displayPath := path
		if rel, rerr := filepath.Rel(walkRoot, path); rerr == nil {
			if rootReturn == "." {
				displayPath = rel
			} else {
				displayPath = filepath.Join(rootReturn, rel)
			}
		}

		pathMatched := false
		contentMatched := false

		switch searchIn {
		case SearchFilesSearchInPath:
			pathMatched = matchString(displayPath)
		case SearchFilesSearchInContent:
			contentMatched = searchFileContent(path, matchString, opts.MaxContentBytes)
		case SearchFilesSearchInPathOrContent:
			pathMatched = matchString(displayPath)
			contentMatched = searchFileContent(path, matchString, opts.MaxContentBytes)
		}

		if !pathMatched && !contentMatched {
			return nil
		}

		matches = append(matches, SearchFileMatch{
			Path:      displayPath,
			MatchKind: combineSearchFileMatchKind(pathMatched, contentMatched),
		})

		if len(matches) >= limit {
			reachedLimit = true
			return errSearchLimitReached
		}
		return nil
	}

	err = filepath.WalkDir(walkRoot, walkFn)
	if err != nil && !errors.Is(err, errSearchLimitReached) {
		return nil, reachedLimit, err
	}

	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, reachedLimit, nil
}

func newSearchStringMatcher(query string, useRegexp, caseSensitive bool) (func(string) bool, error) {
	if useRegexp {
		pattern := query
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}

	needle := query
	if !caseSensitive {
		needle = strings.ToLower(needle)
	}
	return func(s string) bool {
		if !caseSensitive {
			s = strings.ToLower(s)
		}
		return strings.Contains(s, needle)
	}, nil
}

func searchFileContent(path string, matchString func(string) bool, maxBytes int64) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() < 0 || (maxBytes > 0 && info.Size() > maxBytes) {
		return false
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	r := io.Reader(f)
	if maxBytes > 0 {
		r = io.LimitReader(f, maxBytes+1)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return false
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return false
	}

	if len(data) == 0 {
		return matchString("")
	}

	sample := data[:min(len(data), 4096)]
	if !isProbablyTextSample(sample) || !utf8.Valid(data) {
		return false
	}
	return matchString(string(data))
}

func isSearchHiddenName(name string) bool {
	return strings.HasPrefix(name, ".") && name != "." && name != ".."
}

func isValidSearchFilesSearchIn(searchIn SearchFilesSearchIn) bool {
	switch searchIn {
	case SearchFilesSearchInPath, SearchFilesSearchInContent, SearchFilesSearchInPathOrContent:
		return true
	default:
		return false
	}
}

func combineSearchFileMatchKind(pathMatched, contentMatched bool) SearchFileMatchKind {
	switch {
	case pathMatched && contentMatched:
		return SearchFileMatchKindPathAndContent
	case pathMatched:
		return SearchFileMatchKindPath
	default:
		return SearchFileMatchKindContent
	}
}

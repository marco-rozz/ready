package ioutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ListDirectoryKind string

const (
	ListDirectoryKindAll       ListDirectoryKind = "all"
	ListDirectoryKindFile      ListDirectoryKind = "file"
	ListDirectoryKindDirectory ListDirectoryKind = "directory"
	ListDirectoryKindOther     ListDirectoryKind = "other"
)

type ListDirectoryOptions struct {
	NameGlob          string
	IncludeDotEntries bool
	Kind              ListDirectoryKind
	MaxEntries        int
}

type ListDirectoryEntry struct {
	Name string            `json:"name"`
	Kind ListDirectoryKind `json:"kind"`
}

// ListDirectoryDetailedNormalized lists and filters immediate directory entries in a directory that is
// assumed to already be normalized.
func ListDirectoryDetailedNormalized(dir string, opts ListDirectoryOptions) ([]ListDirectoryEntry, bool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, false, ErrInvalidPath
	}

	kind := opts.Kind
	if kind == "" {
		kind = ListDirectoryKindAll
	}
	if !isValidListDirectoryKind(kind) {
		return nil, false, fmt.Errorf("invalid kind %q", kind)
	}
	if opts.MaxEntries < 0 {
		return nil, false, errors.New("maxEntries must be >= 0")
	}
	if opts.NameGlob != "" {
		if _, err := filepath.Match(opts.NameGlob, "x"); err != nil {
			return nil, false, err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, fmt.Errorf("read dir error %w", err)
	}

	out := make([]ListDirectoryEntry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !opts.IncludeDotEntries && strings.HasPrefix(name, ".") {
			continue
		}

		if opts.NameGlob != "" {
			matched, matchErr := filepath.Match(opts.NameGlob, name)
			if matchErr != nil {
				return nil, false, matchErr
			}
			if !matched {
				continue
			}
		}

		entryKind := classifyListDirectoryEntryKind(e)
		if kind != ListDirectoryKindAll && entryKind != kind {
			continue
		}

		out = append(out, ListDirectoryEntry{
			Name: name,
			Kind: entryKind,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})

	reachedMaxEntries := false
	if opts.MaxEntries > 0 && len(out) > opts.MaxEntries {
		reachedMaxEntries = true
		out = out[:opts.MaxEntries]
	}

	return out, reachedMaxEntries, nil
}

func classifyListDirectoryEntryKind(e os.DirEntry) ListDirectoryKind {
	switch {
	case e.IsDir():
		return ListDirectoryKindDirectory
	case e.Type().IsRegular():
		return ListDirectoryKindFile
	default:
		return ListDirectoryKindOther
	}
}

func isValidListDirectoryKind(kind ListDirectoryKind) bool {
	switch kind {
	case ListDirectoryKindAll, ListDirectoryKindFile, ListDirectoryKindDirectory, ListDirectoryKindOther:
		return true
	default:
		return false
	}
}

func UniquePathInDir(dir, base string, maxAttempts int) (string, error) {
	dir = strings.TrimSpace(dir)
	base = strings.TrimSpace(base)
	if dir == "" || base == "" {
		return "", ErrInvalidPath
	}
	if strings.ContainsRune(dir, 0) || strings.ContainsRune(base, 0) {
		return "", ErrInvalidPath
	}

	// Ensure dir exists and is a directory.
	if st, err := os.Stat(dir); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("directory: %s, err: %w", dir, ErrInvalidDir)
	}

	// Base must be a single filename, not a path.
	if base == "." || base == ".." || filepath.Base(base) != base || filepath.VolumeName(base) != "" {
		return "", ErrInvalidPath
	}

	// First try the plain name.
	p := filepath.Join(dir, base)
	_, err := os.Lstat(p)

	if err != nil && errors.Is(err, os.ErrNotExist) {
		return p, nil
	} else if err != nil {
		return "", err
	}

	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	// Try a few times; collisions are extremely unlikely with time+random suffix.
	for range maxAttempts {
		sfx, err := randomHex(6) // 12 hex chars
		if err != nil {
			return "", err
		}
		ts := time.Now().UTC().Format("20060102T150405.000000000Z")
		name := fmt.Sprintf("%s.%s.%s%s", stem, ts, sfx, ext)
		candidate := filepath.Join(dir, name)
		if _, err := os.Lstat(candidate); err == nil {
			continue
		} else if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique path for %q", base)
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

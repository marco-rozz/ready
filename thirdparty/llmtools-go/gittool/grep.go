package gittool

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/flexigpt/llmtools-go/spec"
	"github.com/go-git/go-git/v5/plumbing/object"
)

const grepFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/gittool/grep.Grep"

var grepTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019b8358-3d2f-7d35-a391-85b7f5e4c01a",
	Slug:          "gitgrep",
	Version:       spec.VersionOne,
	DisplayName:   "Git grep",
	Description:   "Search UTF-8 tracked file contents at HEAD or another revision using Go regular expressions.",
	Tags:          []string{toolTagGit, toolTagRead},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
	"repoPath": {
		"type": "string",
		"description": "Path to an existing local Git repository."
	},
	"pattern": {
		"type": "string",
		"description": "Search pattern. Interpreted as RE2 regexp unless literal=true."
	},
	"revision": {
		"type": "string",
		"description": "Revision to search.",
		"default": "HEAD"
	},
	"paths": {
		"type": "array",
		"items": {"type": "string"},
		"description": "Optional repository-relative paths to include."
	},
	"literal": {
		"type": "boolean",
		"description": "If true, pattern is treated as a literal string.",
		"default": false
	},
	"ignoreCase": {
		"type": "boolean",
		"description": "If true, perform case-insensitive matching.",
		"default": false
	},
	"contextLines": {
		"type": "integer",
		"description": "Number of surrounding lines to include with each match.",
		"default": 0,
		"minimum": 0,
		"maximum": 5
	},
	"maxMatches": {
		"type": "integer",
		"description": "Maximum matches to return.",
		"default": 100,
		"minimum": 1,
		"maximum": 1000
	},
	"maxFiles": {
		"type": "integer",
		"description": "Maximum files to inspect.",
		"default": 10000,
		"minimum": 1,
		"maximum": 50000
	},
	"maxBytes": {
		"type": "integer",
		"description": "Maximum bytes to read per file.",
		"default": 1048576,
		"minimum": 1024,
		"maximum": 4194304
	}
},
"required": ["repoPath", "pattern"],
"additionalProperties": false
}`),
	GoImpl: spec.GoToolImpl{FuncID: grepFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

type GrepArgs struct {
	RepoPath     string   `json:"repoPath"`
	Pattern      string   `json:"pattern"`
	Revision     string   `json:"revision,omitempty"`
	Paths        []string `json:"paths,omitempty"`
	Literal      bool     `json:"literal,omitempty"`
	IgnoreCase   bool     `json:"ignoreCase,omitempty"`
	ContextLines *int     `json:"contextLines,omitempty"`
	MaxMatches   int      `json:"maxMatches,omitempty"`
	MaxFiles     int      `json:"maxFiles,omitempty"`
	MaxBytes     int      `json:"maxBytes,omitempty"`
}

type GrepMatch struct {
	Path       string   `json:"path"`
	LineNumber int      `json:"lineNumber"`
	Line       string   `json:"line"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

type GrepOut struct {
	RepoPath           string      `json:"repoPath"`
	Revision           string      `json:"revision"`
	Pattern            string      `json:"pattern"`
	Paths              []string    `json:"paths,omitempty"`
	Literal            bool        `json:"literal"`
	IgnoreCase         bool        `json:"ignoreCase"`
	ContextLines       int         `json:"contextLines"`
	MaxMatches         int         `json:"maxMatches"`
	MaxFiles           int         `json:"maxFiles"`
	MaxBytes           int         `json:"maxBytes"`
	Matches            []GrepMatch `json:"matches"`
	Count              int         `json:"count"`
	FilesVisited       int         `json:"filesVisited"`
	Truncated          bool        `json:"truncated"`
	OmittedBinaryFiles int         `json:"omittedBinaryFiles"`
	SkippedLargeFiles  int         `json:"skippedLargeFiles"`
	OutputMeta         OutputMeta  `json:"outputMeta"`
}

func grep(ctx context.Context, snap gitToolSnapshot, args GrepArgs) (*GrepOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pattern := args.Pattern
	if pattern == "" {
		return nil, errors.New("pattern is required")
	}
	if strings.ContainsRune(pattern, '\x00') {
		return nil, errors.New("pattern contains NUL byte")
	}
	if args.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile pattern: %w", err)
	}

	revision := strings.TrimSpace(args.Revision)
	if revision == "" {
		revision = revisionHead
	}
	pathFilters, err := normalizeRepoRelativePaths(args.Paths, true)
	if err != nil {
		return nil, err
	}
	contextLines := normalizeOptionalInt(args.ContextLines, 0, 0, 5)
	maxMatches := normalizePositiveInt(args.MaxMatches, defaultGrepMaxMatches, 1, hardGrepMaxMatches)
	maxFiles := normalizePositiveInt(args.MaxFiles, defaultGrepMaxFiles, 1, hardGrepMaxFiles)
	maxBytes := normalizePositiveInt(args.MaxBytes, defaultDiffMaxBytes, 1024, hardBlobReadBytes)

	repo, abs, err := openRepository(ctx, snap, args.RepoPath)
	if err != nil {
		return nil, err
	}
	commit, err := resolveCommit(repo, revision)
	if err != nil {
		return nil, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	out := &GrepOut{
		RepoPath:     abs,
		Revision:     revision,
		Pattern:      args.Pattern,
		Paths:        pathFilters,
		Literal:      args.Literal,
		IgnoreCase:   args.IgnoreCase,
		ContextLines: contextLines,
		MaxMatches:   maxMatches,
		MaxFiles:     maxFiles,
		MaxBytes:     maxBytes,
	}

	iter := tree.Files()
	defer iter.Close()
	err = iter.ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !matchesRepoRelativePathFilter(f.Name, pathFilters) {
			return nil
		}
		out.FilesVisited++
		if out.FilesVisited > maxFiles {
			out.Truncated = true
			return errStopIteration
		}
		data, truncated, err := readGitObjectFileLimited(f, int64(maxBytes))
		if err != nil {
			return err
		}
		if truncated {
			out.SkippedLargeFiles++
			return nil
		}
		if isBinaryData(data) {
			out.OmittedBinaryFiles++
			return nil
		}
		lines := grepSplitLines(string(data))
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			match := GrepMatch{
				Path:       f.Name,
				LineNumber: i + 1,
				Line:       line,
			}
			if contextLines > 0 {
				beforeStart := max(0, i-contextLines)
				if beforeStart < i {
					match.Before = append([]string(nil), lines[beforeStart:i]...)
				}
				afterEnd := min(len(lines), i+contextLines+1)
				if i+1 < afterEnd {
					match.After = append([]string(nil), lines[i+1:afterEnd]...)
				}
			}
			out.OutputMeta.Bytes += len(line)
			for _, before := range match.Before {
				out.OutputMeta.Bytes += len(before)
			}
			for _, after := range match.After {
				out.OutputMeta.Bytes += len(after)
			}
			out.Matches = append(out.Matches, match)
			if len(out.Matches) >= maxMatches {
				out.Truncated = true
				return errStopIteration
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopIteration) {
		return nil, err
	}

	out.Count = len(out.Matches)
	out.OutputMeta.Truncated = out.Truncated
	out.OutputMeta.OmittedBinaryFiles = out.OmittedBinaryFiles
	out.OutputMeta.SkippedLargeFiles = out.SkippedLargeFiles
	out.OutputMeta.MaxBytes = maxBytes
	return out, nil
}

func readGitObjectFileLimited(f *object.File, maxBytes int64) (data []byte, truncated bool, err error) {
	r, err := f.Reader()
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	return readLimited(r, maxBytes)
}

func grepSplitLines(s string) []string {
	scanner := bufio.NewScanner(strings.NewReader(s))
	scanner.Buffer(make([]byte, 0, 64*1024), hardBlobReadBytes)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

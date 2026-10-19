package texttool

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/flexigpt/llmtools-go/internal/fspolicy"
	"github.com/flexigpt/llmtools-go/internal/ioutil"
	"github.com/flexigpt/llmtools-go/spec"
)

const findTextFuncID spec.FuncID = "github.com/flexigpt/llmtools-go/texttool/findtext.FindText"

var findTextTool = spec.Tool{
	SchemaVersion: spec.SchemaVersion,
	ID:            "019c04d3-fba2-7a49-b1ed-8bdee5055db4",
	Slug:          "findtext",
	Version:       spec.VersionOne,
	DisplayName:   "Find text matches",
	Description: "Find substring or regex matches in a text file and return exact " +
		"line/column ranges with surrounding context. Multi-line search works by including newlines in query",
	Tags: []string{toolTagText},

	ArgSchema: spec.JSONSchema(`{
"$schema": "http://json-schema.org/draft-07/schema#",
"type": "object",
"properties": {
  "path": {
    "type": "string",
    "description": "Path of the file"
  },
  "queryType": {
    "type": "string",
    "enum": ["substring", "regex"],
    "default": "substring"
  },
  "query": {
    "type": "string",
    "minLength": 1,
    "description": "Text or regex to find. May include newline characters for multi-line search. For substring search, TrimSpace is used"
  },
  "contextLines": {
    "type": "integer",
    "minimum": 1,
    "default": 1,
    "description": "Number of lines to include before and after each match. Returned context always includes the matched lines themselves."
  },
  "maxMatches": {
    "type": "integer",
    "minimum": 1,
    "default": 10,
    "description": "Maximum number of non-overlapping matches to return."
  }
},
"required": ["path", "query"],
"additionalProperties": false
}`),

	GoImpl: spec.GoToolImpl{FuncID: findTextFuncID},

	CreatedAt:  spec.SchemaStartTime,
	ModifiedAt: spec.SchemaStartTime,
}

const (
	findTypeSubstring = "substring"
	findTypeRegex     = "regex"
)

type FindTextArgs struct {
	Path string `json:"path"`

	QueryType string `json:"queryType,omitempty"` // substring (default) | regex
	Query     string `json:"query"`               // required, may contain newlines

	ContextLines int `json:"contextLines,omitempty"` // default 1, effective min 1
	MaxMatches   int `json:"maxMatches,omitempty"`   // default 10
}

type FindTextLine struct {
	LineNumber int    `json:"lineNumber"` // 1-based
	Text       string `json:"text"`       // original line, without trailing newline
}

type FindTextMatch struct {
	MatchStartLine   int `json:"matchStartLine"`   // 1-based
	MatchStartColumn int `json:"matchStartColumn"` // 1-based UTF-8 rune column, inclusive

	MatchEndLine   int `json:"matchEndLine"`   // 1-based
	MatchEndColumn int `json:"matchEndColumn"` // 1-based UTF-8 rune column, exclusive

	Context []FindTextLine `json:"context,omitempty"` // includes matched lines as well
}

type FindTextOut struct {
	ReachedMaxMatches        bool            `json:"reachedMaxMatches"`
	AdditionalMatchesOmitted int             `json:"additionalMatchesOmitted,omitempty"`
	MatchesReturned          int             `json:"matchesReturned"`
	Matches                  []FindTextMatch `json:"matches"`
}

// findText finds literal substring or regex occurrences and returns context.
//
// Behavior notes:
//   - File must exist, be regular, not a symlink, and valid UTF-8.
//   - Search runs over the whole file text after normalizing line endings to "\n".
//   - Substring mode trims leading/trailing whitespace from query before matching.
//   - Regex mode preserves the pattern as provided except actual CRLF/CR line endings
//     in the query are normalized to "\n" before compilation.
//   - Matches are returned in ascending file order and are non-overlapping.
//   - Columns are 1-based UTF-8 rune columns; matchEndColumn is exclusive.
func findText(ctx context.Context, args FindTextArgs, p fspolicy.FSPolicy) (*FindTextOut, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, errors.New("path is required")
	}

	qtype := strings.ToLower(strings.TrimSpace(args.QueryType))
	if qtype == "" {
		qtype = findTypeSubstring
	}
	switch qtype {
	case findTypeSubstring, findTypeRegex:
	default:
		return nil, fmt.Errorf(`invalid queryType %q (expected "substring" or "regex")`, args.QueryType)
	}

	contextLines := max(args.ContextLines, 1)
	if contextLines > hardFindTextContextLines {
		return nil, fmt.Errorf("contextLines too large: %d", contextLines)
	}

	maxMatches := args.MaxMatches
	if maxMatches <= 0 {
		maxMatches = 10
	}
	if maxMatches > hardFindTextMaxMatches {
		return nil, fmt.Errorf("maxMatches too large: %d", maxMatches)
	}

	normalizedQuery := normalizeFindTextQuery(args.Query)
	if strings.TrimSpace(normalizedQuery) == "" {
		if qtype == findTypeRegex {
			return nil, errors.New("query is required for queryType=regex")
		}
		return nil, errors.New("query is required for queryType=substring")
	}

	searchQuery := normalizedQuery

	var re *regexp.Regexp
	if qtype == findTypeSubstring {
		searchQuery = strings.TrimSpace(normalizedQuery)
		if searchQuery == "" {
			return nil, errors.New("query is required for queryType=substring")
		}
	} else {
		var err error
		re, err = regexp.Compile(searchQuery)
		if err != nil {
			return nil, err
		}
	}

	tf, err := ioutil.ReadTextFileUTF8(p, args.Path, hardTextProcessingBytes)
	if err != nil {
		return nil, err
	}

	totalLines := len(tf.Lines)
	if totalLines == 0 {
		return &FindTextOut{
			ReachedMaxMatches: false,
			MatchesReturned:   0,
			Matches:           nil,
		}, nil
	}

	normalizedText := buildFindTextNormalizedText(tf.Lines)
	lineStarts := buildFindTextLineStartOffsets(tf.Lines)

	out := &FindTextOut{
		Matches: make([]FindTextMatch, 0, min(maxMatches, 16)),
	}

	totalReturnedLines := 0
	addMatch := func(startByte, endByte int) error {
		if startByte < 0 || endByte <= startByte || endByte > len(normalizedText) {
			return fmt.Errorf("internal error: invalid match range %d..%d", startByte, endByte)
		}

		startLineIdx, startCol, err := findTextOffsetToLineColumn(tf.Lines, lineStarts, startByte)
		if err != nil {
			return err
		}
		endLineIdx, endCol, err := findTextOffsetToLineColumn(tf.Lines, lineStarts, endByte)
		if err != nil {
			return err
		}

		ctxStart := max(0, startLineIdx-contextLines)
		ctxEnd := min(totalLines-1, endLineIdx+contextLines)

		nCtx := ctxEnd - ctxStart + 1
		totalReturnedLines += nCtx
		if totalReturnedLines > hardFindTextTotalReturnedLines {
			return fmt.Errorf(
				"response too large (context window lines exceed %d). Reduce contextLines or maxMatches",
				hardFindTextTotalReturnedLines,
			)
		}

		context := make([]FindTextLine, 0, nCtx)
		for i := ctxStart; i <= ctxEnd; i++ {
			context = append(context, FindTextLine{
				LineNumber: i + 1,
				Text:       tf.Lines[i],
			})
		}

		out.Matches = append(out.Matches, FindTextMatch{
			MatchStartLine:   startLineIdx + 1,
			MatchStartColumn: startCol,
			MatchEndLine:     endLineIdx + 1,
			MatchEndColumn:   endCol,
			Context:          context,
		})
		return nil
	}

	switch qtype {
	case findTypeSubstring:
		for searchStart := 0; searchStart <= len(normalizedText); {
			if err := ctx.Err(); err != nil {
				return nil, err
			}

			idx := strings.Index(normalizedText[searchStart:], searchQuery)
			if idx < 0 {
				break
			}

			startByte := searchStart + idx
			endByte := startByte + len(searchQuery)

			if len(out.Matches) < maxMatches {
				if err := addMatch(startByte, endByte); err != nil {
					return nil, err
				}
			} else {
				out.AdditionalMatchesOmitted++
			}

			searchStart = endByte
		}

	case findTypeRegex:

		nonEmptyMatches := 0
		zeroLengthMatches := 0

		for searchStart := 0; searchStart <= len(normalizedText); {

			if err := ctx.Err(); err != nil {
				return nil, err
			}
			loc := re.FindStringIndex(normalizedText[searchStart:])
			if loc == nil {
				break
			}
			if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(normalizedText) {
				return nil, fmt.Errorf("internal error: invalid regex match range %v", loc)
			}
			startByte := searchStart + loc[0]
			endByte := searchStart + loc[1]
			if startByte == endByte {
				zeroLengthMatches++
				if startByte >= len(normalizedText) {
					break
				}
				_, width := utf8.DecodeRuneInString(normalizedText[startByte:])
				if width <= 0 {
					width = 1
				}
				searchStart = startByte + width
				continue
			}

			nonEmptyMatches++
			if len(out.Matches) < maxMatches {
				if err := addMatch(startByte, endByte); err != nil {
					return nil, err
				}
			} else {
				out.AdditionalMatchesOmitted++
			}
			searchStart = endByte
		}

		if nonEmptyMatches == 0 && zeroLengthMatches > 0 {
			return nil, errors.New("regex matched only empty strings; use a pattern that matches actual text")
		}
	}

	out.ReachedMaxMatches = out.AdditionalMatchesOmitted > 0
	out.MatchesReturned = len(out.Matches)
	return out, nil
}

func normalizeFindTextQuery(in string) string {
	in = strings.ReplaceAll(in, "\r\n", "\n")
	in = strings.ReplaceAll(in, "\r", "\n")
	return in
}

func buildFindTextNormalizedText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func buildFindTextLineStartOffsets(lines []string) []int {
	if len(lines) == 0 {
		return nil
	}

	starts := make([]int, len(lines))
	pos := 0
	for i, line := range lines {
		starts[i] = pos
		pos += len(line)
		if i < len(lines)-1 {
			pos++
		}
	}
	return starts
}

func findTextOffsetToLineColumn(lines []string, lineStarts []int, offset int) (lineIdx, col int, err error) {
	if len(lines) == 0 || len(lineStarts) == 0 {
		return 0, 0, errors.New("internal error: no lines available for offset mapping")
	}
	if offset < 0 {
		return 0, 0, fmt.Errorf("internal error: invalid offset %d", offset)
	}

	lastLine := len(lines) - 1
	maxOffset := lineStarts[lastLine] + len(lines[lastLine])
	if offset > maxOffset {
		return 0, 0, fmt.Errorf("internal error: invalid offset %d", offset)
	}

	lineIdx = max(sort.Search(len(lineStarts), func(i int) bool {
		return lineStarts[i] > offset
	})-1, 0)

	rel := offset - lineStarts[lineIdx]
	if rel < 0 || rel > len(lines[lineIdx]) {
		return 0, 0, fmt.Errorf("internal error: offset %d does not map cleanly to line %d", offset, lineIdx+1)
	}

	col = utf8.RuneCountInString(lines[lineIdx][:rel]) + 1
	return lineIdx, col, nil
}

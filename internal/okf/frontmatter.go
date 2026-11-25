package okf

import (
	"errors"
	"io"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// FMState classifies the frontmatter of a file.
type FMState int

// Frontmatter states.
const (
	// FMAbsent: the first line is not exactly "---".
	FMAbsent FMState = iota
	// FMOK: a readable mapping (possibly empty).
	FMOK
	// FMDuplicate: a key is repeated; Meta still holds the last-wins view.
	FMDuplicate
	// FMInvalid: the block does not parse as YAML.
	FMInvalid
	// FMNotMapping: the block parses but is not a mapping.
	FMNotMapping
	// FMUnclosed: no closing delimiter.
	FMUnclosed
)

// Frontmatter is the result of splitting and parsing one file.
type Frontmatter struct {
	State FMState
	// Meta is the decoded mapping (core-schema scalars, timestamps kept as
	// strings). For FMDuplicate it is the last-wins view, as the reference
	// scanner reads it.
	Meta map[string]any
	// Body is the text after the closing delimiter.
	Body string
	// BodyLine is the 1-based file line of the first body line.
	BodyLine int
	// DupKey and DupLine describe the first repeated key (FMDuplicate); the
	// line is the file line of the repeated key.
	DupKey  string
	DupLine int
	// Problem is a short human reason for FMInvalid/FMNotMapping/FMUnclosed.
	Problem string
}

// fmDelimiter is the exact frontmatter delimiter line.
const fmDelimiter = "---"

const bom = "\xef\xbb\xbf"

// splitLines splits like Python's str.splitlines for the separators that
// matter in Markdown files: \n, \r\n and \r.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			lines = append(lines, text[start:i])
			start = i + 1
		case '\r':
			lines = append(lines, text[start:i])
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}

	return lines
}

// startsWithFrontmatter reports whether the first line is exactly "---"
// (a trailing carriage return tolerated).
func startsWithFrontmatter(text string) bool {
	lines := splitLines(text)

	return len(lines) > 0 && strings.TrimRight(lines[0], "\r") == fmDelimiter
}

// ParseFrontmatter splits and parses the frontmatter of text (a leading
// byte-order mark is removed first).
func ParseFrontmatter(text string) Frontmatter {
	text = strings.TrimLeft(text, bom)
	lines := splitLines(text)
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != fmDelimiter {
		return Frontmatter{State: FMAbsent, Meta: map[string]any{}, Body: text, BodyLine: 1}
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") != fmDelimiter {
			continue
		}
		block := strings.Join(lines[1:i], "\n")
		fm := Frontmatter{
			Meta:     map[string]any{},
			Body:     strings.Join(lines[i+1:], "\n"),
			BodyLine: i + 2,
		}
		decodeBlock(&fm, block)

		return fm
	}

	return Frontmatter{
		State: FMUnclosed, Meta: map[string]any{}, BodyLine: len(lines) + 1,
		Problem: "the closing --- line is missing",
	}
}

// decodeBlock parses the YAML block into fm following the reference order:
// parse errors first, then duplicate keys, then the mapping check.
func decodeBlock(fm *Frontmatter, block string) {
	dec := yaml.NewDecoder(strings.NewReader(block))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) { // empty or comment-only block
			fm.State = FMOK

			return
		}
		fm.State, fm.Problem = FMInvalid, "invalid YAML ("+firstLine(err.Error())+")"

		return
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		fm.State, fm.Problem = FMInvalid, "invalid YAML (expected a single document)"

		return
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			fm.State = FMOK

			return
		}
		root = root.Content[0]
	}

	key, line, dup := findDuplicateKey(root)
	value, err := nodeValue(root)
	if err != nil {
		fm.State, fm.Problem = FMInvalid, "invalid YAML ("+err.Error()+")"

		return
	}
	switch m := value.(type) {
	case nil:
		fm.State = FMOK
	case map[string]any:
		fm.Meta = m
		fm.State = FMOK
	default:
		_ = m
		fm.State, fm.Problem = FMNotMapping, "frontmatter is not a YAML mapping"
	}
	if dup {
		// Reference order: a duplicate is reported even when the block is
		// not a mapping, and wins over the mapping check.
		fm.State, fm.DupKey, fm.DupLine = FMDuplicate, key, line+1
		fm.Problem = ""
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}

// findDuplicateKey walks every mapping (nested included) breadth-first, like
// the reference constructor, and returns the first repeated key with its
// 1-based line inside the block.
func findDuplicateKey(root *yaml.Node) (string, int, bool) {
	queue := []*yaml.Node{root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for n.Kind == yaml.AliasNode && n.Alias != nil {
			n = n.Alias
		}
		switch n.Kind {
		case yaml.MappingNode:
			seen := map[string]bool{}
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				id := k.ShortTag() + ":" + k.Value
				if seen[id] {
					return k.Value, k.Line, true
				}
				seen[id] = true
			}
			for i := 1; i < len(n.Content); i += 2 {
				queue = append(queue, n.Content[i])
			}
		case yaml.SequenceNode:
			queue = append(queue, n.Content...)
		}
	}

	return "", 0, false
}

// nodeValue converts a node using the YAML 1.2 core schema. Timestamp-like
// scalars stay strings; mapping keys are stringified; later duplicate keys
// win.
func nodeValue(n *yaml.Node) (any, error) {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, errors.New("found an unhashable mapping key")
			}
			v, err := nodeValue(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}

		return m, nil
	case yaml.SequenceNode:
		l := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeValue(c)
			if err != nil {
				return nil, err
			}
			l = append(l, v)
		}

		return l, nil
	case yaml.ScalarNode:
		return scalarValue(n), nil
	}

	return nil, nil //nolint:nilnil // an empty document node is a null value
}

func scalarValue(n *yaml.Node) any {
	switch n.ShortTag() {
	case "!!null":
		return nil
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return b
		}
	case "!!int":
		var i int64
		if n.Decode(&i) == nil {
			return i
		}
	case "!!float":
		var f float64
		if n.Decode(&f) == nil && !math.IsNaN(f) {
			return f
		}
	}

	return n.Value
}

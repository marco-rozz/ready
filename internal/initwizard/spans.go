// Minimal byte-span editing of authored YAML for the bounded offline
// repair (design D5): only the affected authored spans may change, so a
// repair replaces the documented scalar's exact bytes in the raw document —
// never a re-marshal of the whole file. Block styles, anchors, aliases and
// unsupported constructs fail closed (the operator edits those by hand or
// re-runs serve's migration).
package initwizard

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// errUnsupportedSpan marks a document shape repair refuses rather than
// approximates.
var errUnsupportedSpan = errors.New("unsupported YAML construct for a span-repair")

// locateValueNode walks the document tree to the scalar node at path.
func locateValueNode(data []byte, path []string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: document does not parse", errUnsupportedSpan)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	node := root
	for _, seg := range path {
		switch node.Kind {
		case yaml.MappingNode:
			found := false
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == seg {
					node = node.Content[i+1]
					found = true

					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: %v is absent from the document", errUnsupportedSpan, path)
			}
		case yaml.SequenceNode:
			idx, iok := seqIndex(seg)
			if !iok || idx >= len(node.Content) {
				return nil, fmt.Errorf("%w: %v has no such entry", errUnsupportedSpan, path)
			}
			node = node.Content[idx]
		default:
			return nil, fmt.Errorf("%w: %v does not name a nested location", errUnsupportedSpan, path)
		}
	}
	if node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("%w: %v is not a scalar", errUnsupportedSpan, path)
	}

	return node, nil
}

// seqIndex parses a sequence-index path segment (a bare integer). A plain
// "0"-looking mapping key in a mapping path is unambiguous because the
// caller switches on the node kind.
func seqIndex(seg string) (int, bool) {
	if seg == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return 0, false
		}
		n = n*10 + int(seg[i]-'0')
	}

	return n, true
}

// replaceScalarSpan rewrites the value bytes of one scalar node inside the
// raw document. The replacement is written with the original scalar style
// where it can carry the value exactly (plain one-line scalars write as
// plain; single- and double-quoted write re-quoted); anything else — block
// styles, multi-line plain folds, mixtures with inline comments — is
// refused: a repair never approximates.
func replaceScalarSpan(data []byte, path []string, value string) ([]byte, error) {
	node, err := locateValueNode(data, path)
	if err != nil {
		return nil, err
	}

	span, err := scalarValueSpan(node, data)
	if err != nil {
		return nil, err
	}
	start, end := span[0], span[1]

	switch node.Style {
	case 0, yaml.TaggedStyle: // plain
		if bytesContains(data[start:end], "\n") {
			return nil, fmt.Errorf("%w: plain multi-line scalar at %s; edit the document manually", errUnsupportedSpan, strings.Join(path, "."))
		}
		replacement, rerr := plainScalarFor(value)
		if rerr != nil {
			return nil, rerr
		}

		return replaceBytes(data, start, end, replacement), nil
	case yaml.SingleQuotedStyle:
		return replaceBytes(data, start, end, []byte("'"+strings.ReplaceAll(value, "'", "''")+"'")), nil
	case yaml.DoubleQuotedStyle:
		enc, derr := yamlEncodeDoubleQuoted(value)
		if derr != nil {
			return nil, derr
		}

		return replaceBytes(data, start, end, enc), nil
	default:
		return nil, fmt.Errorf("%w: block/unsupported scalar style at %s; edit the document manually", errUnsupportedSpan, strings.Join(path, "."))
	}
}

// scalarValueSpan computes the [start,end) byte offsets of the value
// content of one scalar node: the node's own (line,column) to the end of
// the scalar's on-line content (plain/quoted only — callers refuse
// everything else).
func scalarValueSpan(node *yaml.Node, data []byte) ([2]int, error) {
	line := node.Line - 1
	if line < 0 || line >= lines(data) {
		return [2]int{}, fmt.Errorf("%w: scalar outside document bounds", errUnsupportedSpan)
	}
	ls := lineStart(data, line)
	le := lineEnd(data, line)
	if le < ls {
		return [2]int{}, fmt.Errorf("%w: broken line layout", errUnsupportedSpan)
	}

	// Column is 1-based and rune-based; refuse lines where a byte-indexed
	// column would not line up (multi-byte keys requiring rune walking are
	// unsupported here — a fail-closed refusal, never an approximation).
	byteCol := node.Column - 1
	if byteCol > le-ls {
		return [2]int{}, fmt.Errorf("%w: scalar column beyond line length", errUnsupportedSpan)
	}
	start := ls + byteCol
	// Trim any trailing whitespace of the logical line; the span ends at
	// the last non-space byte.
	end := le
	for end > start && (data[end-1] == ' ' || data[end-1] == '\t' || data[end-1] == '\r') {
		end--
	}

	return [2]int{start, end}, nil
}

func lines(data []byte) int { return strings.Count(string(data), "\n") + 1 }

func lineStart(data []byte, line int) int {
	start := 0
	for i := 0; i < line; i++ {
		next := indexByte(data[start:], '\n')
		if next < 0 {
			return len(data)
		}
		start += next + 1
	}

	return start
}

func lineEnd(data []byte, line int) int {
	start := lineStart(data, line)
	next := indexByte(data[start:], '\n')
	if next < 0 {
		return len(data)
	}

	return start + next
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}

	return -1
}

func bytesContains(b []byte, sub string) bool { return strings.Contains(string(b), sub) }

func replaceBytes(data []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(data)-(end-start)+len(replacement))
	out = append(out, data[:start]...)
	out = append(out, replacement...)
	out = append(out, data[end:]...)

	return out
}

// plainScalarFor writes value as a safe plain YAML scalar, refusing values
// that would need quoting (the caller re-quotes instead). Internal ':' is
// legal in plain scalars (URL paths, model refs); ': ' and a trailing ':'
// are not.
func plainScalarFor(value string) ([]byte, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\n") || strings.Contains(value, ": ") || strings.HasSuffix(value, ":") ||
		strings.Contains(value, "\t") || strings.Contains(value, " #") || strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") ||
		strings.HasPrefix(value, "*") || strings.HasPrefix(value, "&") || strings.HasPrefix(value, "!") ||
		strings.HasPrefix(value, "%") || strings.HasPrefix(value, "@") || strings.HasPrefix(value, "`") ||
		strings.HasPrefix(value, "#") || strings.HasPrefix(value, "- ") || strings.Contains(value, " #") ||
		strings.HasPrefix(value, "?") || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "]") ||
		strings.HasPrefix(value, "{") || strings.HasPrefix(value, "}") || strings.HasPrefix(value, ",") ||
		strings.HasPrefix(value, "'") || strings.HasPrefix(value, "\"") || strings.Contains(value, "\t") {
		return nil, fmt.Errorf("%w: value needs quoting; plainly unsupported in repair", errUnsupportedSpan)
	}

	return []byte(value), nil
}

// yamlEncodeDoubleQuoted re-encodes value exactly with the libyaml emitter
// (double-quote style pinned) so round-trips keep the literal string.
func yamlEncodeDoubleQuoted(value string) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&stringWriter{w: &buf})
	if err := enc.Encode(doubleQuotedScalar(value)); err != nil {
		return nil, fmt.Errorf("%w: cannot re-encode a double-quoted scalar safely", errUnsupportedSpan)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("%w: cannot close the scalar encoder safely", errUnsupportedSpan)
	}

	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}

// doubleQuotedScalar is a styled scalar node for exact quoting.
func doubleQuotedScalar(value string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Style: yaml.DoubleQuotedStyle,
		Value: value,
	}
}

// removeMappingLeaf deletes the whole `key: value` line covering the final
// path segment. Block-style values are refused (fail closed).
func removeMappingLeaf(data []byte, path []string) ([]byte, error) {
	// Walk to the leaf's value node and capture the parent for the key line.
	value, err := locateValueNode(data, path)
	if err != nil {
		return nil, err
	}
	if value.Style != 0 && value.Style != yaml.TaggedStyle && value.Style != yaml.SingleQuotedStyle && value.Style != yaml.DoubleQuotedStyle {
		return nil, fmt.Errorf("%w: block-style value at %s; edit the document manually", errUnsupportedSpan, strings.Join(path, "."))
	}

	// The `key:` and an inline value share the line for every accepted style
	// (plains and quotes are single-line scalars by construction here).
	line := value.Line - 1
	if line < 0 || line >= lines(data) {
		return nil, fmt.Errorf("%w: key outside document bounds", errUnsupportedSpan)
	}
	start := lineStart(data, line)
	end := lineEnd(data, line)
	if end < len(data) {
		end++ // consume the newline too
	}
	if end > len(data) {
		end = len(data)
	}

	return replaceBytes(data, start, end, nil), nil
}

// insertKeyAfter inserts `newKey: value` as a line with the indentation of
// siblingKey, directly after it, inside the mapping at parentPath.
func insertKeyAfter(data []byte, parentPath []string, siblingKey, newKey, value string) ([]byte, error) {
	parent, err := locateMappingNode(data, parentPath)
	if err != nil {
		return nil, err
	}
	sibLine := siblingLine(data, parent, siblingKey) // 1-based
	if sibLine < 1 {
		return nil, fmt.Errorf("%w: sibling %q not found in %v", errUnsupportedSpan, siblingKey, parentPath)
	}
	sib := sibLine - 1 // the 0-based line all helpers in this file speak

	var sb strings.Builder
	indent := lineIndent(data, sib)
	sb.WriteString(indent + newKey + ": " + value + "\n")

	insertAt := lineEnd(data, sib)
	if insertAt < len(data) {
		insertAt++ // after the newline
	}

	out := make([]byte, 0, len(data)+sb.Len())
	out = append(out, data[:insertAt]...)
	out = append(out, []byte(sb.String())...)
	out = append(out, data[insertAt:]...)

	return out, nil
}

// locateMappingNode walks to the mapping node at path.
func locateMappingNode(data []byte, path []string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: document does not parse", errUnsupportedSpan)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	node := root
	for _, seg := range path {
		if node.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%w: %v is not a mapping path", errUnsupportedSpan, path)
		}
		found := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == seg {
				node = node.Content[i+1]
				found = true

				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: %v is absent from the document", errUnsupportedSpan, path)
		}
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: %v is not a mapping", errUnsupportedSpan, path)
	}

	return node, nil
}

// locateValueNode walks the document tree

// siblingLine returns the line number (1-based) of a key within a mapping.
func siblingLine(_ []byte, parent *yaml.Node, key string) int {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i].Line
		}
	}

	return -1
}

// lineIndent renders the leading whitespace of one 0-based line.
func lineIndent(data []byte, line int) string {
	start := lineStart(data, line)
	n := 0
	for start+n < len(data) && (data[start+n] == ' ' || data[start+n] == '\t') {
		n++
	}

	return string(data[start : start+n])
}

type stringWriter struct{ w *strings.Builder }

func (s *stringWriter) Write(p []byte) (int, error) { return s.w.Write(p) }

// setReference writes a Secret reference (`!secret name`) at path: it replaces
// an existing scalar value in place or inserts the key after siblingFallback.
// The reference is written raw — a tag must never be quoted.
func setReference(data []byte, path []string, ref, siblingFallback string) ([]byte, error) {
	if node, err := locateValueNode(data, path); err == nil {
		span, serr := scalarValueSpan(node, data)
		if serr != nil {
			return nil, serr
		}

		return replaceBytes(data, span[0], span[1], []byte(ref)), nil
	}

	return insertKeyAfter(data, path[:len(path)-1], siblingFallback, path[len(path)-1], ref)
}

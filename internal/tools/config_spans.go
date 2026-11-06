package tools

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/marco-rozz/ready/internal/config"

	"gopkg.in/yaml.v3"
)

// yamlSpanSupport is the tool-only supported YAML subset (design D1):
// block-context plain, single-quoted, double-quoted, literal and folded
// scalars over LF/CRLF text, Unicode coordinates. Anchors, aliases, merge
// keys, flow containers, multi-line plain scalars and explicit tags on
// sensitive values are refused instead of approximated. This restriction
// belongs to the config-manager tool; ordinary config loading is unaffected.

// stagedDoc is the analysis of one staged text snapshot: the parsed
// document and, for every classified sensitive scalar, its exact byte span.
type stagedDoc struct {
	version int
	spans   []configSpan
}

// configSpan locates one classified sensitive scalar inside raw text.
type configSpan struct {
	// path is the exact YAML path of the classified field.
	path []string
	// style is the scalar's lexical style (0 = plain).
	style yaml.Style
	// start/end delimit the scalar token's byte range [start,end) —
	// including its quotes or block header+content, excluding surrounding
	// spacing and syntax.
	start int
	end   int
	// value is the decoded scalar value (never logged).
	value string
	// multiline marks a literal/folded block or a value containing a newline.
	multiline bool
}

// lineOffsets returns the byte offset of each 1-based line start.
func lineOffsets(data []byte) []int {
	offs := make([]int, 1, 1+strings.Count(string(data), "\n"))
	offs[0] = 0
	for i, b := range data {
		if b == '\n' {
			offs = append(offs, i+1)
		}
	}

	return offs
}

// spanOf maps a node's Line/Column to a byte offset start.
func spanOf(data []byte, offs []int, line, col int) (start int, err error) {
	if line < 1 || line > len(offs) {
		return 0, fmt.Errorf("%w: scalar position outside the document (line %d)", ErrConfigUnsupportedYAML, line)
	}
	start = offs[line-1]
	for column := 1; column < col && start < len(data); column++ {
		_, width := utf8.DecodeRune(data[start:])
		start += width
	}
	if start > len(data) {
		return 0, fmt.Errorf("%w: scalar position outside the document", ErrConfigUnsupportedYAML)
	}

	return start, nil
}

// parseStagedDocument parses data into a document node, refusing anchors,
// aliases and merge keys document-wide before anything else is exposed.
func parseStagedDocument(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: the document does not parse", ErrConfigUnsupportedYAML)
	}

	if err := checkNoAnchorsAliasesMerges(&doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

// checkNoAnchorsAliasesMerges walks the whole document and refuses anchors,
// aliases and merge keys. The restriction is document-wide: an alias can
// supply a credential from an unclassified key, defeating the reference-only rule.
func checkNoAnchorsAliasesMerges(node *yaml.Node) error { //nolint:cyclop // linear walker over five cases
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if err := checkNoAnchorsAliasesMerges(child); err != nil {
				return err
			}
		}

		return nil
	case yaml.AliasNode:
		return fmt.Errorf("%w: YAML aliases are not supported by config-manager", ErrConfigUnsupportedYAML)
	case yaml.ScalarNode:
		if node.Value == "<<" && (node.Tag == "!!merge" || node.Alias != nil) {
			return fmt.Errorf("%w: YAML merge keys are not supported by config-manager", ErrConfigUnsupportedYAML)
		}
		if node.Anchor != "" {
			return fmt.Errorf("%w: YAML anchors are not supported by config-manager", ErrConfigUnsupportedYAML)
		}

		return nil
	case yaml.MappingNode, yaml.SequenceNode:
		if node.Anchor != "" {
			return fmt.Errorf("%w: YAML anchors are not supported by config-manager", ErrConfigUnsupportedYAML)
		}
		for _, child := range node.Content {
			if err := checkNoAnchorsAliasesMerges(child); err != nil {
				return err
			}
		}

		return nil
	default:
		return fmt.Errorf("%w: unsupported YAML node", ErrConfigUnsupportedYAML)
	}
}

// documentVersion reads config_version from the parsed document.
func documentVersion(doc *yaml.Node) (int, bool, error) {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return 0, false, fmt.Errorf("%w: empty document", ErrConfigUnsupportedYAML)
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return 0, false, fmt.Errorf("%w: document root is not a mapping", ErrConfigUnsupportedYAML)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "config_version" {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(root.Content[i+1].Value))
		if err != nil {
			return 0, true, fmt.Errorf("%w: config_version is not an integer", ErrConfigUnsupportedYAML)
		}

		return v, true, nil
	}

	return 0, false, nil
}

// analyzeSpans parses raw text, checks the schema version against the
// compiled target and computes the exact byte span of every classified
// literal scalar — a value written where only a Secret reference belongs.
// Any unsupported construct fails closed with no spans returned.
func analyzeSpans(data []byte) (*stagedDoc, error) {
	doc, err := parseStagedDocument(data)
	if err != nil {
		return nil, err
	}
	version, present, err := documentVersion(doc)
	if err != nil {
		return nil, err
	}
	if !present || version != config.TargetConfigVersion {
		if !present {
			return nil, fmt.Errorf("%w: missing config_version", ErrConfigUnsupportedYAML)
		}

		return nil, fmt.Errorf("%w: config_version %d does not equal this binary's target %d — run the ordinary migration flow instead", ErrConfigUnsupportedYAML, version, config.TargetConfigVersion)
	}

	d := &stagedDoc{version: version}
	if err = collectSensitiveSpans(data, doc, []string{}, false, d); err != nil {
		return nil, err
	}

	return d, nil
}

// collectSensitiveSpans walks mappings/sequences, tracking the exact path
// and any flow-style ancestor, and computes spans for classified scalars.
func collectSensitiveSpans(data []byte, node *yaml.Node, path []string, flowAncestor bool, d *stagedDoc) error {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil
		}

		return collectSensitiveSpans(data, node.Content[0], path, flowAncestor, d)
	case yaml.MappingNode:
		if node.Style&yaml.FlowStyle != 0 {
			flowAncestor = true
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("%w: unsupported mapping key", ErrConfigUnsupportedYAML)
			}
			childPath := append(append([]string{}, path...), key.Value)
			if err := collectSensitiveSpans(data, value, childPath, flowAncestor, d); err != nil {
				return err
			}
		}

		return nil
	case yaml.SequenceNode:
		if node.Style&yaml.FlowStyle != 0 {
			flowAncestor = true
		}
		for i, child := range node.Content {
			childPath := append(append([]string{}, path...), strconv.Itoa(i))
			if err := collectSensitiveSpans(data, child, childPath, flowAncestor, d); err != nil {
				return err
			}
		}

		return nil
	case yaml.ScalarNode:
		if !config.IsSensitivePath(path) || isReferenceScalar(node) {
			return nil
		}
		span, err := computeScalarSpan(data, node)
		if err != nil {
			return err
		}
		if flowAncestor || node.Style&yaml.FlowStyle != 0 {
			return fmt.Errorf("%w: sensitive scalar at %s sits in a flow container — config-manager supports block scalars only", ErrConfigUnsupportedYAML, strings.Join(path, "."))
		}

		d.spans = append(d.spans, configSpan{
			path: path, style: node.Style, start: span[0], end: span[1],
			value:     node.Value,
			multiline: node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || strings.Contains(node.Value, "\n"),
		})

		return nil
	default:
		if config.IsSensitivePath(path) {
			return fmt.Errorf("%w: sensitive field %s is not a scalar", ErrConfigUnsupportedYAML, strings.Join(path, "."))
		}

		return nil
	}
}

// isReferenceScalar reports whether a sensitive scalar is a legal Secret
// reference (`!secret`/`!secret_file`) or an absent value, i.e. not a literal.
func isReferenceScalar(node *yaml.Node) bool {
	switch node.Tag {
	case config.TagSecret, config.TagSecretFile:
		return true
	case "!!null":
		return true
	default:
		return false
	}
}

// computeScalarSpan returns the exact byte range of a scalar token.
func computeScalarSpan(data []byte, node *yaml.Node) ([2]int, error) {
	offs := lineOffsets(data)
	start, err := spanOf(data, offs, node.Line, node.Column)
	if err != nil {
		return [2]int{}, err
	}

	switch node.Style {
	case yaml.LiteralStyle, yaml.FoldedStyle:
		return blockScalarSpan(data, node, start)
	case yaml.DoubleQuotedStyle:
		return quotedSpan(data, node, start, '"')
	case yaml.SingleQuotedStyle:
		return quotedSpan(data, node, start, '\'')
	case 0, yaml.TaggedStyle:
		return plainSpan(data, node, start)
	default:
		return [2]int{}, fmt.Errorf("%w: unsupported scalar style at line %d", ErrConfigUnsupportedYAML, node.Line)
	}
}

// contiguousScalarSpan verifies a candidate span by parsing it as YAML and
// comparing the decoded scalar value with the expected node value.
func contiguousScalarSpan(data []byte, node *yaml.Node, start, end int) ([2]int, error) {
	candidateBytes := make([]byte, 0, end-start)
	candidateBytes = append(candidateBytes, data[start:end]...)

	var probe yaml.Node
	if err := yaml.Unmarshal(candidateBytes, &probe); err != nil {
		return [2]int{}, fmt.Errorf("%w: scalar at line %d does not round trip", ErrConfigUnsupportedYAML, node.Line)
	}
	scalar := probe
	if scalar.Kind == yaml.DocumentNode && len(scalar.Content) > 0 {
		scalar = *scalar.Content[0]
	}
	if scalar.Kind != yaml.ScalarNode || node.Value != scalar.Value {
		return [2]int{}, fmt.Errorf("%w: scalar at line %d does not round trip", ErrConfigUnsupportedYAML, node.Line)
	}

	return [2]int{start, end}, nil
}

// plainSpan computes the span of a plain scalar: from its column to the end
// of the token on that line, bounded by a comment start or the line end.
func plainSpan(data []byte, node *yaml.Node, start int) ([2]int, error) {
	if strings.Contains(node.Value, "\n") {
		return [2]int{}, fmt.Errorf("%w: multi-line plain scalar at line %d is not supported", ErrConfigUnsupportedYAML, node.Line)
	}
	end := start
	for end < len(data) {
		b := data[end]
		if b == '\n' || b == '\r' {
			break
		}
		if b == '#' && end > start && (data[end-1] == ' ' || data[end-1] == '\t') {
			break
		}
		if b == '#' && end == start {
			// "#..." means the column was not the value start.
			break
		}
		end++
	}
	// Trim trailing whitespace/indentation noise off the token.
	for end > start && (data[end-1] == ' ' || data[end-1] == '\t') {
		end--
	}
	if end < start {
		end = start
	}

	if node.Value == "" {
		// An empty scalar (`key:` with nothing): the span is empty and
		// verified by position only.
		return [2]int{start, start}, nil
	}

	return contiguousScalarSpan(data, node, start, end)
}

// quotedSpan computes the span of a single- or double-quoted scalar,
// resolving escape sequences to find the genuine closing quote.
func quotedSpan(data []byte, node *yaml.Node, start int, quote byte) ([2]int, error) {
	if start >= len(data) || data[start] != quote {
		return [2]int{}, fmt.Errorf("%w: quoted scalar at line %d does not begin with its quote", ErrConfigUnsupportedYAML, node.Line)
	}
	end := start + 1
	for end < len(data) {
		b := data[end]
		if b == '\n' || b == '\r' {
			return [2]int{}, fmt.Errorf("%w: quoted scalar at line %d is not closed on its line", ErrConfigUnsupportedYAML, node.Line)
		}
		if quote == '"' && b == '\\' {
			end += 2

			continue
		}
		if quote == '\'' && b == '\'' && end+1 < len(data) && data[end+1] == '\'' {
			end += 2

			continue
		}
		if b == quote {
			end++

			break
		}
		end++
	}
	if end > len(data) || (end <= len(data) && end-1 >= start && data[end-1] != quote) {
		return [2]int{}, fmt.Errorf("%w: quoted scalar at line %d is not closed", ErrConfigUnsupportedYAML, node.Line)
	}

	return contiguousScalarSpan(data, node, start, end)
}

// blockScalarSpan computes the span of a literal (|) or folded (>)
// scalar: the header through the last content line of its block.
func blockScalarSpan(data []byte, node *yaml.Node, start int) ([2]int, error) {
	offs := lineOffsets(data)
	headerLine := node.Line
	lineEnd := start
	for lineEnd < len(data) && data[lineEnd] != '\n' {
		lineEnd++
	}

	// Content lines start after the header. The block runs while lines are
	// blank or indented at least as far as the first content line.
	blockIndent := -1
	end := lineEnd
	if end < len(data) && data[end] == '\n' {
		line := headerLine + 1
		for line <= len(offs) {
			lineStart := offs[line-1]
			lineStop := lineStart
			for lineStop < len(data) && data[lineStop] != '\n' {
				lineStop++
			}
			// Keep a CRLF line ending out of the block content span.
			if lineStop > lineStart && data[lineStop-1] == '\r' && lineStop < len(data) && data[lineStop] == '\n' {
				lineStop--
			}
			text := string(data[lineStart:lineStop])
			if strings.TrimSpace(text) == "" {
				// Blank lines stay part of the block until a non-blank,
				// shallower line ends it.
				if blockIndent < 0 {
					end = lineStop

					break
				}
				end = lineStop
				line++

				continue
			}
			indent := 0
			for indent < len(text) && text[indent] == ' ' {
				indent++
			}
			if blockIndent < 0 {
				blockIndent = indent
			}
			if indent < blockIndent {
				break
			}
			end = lineStop
			line++
		}
	}

	if strings.Contains(node.Value, "\n") && end == lineEnd {
		return [2]int{}, fmt.Errorf("%w: block scalar at line %d has no content lines", ErrConfigUnsupportedYAML, node.Line)
	}
	if end < len(data) && data[end] == '\r' {
		end++
	}
	if end < len(data) && data[end] == '\n' {
		end++
	}

	return contiguousScalarSpan(data, node, start, end)
}

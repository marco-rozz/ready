package okf

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// keySpan is the line range one top-level frontmatter key owns inside the
// block: comment lines directly above the key line belong to it, and it runs
// to the line before the next key's span.
type keySpan struct {
	key      string
	start    int // first line of the span (comment lines above included)
	keyLine  int // the key's own line
	end      int // exclusive
	node     *yaml.Node
	valueNod *yaml.Node
}

// fmDoc is a concept file taken apart for splicing. All line slices keep
// their original terminators so untouched spans are copied byte for byte.
type fmDoc struct {
	bom      string
	open     string   // opening delimiter line, with terminator
	preamble []string // lines before the first key's span
	spans    []keySpan
	lines    []string // block lines (between the delimiters)
	closing  string   // closing delimiter line, with terminator
	body     string   // raw text after the closing delimiter line
}

// splitKeepEnds splits text into lines keeping each terminator (\n, \r\n or
// a lone \r).
func splitKeepEnds(text string) []string {
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			out = append(out, text[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			out = append(out, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}

	return out
}

func trimEOL(s string) string { return strings.TrimRight(s, "\r\n") }

// parseDoc splits a concept file for merging. A file whose frontmatter is
// absent, unclosed, unparsable, not a mapping, or that repeats a key (or
// uses aliases, which would tie spans together) is unmergeable.
func parseDoc(text string) (*fmDoc, error) {
	d := &fmDoc{}
	if strings.HasPrefix(text, bom) {
		d.bom, text = bom, text[len(bom):]
	}
	fm := ParseFrontmatter(text)
	switch fm.State {
	case FMOK:
	case FMAbsent:
		return nil, fmt.Errorf("%w: no frontmatter", ErrUnmergeable)
	case FMDuplicate:
		return nil, fmt.Errorf("%w: repeated key %s (E007)", ErrUnmergeable, pyRepr(fm.DupKey))
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnmergeable, fm.Problem)
	}
	all := splitKeepEnds(text)
	closeIdx := -1
	for i := 1; i < len(all); i++ {
		if trimEOL(all[i]) == fmDelimiter {
			closeIdx = i

			break
		}
	}
	if closeIdx < 0 {
		return nil, fmt.Errorf("%w: the closing --- line is missing", ErrUnmergeable)
	}
	d.open, d.lines, d.closing = all[0], all[1:closeIdx], all[closeIdx]
	d.body = strings.Join(all[closeIdx+1:], "")

	var doc yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(strings.Join(d.lines, "")))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			d.preamble = d.lines

			return d, nil
		}

		return nil, fmt.Errorf("%w: %w", ErrUnmergeable, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: frontmatter is not a mapping", ErrUnmergeable)
	}
	if hasAlias(root) {
		return nil, fmt.Errorf("%w: YAML anchors and aliases are not supported", ErrUnmergeable)
	}
	keyLines := make([]int, 0, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyLines = append(keyLines, root.Content[i].Line-1) // 0-based block line
	}
	if len(keyLines) == 0 {
		d.preamble = d.lines

		return d, nil
	}
	starts := make([]int, len(keyLines))
	for i, kl := range keyLines {
		s := kl
		floor := 0
		if i > 0 {
			floor = keyLines[i-1] + 1
		}
		for s-1 >= floor && strings.HasPrefix(trimEOL(d.lines[s-1]), "#") {
			s--
		}
		starts[i] = s
	}
	d.preamble = d.lines[:starts[0]]
	for i := range keyLines {
		end := len(d.lines)
		if i+1 < len(keyLines) {
			end = starts[i+1]
		}
		d.spans = append(d.spans, keySpan{
			key: root.Content[i*2].Value, start: starts[i], keyLine: keyLines[i], end: end,
			node: root.Content[i*2], valueNod: root.Content[i*2+1],
		})
	}

	return d, nil
}

func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}

	return false
}

func (d *fmDoc) span(key string) *keySpan {
	for i := range d.spans {
		if d.spans[i].key == key {
			return &d.spans[i]
		}
	}

	return nil
}

// keyEdit is one change to the block: a replacement text for a key's span
// (from its key line, keeping the comments above), or removal when remove is
// set; keys with no span are appended.
type keyEdit struct {
	key    string
	text   string // rendered replacement (LF lines), "" with remove
	remove bool
	// appendLines, when set on an existing key, appends these lines after
	// the last non-blank line of the span instead of replacing it.
	appendLines string
}

// render rebuilds the file applying edits. Untouched spans are copied
// verbatim; the body is passed through as supplied.
func (d *fmDoc) render(edits []keyEdit, body string) string {
	byKey := map[string]keyEdit{}
	var appended []keyEdit
	for _, e := range edits {
		if d.span(e.key) != nil {
			byKey[e.key] = e
		} else if !e.remove {
			appended = append(appended, e)
		}
	}
	var sb strings.Builder
	sb.WriteString(d.bom)
	sb.WriteString(d.open)
	for _, l := range d.preamble {
		sb.WriteString(l)
	}
	for _, sp := range d.spans {
		e, edited := byKey[sp.key]
		if !edited {
			writeLines(&sb, d.lines[sp.start:sp.end])

			continue
		}
		switch {
		case e.remove:
			// the whole span, comments included, goes away
		case e.appendLines != "":
			last := sp.end
			for last > sp.keyLine+1 && strings.TrimSpace(d.lines[last-1]) == "" {
				last--
			}
			writeLines(&sb, d.lines[sp.start:last])
			if last > 0 && !strings.HasSuffix(d.lines[last-1], "\n") && !strings.HasSuffix(d.lines[last-1], "\r") {
				sb.WriteString("\n")
			}
			sb.WriteString(e.appendLines)
			writeLines(&sb, d.lines[last:sp.end])
		default:
			writeLines(&sb, d.lines[sp.start:sp.keyLine])
			sb.WriteString(e.text)
		}
	}
	for _, e := range appended {
		sb.WriteString(e.text)
	}
	sb.WriteString(d.closing)
	sb.WriteString(body)

	return sb.String()
}

func writeLines(sb *strings.Builder, lines []string) {
	for _, l := range lines {
		sb.WriteString(l)
	}
}

// newDocText renders a brand-new concept: the emitter's frontmatter and body.
func newDocText(entries []keyEntry, body string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	for _, e := range entries {
		sb.WriteString(renderEntry(e.key, e.value, 0))
	}
	sb.WriteString("---\n")
	sb.WriteString(body)

	return sb.String()
}

type keyEntry struct {
	key   string
	value any
}

// normalizeNewBody turns a replacement body into the written form: LF line
// endings, no leading blank lines, one blank line after the frontmatter and
// exactly one trailing newline. An empty body stays empty.
func normalizeNewBody(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	body = strings.TrimLeft(body, "\n")
	body = strings.TrimRight(body, "\n \t")
	if body == "" {
		return ""
	}

	return "\n" + body + "\n"
}

// normalizeBody applies the meaning rule's body normalization: LF endings,
// trailing whitespace per line removed, runs of blank lines collapsed to one,
// leading and trailing blank lines removed.
func normalizeBody(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	var out []string
	blank := false
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimRight(l, " \t")
		if l == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, l)
	}

	return strings.Trim(strings.Join(out, "\n"), "\n")
}

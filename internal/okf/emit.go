package okf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// omap is an insertion-ordered mapping: JSON objects of tool arguments and
// YAML mappings read back from a file keep their key order through the
// deterministic emitter.
type omap struct {
	keys []string
	vals map[string]any
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

func (m *omap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *omap) get(k string) (any, bool) {
	v, ok := m.vals[k]

	return v, ok
}

// parseOrdered decodes a JSON document keeping object key order. Objects
// become *omap, arrays []any, numbers json.Number-derived float64/int64.
func parseOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}

	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := newOmap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("invalid JSON: %w", err)
				}
				key, _ := kt.(string)
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				m.set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}

			return m, nil
		case '[':
			list := []any{}
			for dec.More() {
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}

			return list, nil
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", t.String())
		}

		return f, nil
	}

	return tok, nil
}

// nodeToOrdered converts a YAML node to the emitter's value model, keeping
// mapping order. Aliases are rejected by the caller beforehand.
func nodeToOrdered(n *yaml.Node) any {
	switch n.Kind {
	case yaml.MappingNode:
		m := newOmap()
		for i := 0; i+1 < len(n.Content); i += 2 {
			m.set(n.Content[i].Value, nodeToOrdered(n.Content[i+1]))
		}

		return m
	case yaml.SequenceNode:
		list := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			list = append(list, nodeToOrdered(c))
		}

		return list
	case yaml.ScalarNode:
		return scalarValue(n)
	}

	return nil
}

var plainKeyRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

func quoteString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)

	return strings.TrimSuffix(buf.String(), "\n")
}

func renderKey(k string) string {
	if plainKeyRE.MatchString(k) {
		return k
	}

	return quoteString(k)
}

func isScalar(v any) bool {
	switch v.(type) {
	case *omap, []any, map[string]any:
		return false
	}

	return true
}

func renderScalar(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return quoteString(x)
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}

	return quoteString(fmt.Sprint(v))
}

func asOrdered(v any) (*omap, bool) {
	switch x := v.(type) {
	case *omap:
		return x, true
	case map[string]any:
		m := newOmap()
		for _, k := range sortedKeys(x) {
			m.set(k, x[k])
		}

		return m, true
	}

	return nil, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort keeps this dependency-free for tiny maps
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}

	return keys
}

func allScalars(l []any) bool {
	for _, e := range l {
		if !isScalar(e) {
			return false
		}
	}

	return true
}

// renderFlow renders a scalar sequence or a mapping of scalars inline.
func renderFlow(v any) (string, bool) {
	if l, ok := v.([]any); ok {
		if !allScalars(l) {
			return "", false
		}
		parts := make([]string, len(l))
		for i, e := range l {
			parts[i] = renderScalar(e)
		}

		return "[" + strings.Join(parts, ", ") + "]", true
	}
	if m, ok := asOrdered(v); ok {
		parts := make([]string, 0, len(m.keys))
		for _, k := range m.keys {
			ev := m.vals[k]
			if !isScalar(ev) {
				return "", false
			}
			parts = append(parts, renderKey(k)+": "+renderScalar(ev))
		}
		if len(parts) == 0 {
			return "{}", true
		}

		return "{ " + strings.Join(parts, ", ") + " }", true
	}

	return "", false
}

// renderEntry renders "key: value" at the given indent as one or more LF
// lines. Strings are double-quoted, numbers/booleans plain, scalar sequences
// in flow style, other collections in block style with 2-space indentation.
func renderEntry(key string, v any, indent int) string {
	pad := strings.Repeat(" ", indent)
	head := pad + renderKey(key) + ":"
	if x, ok := v.([]any); ok {
		if len(x) == 0 {
			return head + " []\n"
		}
		if allScalars(x) {
			s, _ := renderFlow(x)

			return head + " " + s + "\n"
		}

		return head + "\n" + renderSeqItems(x, indent+2)
	}
	if m, ok := asOrdered(v); ok {
		if len(m.keys) == 0 {
			return head + " {}\n"
		}

		return head + "\n" + renderMapEntries(m, indent+2)
	}

	return head + " " + renderScalar(v) + "\n"
}

func renderMapEntries(m *omap, indent int) string {
	var sb strings.Builder
	for _, k := range m.keys {
		sb.WriteString(renderEntry(k, m.vals[k], indent))
	}

	return sb.String()
}

func renderSeqItems(l []any, indent int) string {
	pad := strings.Repeat(" ", indent)
	var sb strings.Builder
	for _, e := range l {
		switch {
		case isScalar(e):
			sb.WriteString(pad + "- " + renderScalar(e) + "\n")
		default:
			if s, ok := renderFlow(e); ok {
				sb.WriteString(pad + "- " + s + "\n")

				continue
			}
			if m, ok := asOrdered(e); ok && len(m.keys) > 0 {
				body := renderMapEntries(m, indent+2)
				sb.WriteString(pad + "- " + strings.TrimLeft(body[:strings.IndexByte(body, '\n')+1], " "))
				sb.WriteString(body[strings.IndexByte(body, '\n')+1:])

				continue
			}
			if inner, ok := e.([]any); ok {
				sb.WriteString(pad + "-\n" + renderSeqItems(inner, indent+2))
			}
		}
	}

	return sb.String()
}

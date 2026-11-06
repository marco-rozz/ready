package tools

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/marco-rozz/ready/internal/config"

	"gopkg.in/yaml.v3"
)

// maskText replaces a hand-edited literal credential in the staged view. The
// model never sees the value; a literal can only be moved into the Secret
// store by the operator (`ready secrets adopt`).
const maskText = "‹secret value not allowed — run ready secrets adopt›"

// adoptGuidance is the shared refusal remedy.
const adoptGuidance = "run `ready secrets adopt` to move it into the Secret store"

// ErrLiteralSecret reports a literal credential where only a Secret reference
// belongs. It never carries the value.
var ErrLiteralSecret = errors.New("a literal secret is not allowed in the configuration")

// literalSpans returns every literal scalar at a sensitive path, in document
// order. Fully tagged references and absent values are not literals.
func literalSpans(data []byte) ([]configSpan, error) {
	doc, err := analyzeSpans(data)
	if err != nil {
		return nil, err
	}
	spans := append([]configSpan(nil), doc.spans...)
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })

	return spans, nil
}

// maskLiterals replaces every single-line hand-edited literal with maskText,
// located by YAML node position. A multi-line or block literal refuses with
// the same guidance: it cannot be masked without disclosing line structure.
func maskLiterals(data []byte) (masked []byte, count int, err error) {
	spans, err := literalSpans(data)
	if err != nil {
		return nil, 0, err
	}
	for _, sp := range spans {
		if sp.multiline {
			return nil, 0, fmt.Errorf("%w: %s holds a multi-line value; %s", ErrLiteralSecret, strings.Join(sp.path, "."), adoptGuidance)
		}
	}

	return replaceSpans(data, spans, maskText), len(spans), nil
}

// substituteLiterals swaps every literal (including the mask) for a harmless
// reference so the document still decodes for structural validation.
func substituteLiterals(data []byte) ([]byte, error) {
	spans, err := literalSpans(data)
	if err != nil {
		return nil, err
	}

	return replaceSpans(data, spans, config.TagSecret+" masked_literal"), nil
}

// newLiterals lists the sensitive paths that hold a literal other than the
// mask — what an update must refuse. Paths only, never values.
func newLiterals(data []byte) ([]string, error) {
	spans, err := literalSpans(data)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sp := range spans {
		if sp.value != maskText {
			out = append(out, strings.Join(sp.path, "."))
		}
	}

	return out, nil
}

// remainingLiterals counts literals of any kind, mask included: a save is
// refused while one remains.
func remainingLiterals(data []byte) (int, error) {
	spans, err := literalSpans(data)
	if err != nil {
		return 0, err
	}

	return len(spans), nil
}

func replaceSpans(data []byte, spans []configSpan, with string) []byte {
	out := cloneBytes(data)
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		next := make([]byte, 0, len(out)-(sp.end-sp.start)+len(with))
		next = append(next, out[:sp.start]...)
		next = append(next, with...)
		next = append(next, out[sp.end:]...)
		out = next
	}

	return out
}

// suggestionFor names the canonical Secret name for a refused path, so the
// refusal can say which reference to write instead.
func suggestionFor(dotted string) string {
	if name, ok := config.CanonicalSecretName(strings.Split(dotted, ".")); ok {
		return name
	}

	return "<name>"
}

// literalRefusal renders the update refusal for literals at the given paths.
func literalRefusal(paths []string) error {
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, fmt.Sprintf("%s (write `%s %s` and set the value with `ready secrets set %s` or the TUI secret editor)",
			p, config.TagSecret, suggestionFor(p), suggestionFor(p)))
	}

	return fmt.Errorf("%w: %s", ErrLiteralSecret, strings.Join(parts, "; "))
}

// scalarAt returns the tag and value of the scalar at the mapping-key path.
func scalarAt(data []byte, path ...string) (tag, value string, ok bool) {
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil {
		return "", "", false
	}
	node := &doc
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	for _, seg := range path {
		if node.Kind != yaml.MappingNode {
			return "", "", false
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == seg {
				next = node.Content[i+1]

				break
			}
		}
		if next == nil {
			return "", "", false
		}
		node = next
	}
	if node.Kind != yaml.ScalarNode {
		return "", "", false
	}

	return node.Tag, node.Value, true
}

// referenceChanged reports whether the scalar at path differs between two
// documents (presence, tag or value).
func referenceChanged(before, after []byte, path ...string) bool {
	bt, bv, bok := scalarAt(before, path...)
	at, av, aok := scalarAt(after, path...)

	return bok != aok || bt != at || bv != av
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)

	return out
}

// AdoptItem is one literal credential found at a sensitive path. Value is the
// secret itself: callers move it into the Secret store and must never log it.
type AdoptItem struct {
	Path  string
	Name  string
	Value string
}

// AdoptLiterals replaces every literal at a sensitive path with a
// `!secret <name>` reference and returns the rewritten document plus the
// values to store. nameFor chooses the Secret name for a dotted path; it is
// called once per distinct value, so equal values share one name. Both
// single-line and block literals are supported (this runs offline, under the
// operator's own authority, never on the model's behalf).
func AdoptLiterals(data []byte, nameFor func(dottedPath string) string) ([]byte, []AdoptItem, error) {
	spans, err := literalSpans(data)
	if err != nil {
		return nil, nil, err
	}
	var items []AdoptItem
	byValue := map[string]string{}
	out := cloneBytes(data)
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		dotted := strings.Join(sp.path, ".")
		name, shared := byValue[sp.value]
		if !shared {
			name = nameFor(dotted)
			byValue[sp.value] = name
		}
		items = append(items, AdoptItem{Path: dotted, Name: name, Value: sp.value})
		next := make([]byte, 0, len(out))
		next = append(next, out[:sp.start]...)
		next = append(next, (config.TagSecret + " " + name)...)
		next = append(next, out[sp.end:]...)
		out = next
	}
	// Report in document order.
	for l, r := 0, len(items)-1; l < r; l, r = l+1, r-1 {
		items[l], items[r] = items[r], items[l]
	}

	return out, items, nil
}

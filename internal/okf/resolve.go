package okf

import (
	"errors"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var (
	linkRE       = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	footnoteRE   = regexp.MustCompile(`\[\^([^\]\s]+)\]`)
	inlineCodeRE = regexp.MustCompile("`[^`]*`")
	headingRE    = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	schemeRE     = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// RefKind is the outcome of resolving a link target or path field.
type RefKind int

// Resolution outcomes (OKF section 6).
const (
	// RefExternal: a URL, or a location outside the bundle root (never read).
	RefExternal RefKind = iota
	// RefFound: the target exists inside the bundle.
	RefFound
	// RefFallback: a bare path field found only through the v0.2 root fallback.
	RefFallback
	// RefMissing: nothing in the bundle.
	RefMissing
)

// Resolution is a resolved reference; Target is the bundle-relative path
// (empty for external and missing references; the source for "").
type Resolution struct {
	Kind   RefKind
	Target string
}

// String renders the resolution for tool output.
func (r Resolution) String() string {
	switch r.Kind {
	case RefExternal:
		return "external (not read)"
	case RefFound:
		return "-> " + r.Target
	case RefFallback:
		return "-> " + r.Target + " (resolved from the bundle root through the v0.2 fallback)"
	}

	return "missing"
}

// exists reports whether rel names an existing entry inside the bundle with
// an exact-case match; external is true when the lookup left the bundle (a
// link resolving outside the root).
func (b *Bundle) exists(rel string) (found, external bool) {
	if rel == "." || b.overlay[rel] {
		return true, false
	}
	if _, err := b.root.Stat(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, false
		}

		return false, true
	}
	// Paths are case-sensitive: confirm each segment by exact name, even on
	// case-folding filesystems.
	parent := "."
	for _, seg := range strings.Split(rel, "/") {
		entries, err := fs.ReadDir(b.root.FS(), parent)
		if err != nil {
			return false, false
		}
		ok := false
		for _, e := range entries {
			if e.Name() == seg {
				ok = true

				break
			}
		}
		if !ok {
			return false, false
		}
		parent = path.Join(parent, seg)
	}

	return true, false
}

// resolve implements the section 6 rules for a link target or path field
// found in the bundle-relative file source. field enables the v0.2 root
// fallback for eligible bundles (path fields only, never body links).
func (b *Bundle) resolve(source, target string, field bool) Resolution {
	if schemeRE.MatchString(target) || strings.HasPrefix(target, "//") {
		return Resolution{Kind: RefExternal}
	}
	clean := target
	if i := strings.IndexByte(clean, '#'); i >= 0 {
		clean = clean[:i]
	}
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	if un, err := url.PathUnescape(clean); err == nil {
		clean = un
	}
	if clean == "" {
		return Resolution{Kind: RefFound, Target: source}
	}
	base := path.Dir(source)
	if strings.HasPrefix(clean, "/") {
		base = "."
	}
	cand := path.Clean(path.Join(base, strings.TrimLeft(clean, "/")))
	if outside(cand) {
		return Resolution{Kind: RefExternal}
	}
	found, external := b.exists(cand)
	if external {
		return Resolution{Kind: RefExternal}
	}
	if found {
		return Resolution{Kind: RefFound, Target: cand}
	}
	if field && b.fallback && !strings.HasPrefix(clean, "/") {
		alt := path.Clean(clean)
		if !outside(alt) {
			if ok, _ := b.exists(alt); ok {
				return Resolution{Kind: RefFallback, Target: alt}
			}
		}
	}

	return Resolution{Kind: RefMissing}
}

func outside(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel)
}

// resolveConceptID resolves a path field to a concept id: the target must be
// an existing .md file. ok is false otherwise.
func (b *Bundle) resolveConceptID(source string, v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	res := b.resolve(source, s, true)
	if (res.Kind != RefFound && res.Kind != RefFallback) || !strings.HasSuffix(res.Target, ".md") {
		return "", false
	}
	info, err := b.root.Stat(res.Target)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return strings.TrimSuffix(res.Target, ".md"), true
}

// proseLine is one body line outside fenced and indented code, with inline
// code removed.
type proseLine struct {
	Offset int
	Text   string
}

func proseLines(body string) []proseLine {
	var out []proseLine
	fence := ""
	for offset, line := range splitLines(body) {
		stripped := strings.TrimLeft(line, " \t\f\v")
		if fence == "" && (strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~")) {
			fence = stripped[:3]

			continue
		}
		if fence != "" {
			if strings.HasPrefix(stripped, fence) {
				fence = ""
			}

			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			continue
		}
		out = append(out, proseLine{Offset: offset, Text: inlineCodeRE.ReplaceAllString(line, "")})
	}

	return out
}

// linkRef is a body link target found on a prose line.
type linkRef struct {
	Line   int // 1-based file line
	Target string
}

// bodyLinks extracts the link targets of prose lines, excluding image links
// and "#" anchors.
func bodyLinks(body string, bodyLine int) []linkRef {
	var out []linkRef
	for _, pl := range proseLines(body) {
		for _, m := range linkRE.FindAllStringSubmatchIndex(pl.Text, -1) {
			if m[0] > 0 && pl.Text[m[0]-1] == '!' {
				continue
			}
			target := pl.Text[m[2]:m[3]]
			if strings.HasPrefix(target, "#") {
				continue
			}
			out = append(out, linkRef{Line: bodyLine + pl.Offset, Target: target})
		}
	}

	return out
}

// footnoteRef is a footnote label found on a prose line.
type footnoteRef struct {
	Line  int
	Label string
}

func bodyFootnotes(body string, bodyLine int) []footnoteRef {
	var out []footnoteRef
	for _, pl := range proseLines(body) {
		for _, m := range footnoteRE.FindAllStringSubmatch(pl.Text, -1) {
			out = append(out, footnoteRef{Line: bodyLine + pl.Offset, Label: m[1]})
		}
	}

	return out
}

// fenceBlock is a fenced code block inside the Computation section.
type fenceBlock struct {
	Info string
	Text string
}

// computationFences returns the fenced code blocks of the section headed
// "Computation" at any ATX level; the section ends at the next heading of the
// same or a higher level (OKF 10.3).
func computationFences(body string) []fenceBlock {
	level := 0
	inSection := false
	fence := ""
	var blocks []fenceBlock
	var cur []string
	var info string
	for _, line := range splitLines(body) {
		stripped := strings.TrimLeft(line, " \t\f\v")
		if fence != "" {
			if strings.HasPrefix(stripped, fence) {
				fence = ""
				if inSection {
					blocks = append(blocks, fenceBlock{Info: info, Text: strings.Join(cur, "\n")})
				}
				cur = nil
			} else if inSection {
				cur = append(cur, line)
			}

			continue
		}
		if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			fence = stripped[:3]
			info = strings.TrimSpace(strings.TrimLeft(stripped, fence[:1]))
			cur = nil

			continue
		}
		if m := headingRE.FindStringSubmatch(line); m != nil {
			depth := len(m[1])
			if inSection && depth <= level {
				break
			}
			if !inSection && strings.EqualFold(strings.TrimSpace(m[2]), "computation") {
				inSection, level = true, depth
			}
		}
	}
	// An unterminated fence in the section still counts as a block, like the
	// reference counter (it counts at the opening fence).
	if fence != "" && inSection {
		blocks = append(blocks, fenceBlock{Info: info, Text: strings.Join(cur, "\n")})
	}

	return blocks
}

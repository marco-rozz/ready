package okf

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// plannedFile is one file a call will write, built in memory before any
// write so the validation gate sees every candidate.
type plannedFile struct {
	Rel     string
	Text    string
	Old     string
	Existed bool
	Kind    string // concept | index | log
	// Outcome is "created", "updated" or "unchanged".
	Outcome string
}

// Log event kinds.
const (
	eventCreation     = "Creation"
	eventUpdate       = "Update"
	eventDeprecation  = "Deprecation"
	eventVerification = "Verification"
)

const (
	kindConcept = "concept"
	kindIndex   = "index"
	kindLog     = "log"
)

// logEvent is one bundle log entry.
type logEvent struct {
	Kind        string // Creation | Update | Deprecation | Verification
	Title       string
	Path        string // bundle-relative
	SupersedeBy *logRef
	Human       string // Verification only
}

type logRef struct{ Title, Path string }

// encodeSegment percent-encodes everything but the RFC 3986 unreserved set.
func encodeSegment(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '-', b == '.', b == '_', b == '~':
			sb.WriteByte(b)
		default:
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}

	return sb.String()
}

func encodePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = encodeSegment(s)
	}

	return strings.Join(segs, "/")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

func escapeTitle(s string) string {
	return strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`).Replace(oneLine(s))
}

// mdLink renders [Title](target) with the title escaped.
func mdLink(title, target string) string {
	return "[" + escapeTitle(title) + "](" + target + ")"
}

var toolEntryRE = regexp.MustCompile(`^\* \[((?:\\.|[^\]\\])*)\]\(([^)\s]+)\)(?: - (.*))?$`)

// indexEntryLine is the tool-format entry line.
func indexEntryLine(title, target, desc string) string {
	line := "* " + mdLink(title, target)
	if d := oneLine(desc); d != "" {
		line += " - " + d
	}

	return line
}

// linkTargets lists the resolved bundle-relative targets of the Markdown
// links on a list-item line, relative to the directory dir.
func linkTargets(line, dir string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "* ") && !strings.HasPrefix(trimmed, "- ") {
		return nil
	}
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(line, -1) {
		t := m[1]
		if i := strings.IndexAny(t, "#?"); i >= 0 {
			t = t[:i]
		}
		if un, err := url.PathUnescape(t); err == nil {
			t = un
		}
		if t == "" || schemeRE.MatchString(t) {
			continue
		}
		base := dir
		if strings.HasPrefix(t, "/") {
			base = "."
		}
		out = append(out, path.Clean(path.Join(base, strings.TrimLeft(t, "/"))))
	}

	return out
}

// ensureEntry makes the index text list target (a concept file or, with
// isDir, a sub-directory). A line whose link resolves to the same target
// counts as the entry; a tool-format one is refreshed, any other is left as a
// person wrote it. A missing entry is appended at the end.
func ensureEntry(text, indexDir, target string, isDir bool, want, wantTarget string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		for _, got := range linkTargets(line, indexDir) {
			if got != target && (!isDir || got != path.Join(target, "index.md")) {
				continue
			}
			if m := toolEntryRE.FindStringSubmatch(line); m != nil && m[2] == wantTarget {
				lines[i] = want
			}

			return strings.Join(lines, "\n")
		}
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}

	return text + want + "\n"
}

// newIndexText is the content of a freshly created index.md.
func newIndexText(dir, bundleName, title, desc string) string {
	if dir == "." {
		t := title
		if t == "" {
			t = bundleName
		}
		var sb strings.Builder
		sb.WriteString("---\n")
		sb.WriteString(renderEntry("okf_version", "0.3", 0))
		sb.WriteString(renderEntry("title", oneLine(t), 0))
		if d := oneLine(desc); d != "" {
			sb.WriteString(renderEntry("description", d, 0))
		}
		sb.WriteString("---\n\n# " + oneLine(t) + "\n\n")

		return sb.String()
	}

	return "# " + oneLine(path.Base(dir)) + "\n\n"
}

type maintInput struct {
	bundle      string
	conceptRel  string
	title, desc string

	bundleTitle, bundleDesc string

	// read returns the current text of a bundle file; exists is false when
	// the file is absent.
	read func(rel string) (text string, exists bool, err error)
}

// maintFailure is a maintenance file that could not be planned.
type maintFailure struct {
	Rel string
	Err error
}

// planIndexes computes the idempotent index maintenance: the concept's
// directory index, then every ancestor up to the root.
func planIndexes(in maintInput) ([]plannedFile, []maintFailure) {
	var files []plannedFile
	var fails []maintFailure
	dir := path.Dir(in.conceptRel)
	child := path.Base(in.conceptRel)
	isDir := false
	for {
		indexRel := path.Join(dir, fileIndex)
		if dir == "." {
			indexRel = fileIndex
		}
		old, exists, err := in.read(indexRel)
		if err != nil {
			fails = append(fails, maintFailure{Rel: indexRel, Err: err})
		} else {
			text := old
			if !exists {
				text = newIndexText(dir, in.bundle, in.bundleTitle, in.bundleDesc)
			}
			var next string
			if isDir {
				wt := encodeSegment(child) + "/"
				next = ensureEntry(text, dir, path.Join(dir, child), true, indexEntryLine(child, wt, ""), wt)
			} else {
				wt := encodeSegment(child)
				next = ensureEntry(text, dir, in.conceptRel, false, indexEntryLine(in.title, wt, in.desc), wt)
			}
			outcome := "unchanged"
			switch {
			case !exists:
				outcome = "created"
			case next != old:
				outcome = "updated"
			}
			files = append(files, plannedFile{Rel: indexRel, Text: next, Old: old, Existed: exists, Kind: kindIndex, Outcome: outcome})
		}
		if dir == "." {
			break
		}
		child = path.Base(dir)
		dir = path.Dir(dir)
		isDir = true
	}

	return files, fails
}

// logLine renders the event entry.
func logLine(e logEvent) string {
	link := mdLink(e.Title, encodePath(e.Path))
	switch e.Kind {
	case eventDeprecation:
		if e.SupersedeBy != nil {
			return "* **Deprecation**: " + link + ", superseded by " + mdLink(e.SupersedeBy.Title, encodePath(e.SupersedeBy.Path)) + "."
		}

		return "* **Deprecation**: " + link + "."
	case eventVerification:
		return "* **Verification**: " + link + " confirmed by human:" + e.Human + "."
	}

	return "* **" + e.Kind + "**: " + link + "."
}

// planLog inserts the event into the root log.md under the event's UTC date.
func planLog(old string, exists bool, e logEvent, now time.Time) plannedFile {
	date := now.UTC().Format("2006-01-02")
	text := old
	if !exists {
		text = "# Bundle Update Log\n\n"
	}
	entry := logLine(e)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	heading := "## " + date
	insertAt, olderAt := -1, -1
	for i, l := range lines {
		if !strings.HasPrefix(l, "## ") {
			continue
		}
		h := strings.TrimSpace(l[3:])
		if !dateRE.MatchString(h) {
			continue
		}
		if h == date && insertAt < 0 {
			insertAt = i
		}
		if h < date && olderAt < 0 {
			olderAt = i
		}
	}
	switch {
	case insertAt >= 0:
		pos := insertAt + 1
		for pos < len(lines) && strings.TrimSpace(lines[pos]) == "" {
			pos++
		}
		lines = append(lines[:pos], append([]string{entry}, lines[pos:]...)...)
	case olderAt >= 0:
		block := make([]string, 0, 4+len(lines)-olderAt)
		block = append(block, heading, "", entry, "")
		block = append(block, lines[olderAt:]...)
		lines = append(lines[:olderAt], block...)
	default:
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, heading, "", entry)
	}
	next := strings.Join(lines, "\n") + "\n"
	outcome := "updated"
	if !exists {
		outcome = "created"
	}

	return plannedFile{Rel: fileLog, Text: next, Old: old, Existed: exists, Kind: kindLog, Outcome: outcome}
}

package okf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/marco-rozz/ready/internal/confinement"
)

// Fixed bounds (spec "Bounded tool output"). They are contract constants,
// not configuration.
const (
	// KnowledgeDirName is the reserved agent-directory child holding bundles.
	KnowledgeDirName = "okfs"

	// MaxFileBytes is the per-file read cap.
	MaxFileBytes = 1 << 20
	// MaxWalkFiles is the per-call cap on Markdown files visited.
	MaxWalkFiles = 5000
	// MaxWalkBytes is the per-call cap on bytes read.
	MaxWalkBytes = 16 << 20
	// MaxScanRows, MaxIndexEntries, MaxFindings and MaxReadBytes are the
	// output bounds of scan, index, validate and read.
	MaxScanRows     = 50
	MaxIndexEntries = 200
	MaxFindings     = 100
	MaxReadBytes    = 32 << 10
	// MaxCompareRefs bounds the compare action.
	MaxCompareRefs = 10
)

// Sentinels. Refusals are Permanent for the error digester; ErrIO wraps
// filesystem failures (Transient).
var (
	ErrUnknownAction       = errors.New("unknown okf_query action")
	ErrUnknownBundle       = errors.New("unknown bundle")
	ErrUnknownConcept      = errors.New("unknown concept or file")
	ErrInvalidReference    = errors.New("invalid reference")
	ErrOutsideBundle       = errors.New("target outside the bundle")
	ErrKnowledgeRootEscape = errors.New("knowledge root outside the agent directory")
	ErrIO                  = errors.New("knowledge bundle I/O failure")
)

// Skipped-file finding codes (READY-specific, never a reference code).
const (
	CodeInvalidUTF8 = "R301"
	CodeOversize    = "R302"
	CodeUnreadable  = "R303"
	CodeTruncated   = "R304"
)

// BundleInfo is the discovery view of one bundle.
type BundleInfo struct {
	Name        string
	Version     string // declared okf_version, "" when not declared
	Title       string
	Description string
	License     string
	HasIndex    bool
	Notes       []string
}

// Skipped names a directory entry that is not a bundle and why.
type Skipped struct {
	Name   string
	Reason string
}

// Knowledge is a handle on one agent's knowledge root (<agent-dir>/okfs).
type Knowledge struct {
	agentDir string
	dir      string
}

// NewKnowledge builds the handle for agentDir. Nothing is touched on disk.
func NewKnowledge(agentDir string) *Knowledge {
	return &Knowledge{agentDir: agentDir, dir: filepath.Join(agentDir, KnowledgeDirName)}
}

// Dir returns the knowledge root directory path.
func (k *Knowledge) Dir() string { return k.dir }

// checkRoot refuses a knowledge root resolving outside the agent directory.
func (k *Knowledge) checkRoot() error {
	if err := confinement.Check(k.agentDir, k.dir); err != nil {
		return fmt.Errorf("%w: %w", ErrKnowledgeRootEscape, err)
	}

	return nil
}

// Discover lists the bundles under the knowledge root and the skipped
// symlinked entries. A missing or empty root yields zero bundles.
func (k *Knowledge) Discover(ctx context.Context) ([]BundleInfo, []Skipped, error) {
	if err := k.checkRoot(); err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(k.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	var infos []BundleInfo
	var skipped []Skipped
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("discovering bundles: %w", err)
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			skipped = append(skipped, Skipped{Name: name, Reason: "symlinked entry (copy or clone the bundle inside okfs/ instead)"})

			continue
		}
		if !e.IsDir() {
			continue
		}
		b, err := k.Open(name)
		if err != nil {
			return nil, nil, err
		}
		infos = append(infos, b.Info())
		b.Close()
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	return infos, skipped, nil
}

// Names returns the discovered bundle names (for refusal messages).
func (k *Knowledge) names() []string {
	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)

	return out
}

// Open opens a bundle by exact lookup among the discovered names. The bundle
// argument is never joined onto a path before it has matched a discovered
// directory.
func (k *Knowledge) Open(name string) (*Bundle, error) {
	if err := k.checkRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(k.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	for _, e := range entries {
		if e.Name() != name || strings.HasPrefix(name, ".") || e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		b, err := OpenBundleDir(filepath.Join(k.dir, e.Name()), e.Name())
		if err != nil {
			return nil, err
		}

		return b, nil
	}
	avail := k.names()
	list := "none"
	if len(avail) > 0 {
		list = strings.Join(avail, ", ")
	}

	return nil, fmt.Errorf("%w %q (available: %s)", ErrUnknownBundle, name, list)
}

// Bundle is one opened bundle. Every read goes through an os.Root anchored
// at the bundle directory, so nothing outside it can ever be opened.
type Bundle struct {
	Name string
	root *os.Root

	version  string
	meta     map[string]any
	hasIndex bool
	notes    []string
	fallback bool

	// overlay lists bundle paths (files and their directories) that exist
	// only as in-memory write candidates; the validation gate sets it so a
	// candidate's links to a sibling candidate resolve. Never set on a
	// bundle that serves reads.
	overlay map[string]bool
}

// OpenBundleDir opens dir as a bundle. Used by Knowledge.Open and tests.
func OpenBundleDir(dir, name string) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	b := &Bundle{Name: name, root: root, meta: map[string]any{}}
	b.loadRootIndex()

	return b, nil
}

// Close releases the bundle root.
func (b *Bundle) Close() { _ = b.root.Close() }

func (b *Bundle) loadRootIndex() {
	text, err := b.readText(fileIndex, nil)
	switch {
	case err == nil:
		b.hasIndex = true
		fm := ParseFrontmatter(text)
		if fm.State == FMOK || fm.State == FMDuplicate {
			b.meta = fm.Meta
		}
	case errors.Is(err, fs.ErrNotExist):
		b.notes = append(b.notes, "no root index.md")
	default:
		b.hasIndex = true
		b.notes = append(b.notes, "root index.md unreadable")
	}
	if v, ok := b.meta["okf_version"]; ok && !blank(v) {
		b.version = str(v)
	}
	b.fallback = fallbackEligible(b.version)
	switch {
	case b.version == "" && b.hasIndex:
		b.notes = append(b.notes, "okf_version not declared")
	case b.version != "" && b.version != "0.3":
		b.notes = append(b.notes, "okf_version "+b.version+" is not 0.3: read best-effort with the v0.3 rules")
	}
}

// Info returns the discovery view.
func (b *Bundle) Info() BundleInfo {
	info := BundleInfo{Name: b.Name, Version: b.version, HasIndex: b.hasIndex, Notes: append([]string(nil), b.notes...)}
	if s, ok := b.meta["title"].(string); ok {
		info.Title = s
	}
	if s, ok := b.meta["description"].(string); ok {
		info.Description = s
	}
	if s, ok := b.meta["license"].(string); ok {
		info.License = s
	}

	return info
}

// FallbackEligible reports whether the older-version fallbacks apply.
func (b *Bundle) FallbackEligible() bool { return b.fallback }

// budget tracks the per-call caps shared by one walk or action.
type budget struct {
	files     int
	bytes     int64
	truncated bool
}

func (g *budget) allowFile() bool {
	if g == nil {
		return true
	}
	if g.files >= MaxWalkFiles || g.bytes >= MaxWalkBytes {
		g.truncated = true

		return false
	}
	g.files++

	return true
}

// readIssue is a per-file problem reported as output, never as an error.
type readIssue struct {
	Code   string
	Reason string
}

func (r *readIssue) Error() string { return r.Code + ": " + r.Reason }

// readText reads a bundle-relative file through the bundle root. The result
// is BOM-stripped valid UTF-8 text. Per-file problems come back as *readIssue.
func (b *Bundle) readText(rel string, g *budget) (string, error) {
	data, err := b.readRaw(rel, g)
	if err != nil {
		return "", err
	}

	return strings.TrimLeft(string(data), bom), nil
}

// readRaw reads a bundle-relative file through the bundle root and returns
// its raw bytes, valid UTF-8 checked, BOM kept. The content digest and the
// verification token are computed over exactly these bytes.
func (b *Bundle) readRaw(rel string, g *budget) ([]byte, error) {
	info, err := b.root.Stat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}

		return nil, &readIssue{CodeUnreadable, "unreadable or a link resolving outside the bundle"}
	}
	if !info.Mode().IsRegular() {
		return nil, &readIssue{CodeUnreadable, "not a regular file"}
	}
	if info.Size() > MaxFileBytes {
		return nil, &readIssue{CodeOversize, fmt.Sprintf("larger than the %d byte per-file cap", MaxFileBytes)}
	}
	f, err := b.root.Open(rel)
	if err != nil {
		return nil, &readIssue{CodeUnreadable, "unreadable or a link resolving outside the bundle"}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, &readIssue{CodeUnreadable, "read error"}
	}
	if len(data) > MaxFileBytes {
		return nil, &readIssue{CodeOversize, fmt.Sprintf("larger than the %d byte per-file cap", MaxFileBytes)}
	}
	if g != nil {
		g.bytes += int64(len(data))
	}
	if !utf8.Valid(data) {
		return nil, &readIssue{CodeInvalidUTF8, "not valid UTF-8"}
	}

	return data, nil
}

// mdFile is one Markdown file selected by a walk.
type mdFile struct {
	Rel string
}

// walkMarkdown lists the non-hidden .md files of the bundle, sorted
// segment-wise. Directory symlinks are never followed. The per-call file cap
// marks the walk truncated; ctx is checked per entry.
func (b *Bundle) walkMarkdown(ctx context.Context, sub string, g *budget) ([]mdFile, error) {
	start := "."
	if sub != "" {
		start = sub
	}
	var out []mdFile
	err := fs.WalkDir(b.root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if p == start {
				return err
			}

			return nil
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if d.Type()&fs.ModeSymlink == 0 && !d.Type().IsRegular() {
			return nil
		}
		if len(out) >= MaxWalkFiles {
			if g != nil {
				g.truncated = true
			}

			return fs.SkipAll
		}
		out = append(out, mdFile{Rel: p})

		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("walking bundle: %w", err)
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: directory %q", ErrUnknownConcept, sub)
		}

		return nil, fmt.Errorf("%w: %w", ErrIO, err)
	}
	sort.Slice(out, func(i, j int) bool { return lessSegments(out[i].Rel, out[j].Rel) })

	return out, nil
}

// lessSegments orders slash paths segment-wise.
func lessSegments(a, b string) bool {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}

	return len(as) < len(bs)
}

// ValidRelPath implements the lexical rule for concept and file arguments: a
// relative, "/"-separated path with no empty, "." or ".." segment and no
// segment starting with ".". It runs before any filesystem access.
func ValidRelPath(p string) error {
	if p == "" || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w %q: must be a relative, slash-separated path", ErrInvalidReference, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("%w %q: empty, dot or hidden path segment", ErrInvalidReference, p)
		}
	}

	return nil
}

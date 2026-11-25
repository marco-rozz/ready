package okf

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

// Write actions.
const (
	WriteCreate    = "create"
	WriteUpdate    = "update"
	WriteDeprecate = "deprecate"
)

const (
	keyGenerated  = "generated"
	keyVerified   = "verified"
	keyModelExt   = "x-ready-model"
	keyViaExt     = "x-ready-via"
	maxPathSegs   = 8
	maxPathBytes  = 255
	reasonPrefix  = "**Deprecated:** "
	modeFile      = 0o600
	modeDir       = 0o700
	tmpPrefix     = ".okf-write-"
	tmpSuffix     = ".tmp"
	resultCreated = "created"
	resultUpdated = "updated"
	resultDeprec  = "deprecated"
	resultNoop    = "unchanged"
	outcomeFailed = "failed"
)

var (
	segmentRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	versionRE = regexp.MustCompile(`[^A-Za-z0-9._+-]`)
	humanIDRE = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,64}$`)
)

// WriterOptions configures a Writer.
type WriterOptions struct {
	// AgentDir is the agent directory holding the reserved okfs/ root.
	AgentDir string
	// Locks is the process-lifetime lock registry; nil creates a private one
	// (tests only — production shares the shell-owned registry).
	Locks *Locks
	// Now is the injectable clock; nil means UTC wall time.
	Now func() time.Time
	// Version is the daemon build version stamped as generated.by.
	Version string
	// Logger receives WARN records for maintenance failures.
	Logger zerolog.Logger
}

// Writer is the single write path of the daemon into OKF bundles, shared by
// the okf_write tool and the /verify session command.
type Writer struct {
	k       *Knowledge
	locks   *Locks
	now     func() time.Time
	version string
	log     zerolog.Logger
}

// NewWriter builds a Writer.
func NewWriter(o WriterOptions) *Writer {
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	locks := o.Locks
	if locks == nil {
		locks = NewLocks()
	}
	v := versionRE.ReplaceAllString(o.Version, "-")
	if v == "" {
		v = "dev"
	}

	return &Writer{k: NewKnowledge(o.AgentDir), locks: locks, now: now, version: v, log: o.Logger.With().Str("component", "okf-writer").Logger()}
}

// WriteRequest is one okf_write call, already decoded.
type WriteRequest struct {
	Action string
	Bundle string
	// Path is the new concept path (create) or the concept reference
	// (update, deprecate).
	Path       string
	Fields     *omap
	Set        *omap
	Unset      []string
	Body       *string
	BaseDigest string

	SupersededBy string
	Reason       string

	BundleTitle       string
	BundleDescription string

	// Model is the "<provider>::<model>" reference of the attempting model.
	Model string
}

// MaintOutcome reports one maintained file.
type MaintOutcome struct {
	Rel     string
	Outcome string // created | updated | unchanged | failed
	Err     string
}

// Receipt is the result of a write.
type Receipt struct {
	Action    string
	Qualified string
	Result    string

	Digest     string
	PrevDigest string

	GenBy, GenAt, GenModel string

	TierBefore, TierAfter Tier
	NoLongerCurrent       int
	TierDropped           bool

	Maintenance []MaintOutcome
	Diff        string
	DiffOmitted bool
	Notes       []string

	Findings []Finding
	Errors   int
	Warnings int
	Infos    int
}

func validFieldKey(k string) bool {
	return k != "" && !strings.ContainsAny(k, "\r\n\x00") && k == strings.TrimSpace(k)
}

func refuseReserved(keys ...string) error {
	for _, k := range keys {
		if k == keyGenerated || k == keyVerified {
			return fmt.Errorf("%w (%q)", ErrReservedField, k)
		}
	}

	return nil
}

func omapKeys(m *omap) []string {
	if m == nil {
		return nil
	}

	return m.keys
}

// creationPath validates and normalizes a new concept path.
func creationPath(p string) (string, error) {
	if err := ValidRelPath(p); err != nil {
		return "", err
	}
	if !strings.HasSuffix(p, ".md") {
		p += ".md"
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxPathSegs || len(p) > maxPathBytes {
		return "", fmt.Errorf("%w %q: at most %d segments and %d bytes", ErrInvalidCreationPath, p, maxPathSegs, maxPathBytes)
	}
	for _, s := range segs {
		if !segmentRE.MatchString(s) {
			return "", fmt.Errorf("%w %q: segments use lowercase letters, digits, '.', '_' and '-' and start with a letter or digit", ErrInvalidCreationPath, p)
		}
	}
	if last := segs[len(segs)-1]; last == fileIndex || last == fileLog || last == ".md" {
		return "", fmt.Errorf("%w: %q", ErrReservedName, p)
	}

	return p, nil
}

// lstatRegular verifies rel is absent or a regular, non-symlink file.
func (b *Bundle) lstat(rel string) (fs.FileInfo, error) {
	return b.root.Lstat(rel)
}

func (b *Bundle) refuseSymlink(rel string) error {
	info, err := b.lstat(rel)
	switch {
	case err == nil:
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkTarget, rel)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrInvalidReference, rel)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return fmt.Errorf("%w: %w", ErrIO, err)
	}

	return nil
}

// ancestorsOK refuses a path whose existing ancestor is not a plain directory.
func (b *Bundle) ancestorsOK(rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	segs := strings.Split(dir, "/")
	for i := range segs {
		p := strings.Join(segs[:i+1], "/")
		info, err := b.lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrIO, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: directory %s", ErrSymlinkTarget, p)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w %q: %q exists and is not a directory", ErrInvalidCreationPath, rel, p)
		}
	}

	return nil
}

// bundleDir is the directory of an opened bundle.
func (w *Writer) bundleDir(b *Bundle) string { return filepath.Join(w.k.dir, b.Name) }

// Write runs one create, update or deprecate call.
func (w *Writer) Write(ctx context.Context, req WriteRequest) (*Receipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("okf_write: %w", err)
	}
	if err := refuseReserved(append(append(omapKeys(req.Fields), omapKeys(req.Set)...), req.Unset...)...); err != nil {
		return nil, err
	}
	switch req.Action {
	case WriteCreate, WriteUpdate, WriteDeprecate:
	default:
		return nil, fmt.Errorf("%w: unknown action %q (expected create, update or deprecate)", ErrInvalidWriteArgs, req.Action)
	}
	b, err := w.k.Open(req.Bundle)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	unlock := w.locks.Lock(w.bundleDir(b))
	defer unlock()

	now := w.now().UTC()
	pl := &writePlan{w: w, b: b, req: req, now: now}
	if req.Action == WriteCreate {
		err = pl.buildCreate()
	} else {
		err = pl.buildChange()
	}
	if err != nil {
		return nil, err
	}
	if pl.noop {
		return pl.noopReceipt(), nil
	}
	pl.plan()
	if err := pl.gate(); err != nil {
		return nil, err
	}

	return pl.commit(ctx)
}

// writePlan is the in-memory state of one call under the bundle lock.
type writePlan struct {
	w   *Writer
	b   *Bundle
	req WriteRequest
	now time.Time

	rel     string
	oldText string
	oldC    *Concept
	newText string
	newC    *Concept
	noop    bool

	deprecatedNow bool
	supersede     *logRef

	files []plannedFile // concept first, then indexes, then log
	fails []maintFailure
	found []Finding
	// unchanged lists the maintained files that needed no change.
	unchanged []string
	digest    string
}

func (p *writePlan) stamp(existing *keySpan) *omap {
	gen := newOmap()
	gen.set("by", "ready/"+p.w.version)
	gen.set("at", p.now.Format(time.RFC3339))
	if p.req.Model != "" {
		gen.set(keyModelExt, p.req.Model)
	}
	if existing != nil && existing.valueNod != nil && existing.valueNod.Kind == yaml.MappingNode {
		if old, ok := nodeToOrdered(existing.valueNod).(*omap); ok {
			for _, k := range old.keys {
				if k == "by" || k == "at" || k == keyModelExt {
					continue
				}
				gen.set(k, old.vals[k])
			}
		}
	}

	return gen
}

func (p *writePlan) buildCreate() error {
	req := p.req
	rel, err := creationPath(req.Path)
	if err != nil {
		return err
	}
	if req.Fields == nil {
		return fmt.Errorf("%w: create needs fields with a non-blank string type", ErrInvalidWriteArgs)
	}
	for _, k := range req.Fields.keys {
		if !validFieldKey(k) {
			return fmt.Errorf("%w: invalid field name %q", ErrInvalidWriteArgs, k)
		}
	}
	if t, ok := req.Fields.get(keyType); !ok {
		return fmt.Errorf("%w: fields must contain type", ErrInvalidWriteArgs)
	} else if s, isStr := t.(string); !isStr || strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: type must be a non-blank string", ErrInvalidWriteArgs)
	}
	if err := p.b.ancestorsOK(rel); err != nil {
		return err
	}
	if _, err := p.b.lstat(rel); err == nil {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, rel)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	var entries []keyEntry
	for _, k := range req.Fields.keys {
		v := req.Fields.vals[k]
		if v == nil {
			continue
		}
		entries = append(entries, keyEntry{k, v})
	}
	entries = append(entries, keyEntry{keyGenerated, p.stamp(nil)})
	p.rel = rel
	p.newText = newDocText(entries, normalizeNewBody(req.BodyOrEmpty()))

	return p.parseNew()
}

// BodyOrEmpty returns the body argument or "".
func (r WriteRequest) BodyOrEmpty() string {
	if r.Body == nil {
		return ""
	}

	return *r.Body
}

func (p *writePlan) parseNew() error {
	if len(p.newText) > MaxFileBytes {
		return fmt.Errorf("%w: %d bytes, the cap is %d", ErrOversize, len(p.newText), MaxFileBytes)
	}
	c, reason := p.b.conceptFromText(p.rel, p.newText, p.now)
	if c == nil {
		return fmt.Errorf("%w: the result is not a concept (%s)", ErrGateRefused, reason)
	}
	p.newC = c

	return nil
}

func (p *writePlan) supersededTarget(ref string) (string, *logRef, error) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "/")
	if err := ValidRelPath(ref); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrUnresolvedSuperseded, err)
	}
	c, err := p.b.LoadConcept(ref, p.now)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %q", ErrUnresolvedSuperseded, ref)
	}

	return "/" + c.Path, &logRef{Title: c.Title, Path: c.Path}, nil
}

func (p *writePlan) buildChange() error {
	req := p.req
	if req.Action == WriteUpdate && len(req.Set.orEmpty().keys) == 0 && len(req.Unset) == 0 && req.Body == nil {
		return fmt.Errorf("%w: update needs at least one of set, unset, body", ErrInvalidWriteArgs)
	}
	if req.BaseDigest == "" {
		return fmt.Errorf("%w: %s needs base_digest (the digest okf_query read printed)", ErrInvalidWriteArgs, req.Action)
	}
	rel, err := p.b.resolveFileArg(req.Path)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(rel, ".md") || isReserved(rel) {
		return fmt.Errorf("%w %q: not a concept", ErrInvalidReference, req.Path)
	}
	if err = p.b.refuseSymlink(rel); err != nil {
		return err
	}
	raw, err := p.b.readRaw(rel, nil)
	if err != nil {
		return p.b.readErr(rel, err)
	}
	if Digest(raw) != strings.ToLower(req.BaseDigest) {
		return fmt.Errorf("%w (current digest %s)", ErrConflict, Digest(raw))
	}
	p.rel, p.oldText, p.digest = rel, string(raw), Digest(raw)
	oldC, reason := p.b.conceptFromText(rel, p.oldText, p.now)
	if oldC == nil {
		return fmt.Errorf("%w %q: not a concept (%s)", ErrInvalidReference, req.Path, reason)
	}
	p.oldC = oldC
	d, err := parseDoc(p.oldText)
	if err != nil {
		return err
	}

	edits, body, err := p.buildEdits(d)
	if err != nil {
		return err
	}

	text1 := d.render(edits, body)
	if !meaningChanged(p.oldText, text1) {
		p.noop = true

		return nil
	}
	gen := p.stamp(d.span(keyGenerated))
	final := d.render(append(edits, keyEdit{key: keyGenerated, text: renderEntry(keyGenerated, gen, 0)}), body)
	p.newText = final

	if err := p.parseNew(); err != nil {
		return err
	}
	p.deprecatedNow = p.newC.Status == statusDeprecated && p.oldC.Status != statusDeprecated

	return nil
}

// buildEdits turns set, unset, body and the deprecation arguments into the
// key edits and the body of the candidate.
func (p *writePlan) buildEdits(d *fmDoc) ([]keyEdit, string, error) {
	req := p.req
	var edits []keyEdit
	setKeys := omapKeys(req.Set)
	for _, k := range setKeys {
		if !validFieldKey(k) {
			return nil, "", fmt.Errorf("%w: invalid field name %q", ErrInvalidWriteArgs, k)
		}
	}
	for _, k := range req.Unset {
		if !validFieldKey(k) {
			return nil, "", fmt.Errorf("%w: invalid field name %q", ErrInvalidWriteArgs, k)
		}
	}
	for _, k := range req.Unset {
		if k == keyType {
			return nil, "", ErrTypeProtected
		}
		edits = append(edits, keyEdit{key: k, remove: true})
	}
	for _, k := range setKeys {
		v := req.Set.vals[k]
		if k == keyType {
			if s, ok := v.(string); !ok || strings.TrimSpace(s) == "" {
				return nil, "", ErrTypeProtected
			}
		}
		if v == nil {
			edits = append(edits, keyEdit{key: k, remove: true})

			continue
		}
		edits = append(edits, keyEdit{key: k, text: renderEntry(k, v, 0)})
	}

	body := d.body
	if req.Body != nil {
		body = normalizeNewBody(*req.Body)
	}
	if req.Action == WriteDeprecate {
		edits = append(edits, keyEdit{key: keyStatus, text: renderEntry(keyStatus, statusDeprecated, 0)})
		if req.SupersededBy != "" {
			val, ref, err := p.supersededTarget(req.SupersededBy)
			if err != nil {
				return nil, "", err
			}
			p.supersede = ref
			edits = append(edits, keyEdit{key: keySupersededBy, text: renderEntry(keySupersededBy, val, 0)})
		}
		if r := strings.TrimSpace(req.Reason); r != "" {
			cur := strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
			if cur != "" {
				cur += "\n\n"
			}
			body = normalizeNewBody(cur + reasonPrefix + r)
		}
	}

	return edits, body, nil
}

func (m *omap) orEmpty() *omap {
	if m == nil {
		return newOmap()
	}

	return m
}

// meaningChanged implements the conservative meaning rule: any difference in
// a parsed frontmatter value other than generated, or in the normalized body.
func meaningChanged(oldText, newText string) bool {
	a, b := ParseFrontmatter(oldText), ParseFrontmatter(newText)
	ma, mb := withoutGenerated(a.Meta), withoutGenerated(b.Meta)
	if !reflect.DeepEqual(ma, mb) {
		return true
	}

	return normalizeBody(a.Body) != normalizeBody(b.Body)
}

func withoutGenerated(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != keyGenerated {
			out[k] = v
		}
	}

	return out
}

func (p *writePlan) noopReceipt() *Receipt {
	return &Receipt{
		Action: p.req.Action, Qualified: p.b.Name + "/" + p.oldC.ID, Result: resultNoop,
		Digest: p.digest, PrevDigest: p.digest,
		TierBefore: p.oldC.Tier, TierAfter: p.oldC.Tier,
		Notes: []string{"the result equals the current file after normalization: nothing written, no maintenance"},
	}
}

func (p *writePlan) readCurrent(rel string) (string, bool, error) {
	if err := p.b.refuseSymlink(rel); err != nil {
		return "", false, err
	}
	text, err := p.b.readText(rel, nil)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		var ri *readIssue
		if errors.As(err, &ri) {
			return "", false, fmt.Errorf("%s: %s", ri.Code, ri.Reason)
		}

		return "", false, fmt.Errorf("%w: %w", ErrIO, err)
	}

	return text, true, nil
}

// plan builds every candidate file: the concept, the index files, the log.
func (p *writePlan) plan() {
	existed := p.oldText != ""
	outcome := resultCreated
	if existed {
		outcome = resultUpdated
	}
	p.files = []plannedFile{{Rel: p.rel, Text: p.newText, Old: p.oldText, Existed: existed, Kind: kindConcept, Outcome: outcome}}
	idx, fails := planIndexes(maintInput{
		bundle: p.b.Name, conceptRel: p.rel, title: p.newC.Title, desc: p.newC.Desc,
		bundleTitle: p.req.BundleTitle, bundleDesc: p.req.BundleDescription, read: p.readCurrent,
	})
	p.fails = append(p.fails, fails...)
	for _, f := range idx {
		if f.Outcome != resultNoop {
			p.files = append(p.files, f)
		} else {
			p.unchanged = append(p.unchanged, f.Rel)
		}
	}

	kind := eventUpdate
	switch {
	case p.req.Action == WriteCreate:
		kind = eventCreation
	case p.req.Action == WriteDeprecate || p.deprecatedNow:
		kind = eventDeprecation
	}
	old, exists, err := p.readCurrent(fileLog)
	if err != nil {
		p.fails = append(p.fails, maintFailure{Rel: fileLog, Err: err})
	} else {
		ev := logEvent{Kind: kind, Title: p.newC.Title, Path: p.rel}
		if kind == eventDeprecation {
			ev.SupersedeBy = p.supersede
		}
		p.files = append(p.files, planLog(old, exists, ev, p.now))
	}
}

// gate runs the per-file conformance checks on every candidate and refuses
// the call when one gains an error-level finding or the concept is not one.
func (p *writePlan) gate() error {
	p.b.overlay = map[string]bool{}
	for _, f := range p.files {
		p.b.overlay[f.Rel] = true
		for d := path.Dir(f.Rel); d != "." && d != "/"; d = path.Dir(d) {
			p.b.overlay[d] = true
		}
	}
	defer func() { p.b.overlay = nil }()

	var refused []string
	for i := range p.files {
		f := &p.files[i]
		cand := p.b.checkText(f.Kind, f.Rel, f.Text, p.now)
		var oldErrs map[string]int
		if f.Existed {
			oldErrs = map[string]int{}
			for _, o := range p.b.checkText(f.Kind, f.Rel, f.Old, p.now) {
				if o.Level == LevelError {
					oldErrs[o.Code+"\x00"+o.Message]++
				}
			}
		}
		for _, c := range cand {
			if c.Level != LevelError {
				continue
			}
			key := c.Code + "\x00" + c.Message
			if oldErrs[key] > 0 {
				oldErrs[key]--

				continue
			}
			refused = append(refused, fmt.Sprintf("%s: %s %s", f.Rel, c.Code, c.Message))
		}
		if f.Kind == kindConcept {
			p.receiptFindings(cand)
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%w: %s", ErrGateRefused, strings.Join(refused, "; "))
	}
	// W/I findings of the touched index and log files are reported too.
	for i := range p.files {
		if f := p.files[i]; f.Kind != kindConcept {
			p.receiptFindings(p.b.checkText(f.Kind, f.Rel, f.Text, p.now))
		}
	}

	return nil
}

func (p *writePlan) receiptFindings(fs []Finding) {
	for _, f := range fs {
		if f.Level != LevelError {
			p.addFinding(f)
		}
	}
}

func (p *writePlan) addFinding(f Finding) { p.found = append(p.found, f) }

// checkText runs the per-file checks of one candidate text.
func (b *Bundle) checkText(kind, rel, text string, now time.Time) []Finding {
	ck := &checker{b: b, now: now}
	switch kind {
	case kindIndex:
		ck.checkIndex(rel, text)
	case kindLog:
		ck.checkLog(rel, text)
	default:
		ck.checkMarkdown(rel, text)
	}

	return ck.findings
}

func randHex() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])

	return hex.EncodeToString(buf[:])
}

// atomicWrite writes text to rel through the bundle root: hidden temp file in
// the target directory, fsync, rename. Existing files keep their mode; new
// files are 0600 and new directories 0700. A symlink target is refused.
func (b *Bundle) atomicWrite(rel, text string) error {
	if err := b.refuseSymlink(rel); err != nil {
		return err
	}
	if err := b.ancestorsOK(rel); err != nil {
		return err
	}
	dir := path.Dir(rel)
	if dir != "." {
		segs := strings.Split(dir, "/")
		for i := range segs {
			p := strings.Join(segs[:i+1], "/")
			if err := b.root.Mkdir(p, modeDir); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("%w: %w", ErrIO, err)
			}
		}
	}
	mode := fs.FileMode(modeFile)
	if info, err := b.lstat(rel); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path.Join(dir, tmpPrefix+randHex()+tmpSuffix)
	f, err := b.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	cleanup := func() { _ = b.root.Remove(tmp) }
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		cleanup()

		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	_ = f.Chmod(mode)
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()

		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	if err := f.Close(); err != nil {
		cleanup()

		return fmt.Errorf("%w: %w", ErrIO, err)
	}
	if err := b.root.Rename(tmp, rel); err != nil {
		cleanup()

		return fmt.Errorf("%w: %w", ErrIO, err)
	}

	return nil
}

// commit writes the files in order and builds the receipt.
func (p *writePlan) commit(ctx context.Context) (*Receipt, error) {
	r := &Receipt{Action: p.req.Action, Qualified: p.b.Name + "/" + p.newC.ID, Result: resultUpdated, Findings: p.found}
	switch p.req.Action {
	case WriteCreate:
		r.Result = resultCreated
	case WriteDeprecate:
		r.Result = resultDeprec
	}
	for i, f := range p.files {
		if err := ctx.Err(); err != nil && i > 0 {
			r.Maintenance = append(r.Maintenance, MaintOutcome{Rel: f.Rel, Outcome: outcomeFailed, Err: err.Error()})

			continue
		}
		err := p.b.atomicWrite(f.Rel, f.Text)
		if err == nil {
			if i > 0 {
				r.Maintenance = append(r.Maintenance, MaintOutcome{Rel: f.Rel, Outcome: f.Outcome})
			}

			continue
		}
		if i == 0 {
			return nil, err
		}
		p.w.log.Warn().Err(err).Str("bundle", p.b.Name).Str("file", f.Rel).Msg("okf maintenance write failed")
		r.Maintenance = append(r.Maintenance, MaintOutcome{Rel: f.Rel, Outcome: outcomeFailed, Err: err.Error()})
	}
	for _, f := range p.fails {
		p.w.log.Warn().Err(f.Err).Str("bundle", p.b.Name).Str("file", f.Rel).Msg("okf maintenance planning failed")
		r.Maintenance = append(r.Maintenance, MaintOutcome{Rel: f.Rel, Outcome: outcomeFailed, Err: f.Err.Error()})
	}
	for _, rel := range p.unchanged {
		r.Maintenance = append(r.Maintenance, MaintOutcome{Rel: rel, Outcome: resultNoop})
	}
	r.Digest = Digest([]byte(p.newText))
	r.PrevDigest = p.digest
	r.GenBy = "ready/" + p.w.version
	r.GenAt = p.now.Format(time.RFC3339)
	r.GenModel = p.req.Model
	r.TierAfter = p.newC.Tier
	r.TierBefore = TierNone
	before := 0
	if p.oldC != nil {
		r.TierBefore, before = p.oldC.Tier, p.oldC.VerifiedCurrent
	}
	if n := before - p.newC.VerifiedCurrent; n > 0 {
		r.NoLongerCurrent = n
	}
	r.TierDropped = r.TierAfter.rank() < r.TierBefore.rank()
	if p.oldC != nil {
		if diff, ok := unifiedDiff(p.oldC.Body, p.newC.Body, MaxDiffBytes); ok {
			r.Diff = diff
		} else {
			r.DiffOmitted = true
		}
	}
	if b2, err := p.w.k.Open(p.b.Name); err == nil {
		if res, verr := b2.Validate(ctx, p.now); verr == nil {
			r.Errors, r.Warnings, r.Infos = res.Errors, res.Warnings, res.Infos
		}
		b2.Close()
	}

	return r, nil
}

// String renders the receipt in its fixed order.
func (r *Receipt) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "okf_write %s %s: %s\n", r.Action, r.Qualified, r.Result)
	prev := r.PrevDigest
	if prev == "" {
		prev = "none"
	}
	fmt.Fprintf(&sb, "Digest: %s (previous: %s)\n", r.Digest, prev)
	if r.GenBy != "" {
		model := r.GenModel
		if model == "" {
			model = "unavailable"
		}
		fmt.Fprintf(&sb, "Generated: by %s at %s, model %s\n", r.GenBy, r.GenAt, model)
	}
	fmt.Fprintf(&sb, "Trust: %s -> %s; verifications no longer current: %d\n", r.TierBefore, r.TierAfter, r.NoLongerCurrent)
	if r.TierDropped || r.NoLongerCurrent > 0 {
		fmt.Fprintf(&sb, "The concept needs a new verification: ask the human to run /verify %s\n", r.Qualified)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&sb, "Note: %s\n", n)
	}
	if len(r.Maintenance) > 0 {
		sb.WriteString("Maintenance:\n")
		for _, m := range r.Maintenance {
			if m.Err != "" {
				fmt.Fprintf(&sb, "  %s: failed (%s)\n", m.Rel, m.Err)
			} else {
				fmt.Fprintf(&sb, "  %s: %s\n", m.Rel, m.Outcome)
			}
		}
	}
	switch {
	case r.DiffOmitted:
		sb.WriteString("Body diff omitted: the bodies are too large to diff.\n")
	case r.Diff != "":
		sb.WriteString("Body diff:\n" + r.Diff)
	case r.Action != WriteCreate && r.Result != resultNoop:
		sb.WriteString("Body diff: no body change.\n")
	}
	if len(r.Findings) > 0 {
		sb.WriteString("Findings of the touched files:\n")
		shown := r.Findings
		if len(shown) > MaxFindings {
			shown = shown[:MaxFindings]
		}
		for _, f := range shown {
			loc := f.Path
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
			}
			fmt.Fprintf(&sb, "  %s: %s %s %s\n", loc, f.Level, f.Code, f.Message)
		}
	}
	if r.Result != resultNoop {
		fmt.Fprintf(&sb, "Bundle findings: %d errors, %d warnings, %d info\n", r.Errors, r.Warnings, r.Infos)
	}

	return sb.String()
}

// ── human verification ──────────────────────────────────────────────────

// ParseConceptRef splits "<bundle>/<concept>" (percent-decoded, trailing .md
// optional) into the bundle name and the concept id.
func ParseConceptRef(raw string) (bundle, id string, err error) {
	dec, derr := url.PathUnescape(raw)
	if derr != nil {
		return "", "", fmt.Errorf("%w %q: %w", ErrInvalidReference, raw, derr)
	}
	name, rest, ok := strings.Cut(dec, "/")
	if !ok || name == "" || rest == "" || strings.HasPrefix(name, ".") {
		return "", "", fmt.Errorf("%w %q: expected <bundle>/<concept>", ErrInvalidReference, raw)
	}
	rest = strings.TrimSuffix(rest, ".md")
	if verr := ValidRelPath(rest); verr != nil {
		return "", "", verr
	}

	return name, rest, nil
}

// ValidHumanID reports whether id satisfies the verifier identity grammar.
func ValidHumanID(id string) bool { return humanIDRE.MatchString(id) }

// ValidToken reports whether t is 16 hexadecimal characters.
func ValidToken(t string) bool {
	if len(t) != 16 {
		return false
	}
	_, err := hex.DecodeString(t)

	return err == nil
}

// MaxPreviewBytes bounds the raw content of a /verify preview.
const MaxPreviewBytes = 8 << 10

// VerifyPreview is the data of a /verify preview.
type VerifyPreview struct {
	Qualified string
	Type      string
	Tier      Tier
	Status    string
	GenBy     string
	GenAt     string
	Events    int
	Latest    string
	Content   string
	Size      int
	Truncated bool
	Token     string
}

func (w *Writer) openConcept(b *Bundle, id string, now time.Time) (rel string, raw []byte, c *Concept, err error) {
	rel, err = b.resolveFileArg(id)
	if err != nil {
		return "", nil, nil, err
	}
	if !strings.HasSuffix(rel, ".md") || isReserved(rel) {
		return "", nil, nil, fmt.Errorf("%w %q: not a concept", ErrInvalidReference, id)
	}
	if err = b.refuseSymlink(rel); err != nil {
		return "", nil, nil, err
	}
	raw, err = b.readRaw(rel, nil)
	if err != nil {
		return "", nil, nil, b.readErr(rel, err)
	}
	c, reason := b.conceptFromText(rel, string(raw), now)
	if c == nil {
		return "", nil, nil, fmt.Errorf("%w %q: not a concept (%s)", ErrInvalidReference, id, reason)
	}

	return rel, raw, c, nil
}

// VerifyPreview returns the raw content, trust summary and token of a
// concept. It records nothing.
func (w *Writer) VerifyPreview(ctx context.Context, bundle, id string) (*VerifyPreview, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	b, err := w.k.Open(bundle)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	now := w.now().UTC()
	rel, raw, c, err := w.openConcept(b, id, now)
	if err != nil {
		return nil, err
	}
	p := &VerifyPreview{
		Qualified: b.Name + "/" + c.ID, Type: c.Type, Tier: c.Tier, Status: c.Status,
		Events: c.VerifiedTotal, Size: len(raw), Token: VerifyToken(b.Name, strings.TrimSuffix(rel, ".md"), raw),
	}
	if m, ok := asMap(c.Meta[keyGenerated]); ok {
		p.GenBy, p.GenAt = str(m["by"]), str(m["at"])
		if blank(m["by"]) {
			p.GenBy = ""
		}
		if blank(m["at"]) {
			p.GenAt = ""
		}
	}
	list := asList(c.Meta[keyVerified])
	for i := len(list) - 1; i >= 0; i-- {
		if m, ok := asMap(list[i]); ok && !blank(m["by"]) {
			p.Latest = fmt.Sprintf("%s at %s", str(m["by"]), str(m["at"]))

			break
		}
	}
	text := string(raw)
	cut, _, next := sliceUTF8(text, 0, MaxPreviewBytes)
	p.Content, p.Truncated = cut, next > 0

	return p, nil
}

// VerifyResult is the outcome of a recorded verification.
type VerifyResult struct {
	Qualified  string
	HumanID    string
	Tier       Tier
	NotCurrent bool
	LogFailed  error
}

// VerifyConfirm records a human verification when token still matches the
// current bytes. via is the logical channel name stored as x-ready-via.
func (w *Writer) VerifyConfirm(ctx context.Context, bundle, id, humanID, token, via string) (*VerifyResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if !ValidHumanID(humanID) || !ValidToken(token) {
		return nil, fmt.Errorf("%w: identity or token malformed", ErrInvalidWriteArgs)
	}
	b, err := w.k.Open(bundle)
	if err != nil {
		return nil, err
	}
	defer b.Close()
	unlock := w.locks.Lock(w.bundleDir(b))
	defer unlock()

	now := w.now().UTC()
	rel, raw, _, err := w.openConcept(b, id, now)
	if err != nil {
		return nil, err
	}
	if !tokenEqual(VerifyToken(b.Name, strings.TrimSuffix(rel, ".md"), raw), token) {
		return nil, ErrChangedSincePreview
	}
	d, err := parseDoc(string(raw))
	if err != nil {
		return nil, err
	}
	event := newOmap()
	event.set("by", "human:"+humanID)
	event.set("at", now.Format(time.RFC3339))
	event.set(keyViaExt, via)
	edit, err := verifiedEdit(d, event)
	if err != nil {
		return nil, err
	}
	text := d.render([]keyEdit{edit}, d.body)
	if len(text) > MaxFileBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrOversize, len(text))
	}
	newC, reason := b.conceptFromText(rel, text, now)
	if newC == nil {
		return nil, fmt.Errorf("%w: the result is not a concept (%s)", ErrGateRefused, reason)
	}
	files := []plannedFile{{Rel: rel, Text: text, Old: string(raw), Existed: true, Kind: kindConcept, Outcome: resultUpdated}}
	oldLog, logExists, lerr := (&writePlan{b: b}).readCurrent(fileLog)
	var logFailed error
	if lerr != nil {
		logFailed = lerr
	} else {
		files = append(files, planLog(oldLog, logExists, logEvent{Kind: eventVerification, Title: newC.Title, Path: rel, Human: humanID}, now))
	}
	pl := &writePlan{w: w, b: b, now: now, files: files}
	if err := pl.gate(); err != nil {
		return nil, err
	}
	for i, f := range files {
		if err := b.atomicWrite(f.Rel, f.Text); err != nil {
			if i == 0 {
				return nil, err
			}
			logFailed = err
			w.log.Warn().Err(err).Str("bundle", b.Name).Msg("okf verification log entry failed")
		}
	}
	res := &VerifyResult{Qualified: b.Name + "/" + newC.ID, HumanID: humanID, Tier: newC.Tier, LogFailed: logFailed}
	res.NotCurrent = newC.ChangeTime != nil && now.Before(*newC.ChangeTime)

	return res, nil
}

// verifiedEdit builds the edit that appends event to the verified key.
func verifiedEdit(d *fmDoc, event *omap) (keyEdit, error) {
	flow, _ := renderFlow(event)
	sp := d.span(keyVerified)
	if sp == nil {
		return keyEdit{key: keyVerified, text: "verified:\n  - " + flow + "\n"}, nil
	}
	n := sp.valueNod
	switch {
	case n.Kind == yaml.ScalarNode && (n.ShortTag() == "!!null" || n.Value == ""):
		return keyEdit{key: keyVerified, text: "verified:\n  - " + flow + "\n"}, nil
	case n.Kind == yaml.ScalarNode:
		return keyEdit{}, fmt.Errorf("%w: verified is a scalar", ErrUnmergeable)
	case n.Kind == yaml.SequenceNode && len(n.Content) == 0:
		return keyEdit{key: keyVerified, text: "verified:\n  - " + flow + "\n"}, nil
	case n.Kind == yaml.SequenceNode && n.Style&yaml.FlowStyle == 0:
		indent := ""
		for _, l := range d.lines[sp.keyLine+1 : sp.end] {
			if t := strings.TrimLeft(l, " \t"); strings.HasPrefix(t, "-") {
				indent = l[:len(l)-len(t)]

				break
			}
		}

		return keyEdit{key: keyVerified, appendLines: indent + "- " + flow + "\n"}, nil
	}
	// A single mapping or a flow sequence: re-render the key as a block
	// sequence keeping every existing entry.
	var items []any
	if n.Kind == yaml.MappingNode {
		items = []any{nodeToOrdered(n)}
	} else if l, ok := nodeToOrdered(n).([]any); ok {
		items = l
	}
	items = append(items, event)

	return keyEdit{key: keyVerified, text: "verified:\n" + renderSeqItems(items, 2)}, nil
}

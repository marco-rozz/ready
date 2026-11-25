package okf

import (
	"strings"
	"time"
)

// Tier is the OKF trust tier (SPEC 5.3). It is advice for the reader, never
// access control.
type Tier string

// Trust tiers, highest first.
const (
	TierHuman   Tier = "human-reviewed"
	TierMachine Tier = "machine-confirmed"
	TierNone    Tier = "unverified"
)

func (t Tier) rank() int {
	switch t {
	case TierHuman:
		return 2
	case TierMachine:
		return 1
	}

	return 0
}

// MalformedGeneratedNote is the fixed note of the conservative trust rule.
const MalformedGeneratedNote = "generated.at malformed: verification currency cannot be established"

// Concept is a parsed OKF concept with its computed trust state.
type Concept struct {
	Bundle string
	ID     string // bundle-relative path without .md
	Path   string // bundle-relative path
	Type   string
	Title  string
	Desc   string
	Tags   []string
	Meta   map[string]any
	Text   string // whole file text (BOM stripped)
	Body   string
	// BodyLine is the 1-based file line of the first body line.
	BodyLine int

	Status       string // "stable" when absent, otherwise the raw value
	Current      bool
	SupersededBy string // raw value, "" when absent
	Replacement  string // resolved concept id of superseded_by, "" when none

	Tier                 Tier
	TierNotes            []string
	VerifiedBeforeChange bool
	// VerifiedTotal counts the well-formed verification events; VerifiedCurrent
	// those not older than the last change.
	VerifiedTotal, VerifiedCurrent int
	ChangeTime                     *time.Time
	ChangeRaw                      string
	ChangeActor                    string

	Stale          bool
	StaleAfter     string
	StaleMalformed bool

	Notes []string
	// DuplicateKey carries the repeated key when the frontmatter had one
	// (the concept is still read with last-wins values).
	DuplicateKey string
}

// Qualified returns "<bundle>/<id>".
func (c *Concept) Qualified() string { return c.Bundle + "/" + c.ID }

// Freshness renders exactly one of the four section 5.5 forms.
func (c *Concept) Freshness() string {
	switch {
	case c.StaleMalformed:
		return "stale (malformed stale_after)"
	case c.Stale:
		return "stale since " + c.StaleAfter
	case c.StaleAfter != "":
		return "fresh until " + c.StaleAfter
	}

	return "no stale_after"
}

// notConcept explains why a Markdown file is not a concept.
const (
	reasonNoFrontmatter = "no frontmatter"
	reasonNoType        = "no type"
	reasonNotMapping    = "frontmatter is not a mapping"
	reasonNotClosed     = "frontmatter is not closed"
	reasonNotParse      = "frontmatter does not parse"
)

// conceptFromText builds the concept for a bundle-relative path. When the file
// is not a concept the reason is returned instead.
func (b *Bundle) conceptFromText(rel, text string, now time.Time) (*Concept, string) {
	fm := ParseFrontmatter(text)
	switch fm.State {
	case FMAbsent:
		return nil, reasonNoFrontmatter
	case FMInvalid:
		return nil, reasonNotParse
	case FMNotMapping:
		return nil, reasonNotMapping
	case FMUnclosed:
		return nil, reasonNotClosed
	case FMOK, FMDuplicate:
	}
	typ, ok := fm.Meta[keyType].(string)
	if !ok || strings.TrimSpace(typ) == "" {
		return nil, reasonNoType
	}
	c := &Concept{
		Bundle: b.Name, Path: rel, ID: strings.TrimSuffix(rel, ".md"), Type: typ,
		Meta: fm.Meta, Text: strings.TrimLeft(text, bom), Body: fm.Body, BodyLine: fm.BodyLine,
		DuplicateKey: fm.DupKey,
	}
	b.describe(c, now)

	return c, ""
}

// describe computes title, tags, status, trust tier and freshness.
func (b *Bundle) describe(c *Concept, now time.Time) {
	meta := c.Meta
	stem := c.ID
	if i := strings.LastIndexByte(stem, '/'); i >= 0 {
		stem = stem[i+1:]
	}
	if blank(meta["title"]) {
		c.Title = strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(stem))
	} else {
		c.Title = str(meta["title"])
	}
	if !blank(meta["description"]) {
		c.Desc = str(meta["description"])
	}
	c.Tags = asTags(meta["tags"])

	b.computeTrust(c)

	// Status (5.4).
	if blank(meta["status"]) {
		c.Status = statusStable
	} else {
		c.Status = str(meta["status"])
	}
	c.Current = c.Status == statusStable || c.Status == statusDraft
	switch c.Status {
	case statusStable, statusDraft, statusDeprecated:
	default:
		c.Notes = append(c.Notes, "unknown status "+pyRepr(c.Status)+": not current")
	}
	if !blank(meta["superseded_by"]) {
		c.SupersededBy = str(meta["superseded_by"])
	}
	if c.Status == statusDeprecated {
		if id, ok := b.resolveConceptID(c.Path, meta["superseded_by"]); ok {
			c.Replacement = id
		}
	}

	// Freshness (5.5).
	if !blank(meta["stale_after"]) {
		c.StaleAfter = str(meta["stale_after"])
		if t, ok := parseTimestamp(meta["stale_after"]); ok {
			c.Stale = !now.Before(t)
		} else {
			c.StaleMalformed, c.Stale = true, true
			c.Notes = append(c.Notes, "stale_after "+pyRepr(c.StaleAfter)+" is malformed, so the concept counts as stale")
		}
	}
	if c.VerifiedBeforeChange {
		c.Notes = append(c.Notes, "verified only before the last change")
	}
	c.Notes = append(c.Notes, c.TierNotes...)
}

// computeTrust implements the tier rules including the conservative treatment
// of a malformed generated block and the older-bundle timestamp fallback.
func (b *Bundle) computeTrust(c *Concept) {
	meta := c.Meta
	gen := meta["generated"]
	var changeRaw any
	malformed := false
	if m, ok := asMap(gen); ok {
		if !blank(m["at"]) {
			changeRaw = m["at"]
		} else if b.fallback && !blank(meta["timestamp"]) {
			changeRaw = meta["timestamp"]
		}
		if !blank(m["by"]) {
			c.ChangeActor = str(m["by"])
		}
	} else if blank(gen) {
		if b.fallback && !blank(meta["timestamp"]) {
			changeRaw = meta["timestamp"]
		}
	} else {
		malformed = true
	}
	if changeRaw != nil {
		c.ChangeRaw = str(changeRaw)
		if t, ok := parseTimestamp(changeRaw); ok {
			c.ChangeTime = &t
		} else {
			malformed = true
		}
	}
	if malformed {
		c.Tier = TierNone
		c.TierNotes = []string{MalformedGeneratedNote}
		c.ChangeTime = nil

		return
	}

	var events, current []verifiedEvent
	for _, e := range asList(meta["verified"]) {
		m, ok := asMap(e)
		if !ok || blank(m["by"]) {
			continue
		}
		at, ok := parseTimestamp(m["at"])
		if !ok {
			continue
		}
		ev := verifiedEvent{By: str(m["by"]), At: at}
		events = append(events, ev)
		if c.ChangeTime == nil || !at.Before(*c.ChangeTime) {
			current = append(current, ev)
		}
	}
	c.VerifiedTotal, c.VerifiedCurrent = len(events), len(current)
	c.VerifiedBeforeChange = len(events) > 0 && len(current) == 0
	switch {
	case len(current) == 0:
		c.Tier = TierNone
	default:
		c.Tier = TierMachine
		for _, e := range current {
			if strings.HasPrefix(e.By, "human:") {
				c.Tier = TierHuman

				break
			}
		}
	}
}

type verifiedEvent struct {
	By string
	At time.Time
}

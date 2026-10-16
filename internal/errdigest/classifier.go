// Package errdigest classifies errors into user-facing severity levels.
// It has zero dependencies on internal/mcp; callers register sentinel→severity
// pairs at wiring time (cmd/main.go) via a fluent Register chain.
package errdigest

import "errors"

// Severity indicates how serious a tool/MCP failure is from the user's perspective.
type Severity string

const (
	// Transient errors are temporary and the user can retry.
	Transient Severity = "TRANSIENT"
	// Permanent errors require a corrective action by the user.
	Permanent Severity = "PERMANENT"
	// Unknown errors are unclassified.
	Unknown Severity = "UNKNOWN"
)

// Classifier maps registered sentinel errors to severity levels via errors.Is.
type Classifier struct {
	entries []entry
}

type entry struct {
	sentinel error
	severity Severity
}

// NewClassifier returns an empty Classifier. Chain Register calls to populate it.
func NewClassifier() *Classifier {
	return &Classifier{}
}

// Register adds a mapping from sentinel to severity and returns the Classifier
// for method chaining.
func (c *Classifier) Register(sentinel error, severity Severity) *Classifier {
	c.entries = append(c.entries, entry{sentinel: sentinel, severity: severity})

	return c
}

// Classify walks registered entries in order and returns the severity of the
// first sentinel that matches err via errors.Is. Returns Unknown if no match.
func (c *Classifier) Classify(err error) Severity {
	if err == nil {
		return Unknown
	}

	for _, e := range c.entries {
		if errors.Is(err, e.sentinel) {
			return e.severity
		}
	}

	return Unknown
}

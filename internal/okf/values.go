// Package okf implements the read side of the Open Knowledge Format (OKF)
// v0.3: bundle discovery, frontmatter parsing, trust/status/freshness
// computation, path and citation resolution, conflict ordering and a
// conformance validator, plus the read-only okf_query tool.
//
// The normative reference is OKF SPEC v0.3 and the consume skill's trust.md,
// pinned under testdata/reference; the reference Python scripts are test
// oracles and every deliberate divergence is listed in the capability spec.
package okf

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Shared literals (status vocabulary, reserved file names, frontmatter keys,
// tool actions).
const (
	statusStable     = "stable"
	statusDraft      = "draft"
	statusDeprecated = "deprecated"

	fileIndex = "index.md"
	fileLog   = "log.md"

	keyType         = "type"
	keyStatus       = "status"
	keySupersededBy = "superseded_by"
	keyAttester     = "attester"
	keyExecutor     = "executor"

	typeAttestedComputation = "Attested Computation"
	typeNameList            = "list"

	actionList     = "list"
	actionIndex    = "index"
	actionScan     = "scan"
	actionRead     = "read"
	actionCompare  = "compare"
	actionValidate = "validate"

	timeFormat = time.RFC3339
)

var (
	datetimeRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`)
	dateRE     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// blank implements OKF 11.2: null, an empty string and an empty list mean an
// absent key.
func blank(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}

	return false
}

// asList implements OKF 11.2: one item stands for a one-item list.
func asList(v any) []any {
	if blank(v) {
		return nil
	}
	if l, ok := v.([]any); ok {
		return l
	}

	return []any{v}
}

// asTags implements OKF 11.2: a scalar tags value is one tag, never split.
func asTags(v any) []string {
	var tags []string
	for _, t := range asList(v) {
		if !blank(t) {
			tags = append(tags, str(t))
		}
	}

	return tags
}

// str renders a frontmatter value as text (Python str() for scalars).
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}

		return "False"
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}

		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = pyRepr(k) + ": " + pyRepr(x[k])
		}

		return "{" + strings.Join(parts, ", ") + "}"
	}

	return fmt.Sprint(v)
}

// pyRepr is the repr()-like rendering used inside messages.
func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + s + "'"
	}

	return str(v)
}

// typeName names a frontmatter value's type the way the reference does.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case string:
		return "str"
	case bool:
		return "bool"
	case int64, int:
		return "int"
	case float64:
		return "float"
	case []any:
		return typeNameList
	case map[string]any:
		return "dict"
	}

	return fmt.Sprintf("%T", v)
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)

	return m, ok
}

// parseTimestamp parses the two RFC 3339 forms of OKF section 5: a date-time
// with an offset, or a date (start of that day in UTC). Anything else, a
// non-string included, is unparseable.
func parseTimestamp(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	switch {
	case dateRE.MatchString(s):
		t, err := time.Parse("2006-01-02", s)

		return t.UTC(), err == nil
	case datetimeRE.MatchString(s):
		t, err := time.Parse(time.RFC3339Nano, strings.ToUpper(s))

		return t, err == nil
	}

	return time.Time{}, false
}

// isDateOnly reports whether the raw value is a bare date.
func isDateOnly(v any) bool { return dateRE.MatchString(str(v)) }

// versionTuple parses "0.3" style versions; ok is false when unparseable.
func versionTuple(s string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}

	return out, true
}

// versionAtMost reports whether a is <= b using tuple ordering.
func versionAtMost(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}

	return len(a) <= len(b)
}

// fallbackEligible implements the older-version fallbacks (OKF 12.1): a
// bundle declaring 0.2 or earlier, no version, or an unparseable version.
func fallbackEligible(version string) bool {
	if version == "" {
		return true
	}
	t, ok := versionTuple(version)
	if !ok {
		return true
	}

	return versionAtMost(t, []int{0, 2})
}

func containsSpace(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
}

package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteSize is a byte count parsed from a human-readable YAML scalar such as
// `10MB`, `512KB`, `1GB`, `100B`, or a bare integer (bytes). Units are
// case-insensitive and base 1024. Negative values, fractions, unknown units
// and values overflowing int64 are rejected at parse time.
type ByteSize int64

// Guarantees at compile time that ByteSize implements yaml.Unmarshaler.
var _ yaml.Unmarshaler = (*ByteSize)(nil)

// errInvalidByteSize is the sentinel wrapped by every ParseByteSize failure.
var errInvalidByteSize = errors.New("invalid byte size")

// byteUnit is one accepted unit suffix (upper-cased) and its multiplier.
type byteUnit struct {
	suffix string
	mult   int64
}

// byteUnits lists the accepted units, longest suffix first so "KB" is matched
// before "B".
func byteUnits() []byteUnit {
	return []byteUnit{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}
}

// ParseByteSize parses a human-readable byte size (see ByteSize).
func ParseByteSize(s string) (ByteSize, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("%w: empty value", errInvalidByteSize)
	}

	upper := strings.ToUpper(raw)
	num, mult := upper, int64(1)

	for _, u := range byteUnits() {
		if strings.HasSuffix(upper, u.suffix) {
			num, mult = strings.TrimSpace(strings.TrimSuffix(upper, u.suffix)), u.mult

			break
		}
	}

	// Only plain decimal digits: rejects signs, fractions, exponents and
	// unknown units in one check.
	if num == "" || strings.TrimLeft(num, "0123456789") != "" {
		return 0, fmt.Errorf("%w: %q (use a non-negative integer with optional B, KB, MB or GB)", errInvalidByteSize, s)
	}

	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q overflows", errInvalidByteSize, s)
	}

	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("%w: %q overflows", errInvalidByteSize, s)
	}

	return ByteSize(n * mult), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("%w: expected a scalar at line %d", errInvalidByteSize, node.Line)
	}

	v, err := ParseByteSize(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}

	*b = v

	return nil
}

// String renders the size with the largest exact unit (e.g. 10MB, 1536KB).
func (b ByteSize) String() string {
	for _, u := range byteUnits() {
		if u.mult > 1 && int64(b) != 0 && int64(b)%u.mult == 0 {
			return strconv.FormatInt(int64(b)/u.mult, 10) + u.suffix
		}
	}

	return strconv.FormatInt(int64(b), 10) + "B"
}

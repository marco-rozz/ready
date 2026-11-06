package secretstore

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// FieldZMQAuthToken is the dotted config path of the ZMQ shared secret.
const FieldZMQAuthToken = "channels.zmqcli.auth_token" //nolint:gosec // a configuration path, not a credential

// MinZMQSecretLen is the minimum length of the ZMQ shared secret, in characters.
const MinZMQSecretLen = 32

// Rule codes carried by a RuleViolation.
const (
	RuleEmpty    = "empty"
	RuleTooShort = "too_short"
)

// RuleViolation reports a value that breaks a field's rule. It names the field
// and the rule code only: the value is never part of an error.
type RuleViolation struct {
	Field string
	Rule  string
}

func (v *RuleViolation) Error() string {
	switch v.Rule {
	case RuleTooShort:
		return fmt.Sprintf("%s: value must be at least %d characters (%s)", v.Field, MinZMQSecretLen, v.Rule)
	default:
		return fmt.Sprintf("%s: value must not be empty (%s)", v.Field, v.Rule)
	}
}

// CheckValue applies the value rules over the union of fields that reference
// one Secret name. Values are judged exactly as entered (no trimming).
func CheckValue(fields []string, value string) error {
	if value == "" {
		field := ""
		if len(fields) > 0 {
			field = fields[0]
		}

		return &RuleViolation{Field: field, Rule: RuleEmpty}
	}
	for _, f := range fields {
		if f == FieldZMQAuthToken && utf8.RuneCountInString(value) < MinZMQSecretLen {
			return &RuleViolation{Field: f, Rule: RuleTooShort}
		}
	}

	return nil
}

// RuleCode extracts the rule code of a violation, or "" for another error.
func RuleCode(err error) string {
	if v, ok := errors.AsType[*RuleViolation](err); ok {
		return v.Rule
	}

	return ""
}

// NormalizeFileSecret turns the bytes of a `!secret_file` into its value:
// byte for byte, except that exactly one trailing "\n" or "\r\n" is removed
// (the artefact of `echo value > file`). Nothing else is touched.
func NormalizeFileSecret(raw []byte) string {
	s := string(raw)
	if t, ok := strings.CutSuffix(s, "\r\n"); ok {
		return t
	}

	return strings.TrimSuffix(s, "\n")
}

// ReadFileSecret reads a `!secret_file` target. A missing file, or one empty
// after normalisation, yields ok=false (pending external), never an error.
func ReadFileSecret(path string) (value string, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to read secret file: %w", err)
	}
	v := NormalizeFileSecret(raw)

	return v, v != "", nil
}

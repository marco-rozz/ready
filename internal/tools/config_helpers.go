package tools

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/marco-rozz/ready/internal/config"
)

// randomHexID yields a fresh random identifier for staging allocations.
func randomHexID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand never fails on supported platforms; keep a
		// deterministic high-entropy fallback rather than panic.
		return "fallback-" + hex.EncodeToString(buf[:])
	}

	return hex.EncodeToString(buf[:])
}

// jsonMarshal renders a tool result safely.
func jsonMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Results are plain structs; marshal failure is a programming bug.
		return `{"error":"internal result serialization failed"}`
	}

	return string(b)
}

// bytesEqual compares raw byte snapshots.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// isTransientCommit reports positively not-committed, retryable failures.
func isTransientCommit(err error) bool {
	return errors.Is(err, config.ErrCommitTransient) || errors.Is(err, config.ErrCommitPostCommit)
}

// wrapUncommittedIO wraps a read/verification I/O failure before any commit
// artifact as positively not-committed (retry safe).
func wrapUncommittedIO(what string, err error) error {
	return fmt.Errorf("%s: %w: %w", what, config.ErrCommitTransient, err)
}

// wrapCensusRefusal renders a read-time failure without any document
// content: unsupported constructs and unmaskable literals are refused closed,
// never partially disclosed.
func wrapCensusRefusal(err error) error {
	return fmt.Errorf("the elected configuration cannot be opened for editing: %w", err)
}

// wrapBatchRefusal renders an edit/candidate failure as a Permanent refusal
// naming the field and safe guidance (never rejected edit text).
func wrapBatchRefusal(err error) error {
	return fmt.Errorf("update refused: %w", err)
}

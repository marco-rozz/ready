package formatter

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
)

// envelopeFallbackCounter is used only if crypto/rand fails, to keep the
// daemon alive while still emitting unique IDs. In practice crypto/rand does
// not fail on modern operating systems.
//
//nolint:gochecknoglobals // singleton monotonic counter used only on unrecoverable RNG failure
var envelopeFallbackCounter uint64

// NewTurnID returns a fresh per-turn identifier: 4 bytes of cryptographic
// randomness rendered as 8 lowercase hexadecimal characters. On unrecoverable
// RNG failure it falls back to a monotonic counter so callers always receive
// a non-empty, unique string.
func NewTurnID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := atomic.AddUint64(&envelopeFallbackCounter, 1)

		return fmt.Sprintf("%08x", uint32(n)) //nolint:gosec // fallback path, monotonic
	}

	return hex.EncodeToString(b[:])
}

// Envelope wording constants — the imperative text between the tags. Locked
// per Q7 of the exploration record for `formatter-inline-reinforcement`.
const (
	envelopeFinalReminder = "Before responding, verify the response conforms to the rules inside <formatter_skill>...</formatter_skill>. Apply them to the reply you are about to produce."
	envelopeDirective     = "Format your reply using the rules inside <formatter_skill>...</formatter_skill>. Do not restate the rules; apply them."
)

// TurnContextInput carries the dynamic per-turn content rendered into the
// turn-context message body by BuildTurnContext.
type TurnContextInput struct {
	// Timestamp is the pre-formatted current date/time string, already
	// quantized to the active prompt layout's granularity by the caller
	// (minute under tail-dynamic, hour under head-safe).
	Timestamp string
	// InlineFormatter is true on inline-reinforcement channels
	// (formatter_post_step: false and a non-empty formatter_skill), in which
	// case the final reminder and formatting directive blocks are appended.
	// False on post-step channels and channels with no formatter skill,
	// where the turn-context message still carries the timestamp but no
	// reinforcement blocks.
	InlineFormatter bool
}

// BuildTurnContext renders the entire per-turn dynamic tail — timestamp
// and (on inline-reinforcement channels) the verification
// reminder and formatting directive — as a single message body
// All tag pairs use turnID as a suffix so that untrusted
// user content cannot forge a closing tag; turnID is generated fresh per
// turn (via NewTurnID) and is unpredictable to the caller.
//
// Unlike the retired BuildInlineEnvelope, this never touches the user's
// message: the current user message is passed through verbatim by the
// caller, byte-identical to what is persisted to history. That is what lets
// the assembled prefix ahead of this message stay stable across turns (the
// KV-cache warm-start goal).
func BuildTurnContext(turnID string, in TurnContextInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "<turn_context_%s>\n", turnID)
	fmt.Fprintf(&b, "Current date and time: %s", in.Timestamp)
	fmt.Fprintf(&b, "\n</turn_context_%s>", turnID)

	if in.InlineFormatter {
		fmt.Fprintf(&b, "\n\n<final_reminder_%[1]s>\n%[2]s\n</final_reminder_%[1]s>", turnID, envelopeFinalReminder)
		fmt.Fprintf(&b, "\n\n<formatting_directive_%[1]s>\n%[2]s\n</formatting_directive_%[1]s>", turnID, envelopeDirective)
	}

	return b.String()
}

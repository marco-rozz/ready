package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
)

// Ladder selects which cooldown ladder a Demote classification starts
// : soft for transient failure classes, hard for classes that
// will not resolve by retrying the same candidate.
type Ladder int

const (
	// LadderNone means the error does not demote the candidate.
	LadderNone Ladder = iota
	// LadderSoft is the short cooldown ladder (429 rate limiting, 5xx /
	// provider overload).
	LadderSoft
	// LadderHard is the long cooldown ladder (auth, 404 model-not-found,
	// quota exhaustion, transport/DNS/TLS, attempt-budget expiry).
	LadderHard
)

// String returns a human-readable ladder label for logs.
func (l Ladder) String() string {
	switch l {
	case LadderSoft:
		return "soft"
	case LadderHard:
		return "hard"
	default:
		return "none"
	}
}

// Classification is the independent-axis verdict on an LLM call error:
//
//   - Advance: use another candidate for the current call. Independent of
//     whether the failing candidate is considered broken.
//   - Demote (+ Ladder): mark the candidate unhealthy and start a cooldown on
//     the selected ladder. A Demote without Advance cannot occur in the
//     classification table.
//   - RetrySame: retry the same candidate before demotion/advance. Permanent
//     failures may Advance+Demote while leaving RetrySame false.
//
// ProviderScoped is true only for quota-exhaustion failures: every candidate
// sharing the failing candidate's provider instance shares its API key and
// budget, so all of them are demoted together.
type Classification struct {
	Advance        bool
	Demote         bool
	RetrySame      bool
	Ladder         Ladder
	ProviderScoped bool
}

// Neither is the zero Classification: neither advance nor demote.
func Neither() Classification { return Classification{} }

// Classification table:
//
//	                          advance    demote    retry-same   ladder
//	401 / 403                    yes       yes         no       hard
//	404 model-not-found          yes       yes         no       hard
//	quota exhausted              yes       yes         no       hard + provider scope
//	transport / DNS / TLS        yes       yes        yes       hard
//	attempt timeout (no TTFT)    yes       yes        yes       hard
//	429 rate limit               yes       yes        yes       soft
//	5xx / provider overload      yes       yes        yes       soft
//	400 / cancellation / unknown no        no          no       —

// Error substrings used to classify provider errors. eino-ext components wrap
// provider errors into *NodeRunError / fmt.wrapError chains without stable
// typed accessors; the HTTP status is embedded in the message text. These
// constants are exported so tests can pin the exact matched substrings.
const (
	// Hard-ladder markers.
	errSubstr404          = "404"
	errSubstrNotFound     = "not found"
	errSubstr401          = "401"
	errSubstr403          = "403"
	errSubstrUnauthorized = "unauthorized"
	errSubstrForbidden    = "forbidden"
	// A provider may return HTTP 429 for either a temporary rate limit or an
	// exhausted account quota. The latter demotes on the hard ladder, and
	// every candidate sharing the provider instance with it.
	errSubstrUsageLimit  = "usage limit"
	errSubstrQuota       = "quota exceeded"
	errSubstrUsageCode   = "usage_limit_exceeded"
	errSubstrCredits     = "credits depleted"
	errSubstrCreditsCode = "credits_depleted"
	// Soft-ladder markers.
	errSubstr429 = "429"
	errSubstr500 = "500"
	errSubstr502 = "502"
	errSubstr503 = "503"
	errSubstr504 = "504"
	// OpenAI-style gateway errors that arrive WITHOUT a numeric status:
	// "Streaming response failed: [server_error] upstream service timeout"
	// (observed live via an OpenCode Zen chat_completions upstream timeout).
	errSubstrServerErr       = "server_error"
	errSubstrUpstreamTimeout = "upstream service timeout"
	// Agent-level empty completion: the exact product string both agent
	// construction paths return when a candidate's model ended its attempt
	// with neither final content nor a tool call. Matched literally so the
	// rule can never fire on a coincidentally blank provider error.
	errSubstrEmptyResponse = "agent produced no response"
	// Diagnostic-only marker (the design doc's "request shape" row): a 400 is
	// usually a request-shape bug (e.g. a trailing system message on `api:
	// anthropic`), not something a different model fixes, so it neither
	// advances nor demotes — but it is worth a distinct WARN rather than
	// silently retrying the same broken shape every turn.
	errSubstr400 = "400"
)

// ErrAttemptBudgetExceeded reports that a candidate attempt was cancelled
// because it produced no first token or chunk within its attempt budget
// (streaming: the time-to-first-token budget; non-streaming: the
// proportional share of the remaining outer deadline). The orchestrator
// wraps the underlying context error in this sentinel before classification,
// which maps it to Advance + Demote(hard) — a candidate that stays silent
// for the whole budget is treated as broken, while a candidate that emits
// its first chunk in time is never demoted for being slow.
var ErrAttemptBudgetExceeded = errors.New("llm candidate attempt budget exceeded before first token")

// IsRequestShapeError reports whether an error's rendered text contains an
// HTTP 400 marker. Used only for the diagnostic WARN (task 2.6) — it never
// changes either classification axis.
func IsRequestShapeError(err error) bool {
	return err != nil && strings.Contains(err.Error(), errSubstr400)
}

// containsStatusSubstring reports whether needle appears in msg as a STANDALONE
// status token — every non-digit character borders it. criticalities-remediation
// (LOW classify): a bare strings.Contains("500") matched "retry after 1500ms"
// (a rate-limit hint!) as a 5xx and demoted a healthy candidate.
func containsStatusSubstring(msg, needle string) bool {
	start := 0
	for {
		idx := strings.Index(msg[start:], needle)
		if idx < 0 {
			return false
		}

		idx += start
		beforeOK := idx == 0 || !isDigit(msg[idx-1])
		end := idx + len(needle)
		afterOK := end >= len(msg) || !isDigit(msg[end])

		if beforeOK && afterOK {
			return true
		}

		start = idx + 1
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// ClassifyLLMError inspects an error returned by a provider's Generate/Stream
// and returns its independent RetrySame/Advance/Demote classification. It
// inspects the full error chain
// (errors.Is against net.OpError / io.EOF plus strings.Contains on the
// rendered message) because eino-ext components do not expose typed
// HTTP-status accessors.
//
// Precedence notes:
//   - Caller cancellation (context.Canceled) matches nothing — neither axis —
//     so a user aborting a turn never demotes a healthy candidate.
//   - Quota-exhaustion detection takes precedence over the generic 429 rule
//     (load-bearing: an exhausted free-tier daily budget arrives as a 429,
//     and putting it on the soft ladder would retry it every 30 seconds for
//     twelve hours).
//   - An attempt-budget expiry (ErrAttemptBudgetExceeded) is hard: the
//     candidate stayed silent for its whole budget.
//   - io.EOF / unexpected EOF before any content is soft: a provider hiccup
//     that a different candidate survives.
//   - The agent-level empty completion (internal/agent returns the literal
//     "agent produced no response" when a candidate's model ended its attempt
//     with neither content nor a tool call, and the adapter surfaced no other
//     error) is soft: the provider answered cleanly but said nothing — a
//     silent-candidate malfunction the same or another candidate can recover
//     from, since the restart rewind guarantees no tool re-executes.
func ClassifyLLMError(err error) Classification {
	if err == nil {
		return Neither()
	}

	// Attempt-budget expiry is known by sentinel, not by text.
	if errors.Is(err, ErrAttemptBudgetExceeded) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderHard}
	}

	// Caller cancellation: neither axis. (The outer agent-turn deadline is
	// the caller's deadline too — a turn that ran out of time is over, and
	// no candidate is at fault.)
	if errors.Is(err, context.Canceled) {
		return Neither()
	}

	// Transport-level errors (connection refused, DNS, TLS, dial timeouts)
	// are hard.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderHard}
	}

	// criticalities-remediation (H5): context-level deadline expirations and
	// net.Error timeout conditions (dial, TLS, response-header deadlines —
	// and the shape an http.Client timeout produces while reading a body)
	// are advanceable: the next candidate may still deliver within the turn.
	// Note the ordering with context.Canceled above: a caller-cancelled
	// context is Canceled (neither axis), while an expired deadline is an
	// advanceable timeout condition.
	if errors.Is(err, context.DeadlineExceeded) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderHard}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderHard}
	}

	// io.EOF / unexpected EOF on a stream that never delivered content is a
	// provider hiccup — soft.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderSoft}
	}

	msg := strings.ToLower(err.Error())

	// 404 model-not-found: require both the status and "not found" to avoid
	// matching an unrelated "404" substring.
	if containsStatusSubstring(msg, errSubstr404) && strings.Contains(msg, errSubstrNotFound) {
		return Classification{Advance: true, Demote: true, Ladder: LadderHard}
	}

	if containsStatusSubstring(msg, errSubstr401) || strings.Contains(msg, errSubstrUnauthorized) {
		return Classification{Advance: true, Demote: true, Ladder: LadderHard}
	}

	if strings.Contains(msg, errSubstr403) || strings.Contains(msg, errSubstrForbidden) {
		return Classification{Advance: true, Demote: true, Ladder: LadderHard}
	}

	// An exhausted quota is not fixed by retrying the same reference — and
	// not by any other reference on the same provider instance either
	// Check this before the generic 429 rule because providers
	// commonly use 429 for both rate limiting and account-level quota
	// exhaustion.
	if strings.Contains(msg, errSubstrUsageLimit) || strings.Contains(msg, errSubstrQuota) ||
		strings.Contains(msg, errSubstrUsageCode) || strings.Contains(msg, errSubstrCredits) ||
		strings.Contains(msg, errSubstrCreditsCode) {
		return Classification{Advance: true, Demote: true, Ladder: LadderHard, ProviderScoped: true}
	}

	if containsStatusSubstring(msg, errSubstr429) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderSoft}
	}

	if containsStatusSubstring(msg, errSubstr500) ||
		containsStatusSubstring(msg, errSubstr502) ||
		containsStatusSubstring(msg, errSubstr503) ||
		containsStatusSubstring(msg, errSubstr504) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderSoft}
	}

	// Server-side gateway errors without a numeric status ([server_error],
	// "upstream service timeout") are the same failure class as a 5xx: a
	// transient provider/gateway overload another attempt may survive. Soft
	// ladder, retry-same, advanceable — not the "unrecognised" neither-axis
	// row they would otherwise fall into (the chain must be walked for a
	// trivially recoverable upstream timeout).
	// A 400-bearing message wins: "server_error" also appears inside request
	// identifiers (e.g. field names), and the design doc keeps request-shape 400s
	// on neither axis — never retried, never demoted.
	if !containsStatusSubstring(msg, errSubstr400) {
		if strings.Contains(msg, errSubstrServerErr) || strings.Contains(msg, errSubstrUpstreamTimeout) {
			return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderSoft}
		}
	}

	// A completed-but-empty candidate attempt (both agent construction paths
	// return this exact product string) is a named, self-owned failure — not
	// an unknown error shape. Leave it on neither axis and the whole chain
	// aborts on the first blank completion (observed live on 2026-10-05:
	// glm-5.3-flash ended its turn after one tool round with no message).
	if strings.Contains(msg, errSubstrEmptyResponse) {
		return Classification{Advance: true, Demote: true, RetrySame: true, Ladder: LadderSoft}
	}

	// 400 request shape and unrecognised errors: neither axis — the chain is
	// not walked (a novel error shape never causes a spurious switch).
	return Neither()
}

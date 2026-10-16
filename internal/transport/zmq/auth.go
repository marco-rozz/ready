package zmq

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// nonceBytes is the size in bytes of every challenge nonce. 16 bytes is
	// 128 bits, well above the birthday-collision threshold for the
	// 64-outstanding cap.
	nonceBytes = 16
	// maxPendingChallenges caps the number of outstanding challenges per
	// listener. Overflow evicts the oldest entry and emits a WARN log line.
	// The cap protects against an attacker that connects-and-never-replies in
	// a loop trying to exhaust the daemon's memory.
	maxPendingChallenges = 64

	// criticalities-remediation (LOW zmq): issue-rate guard against pre-auth
	// eviction floods. Without it, a flood of unauthenticated probes evicts
	// legitimate clients' pending challenges at will (the oldest-entry
	// eviction below) and starves the authentication path. When the rate is
	// exceeded the new challenge is refused (internal_error on the wire) and
	// capacity is preserved for real authenticating clients.
	maxChallengesPerWindow = 256
	challengeRateWindow    = time.Second
)

// ErrAuthMismatch is returned by VerifyAuthToken when the HMAC comparison
// fails, the nonce is missing, the challenge has expired, or the claimed hex
// value cannot be decoded. Callers should treat all of these as the same
// user-facing "auth_fail" outcome — distinguishing them in the response would
// leak information.
var ErrAuthMismatch = errors.New("zmq auth: token mismatch")

// challenge holds the in-memory state for a single pending challenge.
type challenge struct {
	nonce     []byte
	createdAt time.Time
}

// ChallengeStore owns the per-identity nonce table. It is safe for concurrent
// use; all operations take a write lock since they all mutate the map.
//
// The store deliberately knows nothing about the daemon's shared secret —
// VerifyAuthToken is parameterized on the secret so tests can exercise the
// HMAC path without holding global state.
type ChallengeStore struct {
	// secret is the daemon's auth_token used as the HMAC key. It is captured
	// here only so VerifyAuthToken does not need a second argument at every
	// call site; the value is never logged or surfaced in error messages.
	secret []byte
	// ttl is the per-nonce lifetime. Nonces older than ttl are swept on every
	// IssueChallenge call. Set to 2*connection_idle_timeout by the caller.
	ttl time.Duration
	// logger is scoped to component=zmq-session by the caller.
	logger zerolog.Logger
	// payloadsEnabled gates DEBUG-level events that include the nonce hex.
	payloadsEnabled bool

	mu      sync.Mutex
	pending map[string]challenge // key: hex-encoded identity bytes for stable map key
	// challengeTimes records recent IssueChallenge timestamps for the
	// pre-auth flood rate guard (bounded by the cap, pruned on each call).
	challengeTimes []time.Time
	// randSource is the source of entropy for nonces. Tests can swap it for
	// a deterministic stream; the production default is crypto/rand.Reader.
	randSource io.Reader
}

// NewChallengeStore constructs an empty ChallengeStore.
func NewChallengeStore(secret string, ttl time.Duration, payloadsEnabled bool, logger zerolog.Logger) *ChallengeStore {
	return &ChallengeStore{
		secret:          []byte(secret),
		ttl:             ttl,
		logger:          logger,
		payloadsEnabled: payloadsEnabled,
		pending:         make(map[string]challenge, maxPendingChallenges),
		randSource:      rand.Reader,
	}
}

// IssueChallenge generates a fresh 16-byte nonce for identity, stores it, and
// returns the hex-encoded form ready to be placed in
// Response{ErrorCode: "challenge", Content: <hex>}. The store sweeps expired
// entries (older than ttl) on every call and evicts the oldest pending
// challenge when the cap is exceeded.
//
// Callers are expected to check len(pendingChallenges[identity]) == 0 before
// calling — issuing a second challenge to the same identity while the first
// is still pending overwrites it (and is fine, that's just the client
// retrying).
func (s *ChallengeStore) IssueChallenge(identity []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sweepLocked(time.Now())

	// criticalities-remediation (LOW zmq): pre-auth flood guard — challenge
	// issuance beyond the rate window is refused so an unauthenticated
	// connection flood cannot evict legitimate clients' pending challenges
	// and starve the handshake path.
	now := time.Now()
	windowStart := now.Add(-challengeRateWindow)
	kept := s.challengeTimes[:0]
	for _, ts := range s.challengeTimes {
		if ts.After(windowStart) {
			kept = append(kept, ts)
		}
	}
	s.challengeTimes = kept

	if len(s.challengeTimes) >= maxChallengesPerWindow {
		s.logger.Warn().
			Int("cap", maxChallengesPerWindow).
			Dur("window", challengeRateWindow).
			Msg("challenge_rate_limit_rejected")

		return "", fmt.Errorf("zmq auth: challenge rate limit exceeded — retry later")
	}

	s.challengeTimes = append(s.challengeTimes, now)

	if len(s.pending) >= maxPendingChallenges {
		evicted := s.evictOldestLocked()

		s.logger.Warn().
			Str("evicted_identity", shortIdentity(evicted)).
			Int("cap", maxPendingChallenges).
			Msg("pending_challenges_overflow")
	}

	nonce := make([]byte, nonceBytes)
	if _, err := io.ReadFull(s.randSource, nonce); err != nil {
		return "", fmt.Errorf("zmq auth: failed to generate nonce: %w", err)
	}

	s.pending[hex.EncodeToString(identity)] = challenge{
		nonce:     nonce,
		createdAt: time.Now(),
	}

	nonceHex := hex.EncodeToString(nonce)

	if s.payloadsEnabled {
		s.logger.Debug().
			Str("identity", shortIdentity(identity)).
			Str("nonce", nonceHex).
			Msg("auth_challenge_sent")
	}

	return nonceHex, nil
}

// HasPending reports whether a pending challenge exists for identity. Used by
// the channel's session_start dispatcher to choose the probe-vs-verify branch.
func (s *ChallengeStore) HasPending(identity []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.pending[hex.EncodeToString(identity)]
	if !ok {
		return false
	}

	if time.Since(c.createdAt) > s.ttl {
		delete(s.pending, hex.EncodeToString(identity))

		return false
	}

	return true
}

// VerifyAuthToken compares the client's claimed auth_token (hex-encoded
// HMAC-SHA256 over the nonce) against the expected value. The pending entry
// is removed regardless of outcome — nonce single-use is enforced
// unconditionally.
//
// Returns nil on success, ErrAuthMismatch on any failure (missing nonce,
// expired, decode error, or HMAC mismatch). The constant-time compare is
// applied to the HEX representations, not the raw bytes, so a same-length
// constant-time comparison is always performed.
func (s *ChallengeStore) VerifyAuthToken(identity []byte, claimedHex string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := hex.EncodeToString(identity)

	c, ok := s.pending[key]
	if !ok {
		return ErrAuthMismatch
	}

	// Always remove the entry — nonce single-use, regardless of outcome.
	delete(s.pending, key)

	if time.Since(c.createdAt) > s.ttl {
		return ErrAuthMismatch
	}

	mac := hmac.New(sha256.New, s.secret)
	mac.Write(c.nonce)
	expected := hex.EncodeToString(mac.Sum(nil))

	// subtle.ConstantTimeCompare returns 1 iff the byte slices are equal AND
	// the same length. The HMAC output is fixed-length (32 bytes → 64 hex
	// chars) so this also implicitly rejects malformed claims.
	if subtle.ConstantTimeCompare([]byte(expected), []byte(claimedHex)) != 1 {
		return ErrAuthMismatch
	}

	return nil
}

// PendingCount returns the number of outstanding challenges. Intended for
// tests and metrics — production callers should not need it.
func (s *ChallengeStore) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.pending)
}

// sweepLocked drops expired entries. Caller must hold s.mu.
func (s *ChallengeStore) sweepLocked(now time.Time) {
	for k, c := range s.pending {
		if now.Sub(c.createdAt) > s.ttl {
			delete(s.pending, k)
		}
	}
}

// evictOldestLocked removes and returns the identity (raw bytes) of the
// oldest pending challenge. Caller must hold s.mu and must have already
// checked that len(s.pending) > 0.
func (s *ChallengeStore) evictOldestLocked() []byte {
	var (
		oldestKey string
		oldestAt  time.Time
		first     = true
	)

	for k, c := range s.pending {
		if first || c.createdAt.Before(oldestAt) {
			oldestKey = k
			oldestAt = c.createdAt
			first = false
		}
	}

	delete(s.pending, oldestKey)

	// Decode back to raw bytes for the log field. On a malformed key (should
	// not happen since we always hex-encode on insertion) fall back to the
	// raw key string — even that is safe to log since identities are not
	// secret.
	raw, err := hex.DecodeString(oldestKey)
	if err != nil {
		return []byte(oldestKey)
	}

	return raw
}

// shortIdentity returns the first 8 hex chars of identity, suitable for log
// fields. ZMQ identities are arbitrary byte strings (often 5 bytes when
// auto-assigned by the binding), so we hex-encode and truncate.
func shortIdentity(identity []byte) string {
	h := hex.EncodeToString(identity)
	if len(h) > 8 {
		return h[:8]
	}

	return h
}

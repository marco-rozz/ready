package zmq

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// newTestStore returns a ChallengeStore whose logger writes into the returned
// buffer. The randSource is left as crypto/rand.Reader unless the caller
// swaps it.
func newTestStore(t *testing.T, secret string, ttl time.Duration) (*ChallengeStore, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).With().Str("component", "zmq-session").Logger()
	// Force debug level so payload-gated lines are eligible when payloadsEnabled is true.
	logger = logger.Level(zerolog.DebugLevel)

	return NewChallengeStore(secret, ttl, false, logger), buf
}

// computeExpectedAuthToken mirrors the daemon's HMAC computation: secret over
// nonce, hex-encoded. Tests use it to derive a "correct" auth_token from a
// known nonce.
func computeExpectedAuthToken(secret string, nonce []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nonce)

	return hex.EncodeToString(mac.Sum(nil))
}

// TestIssueChallenge_StoresPending verifies a fresh challenge is stored,
// returned as a 64-hex-char string (32 bytes), and reports as pending.
func TestIssueChallenge_StoresPending(t *testing.T) {
	store, _ := newTestStore(t, "secret", time.Minute)

	identity := []byte("peer-1")

	nonceHex, err := store.IssueChallenge(identity)
	if err != nil {
		t.Fatalf("IssueChallenge returned unexpected error: %v", err)
	}

	if len(nonceHex) != 2*nonceBytes {
		t.Fatalf("nonce hex length = %d, want %d", len(nonceHex), 2*nonceBytes)
	}

	if !store.HasPending(identity) {
		t.Fatalf("HasPending should be true immediately after IssueChallenge")
	}

	if store.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1", store.PendingCount())
	}
}

// TestVerifyAuthToken_CorrectTokenPasses is the happy path.
func TestVerifyAuthToken_CorrectTokenPasses(t *testing.T) {
	store, _ := newTestStore(t, "topsecret", time.Minute)

	identity := []byte("peer-1")

	nonceHex, err := store.IssueChallenge(identity)
	if err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}

	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		t.Fatalf("hex.DecodeString: %v", err)
	}

	authToken := computeExpectedAuthToken("topsecret", nonce)

	if err = store.VerifyAuthToken(identity, authToken); err != nil {
		t.Fatalf("VerifyAuthToken should pass with correct token, got: %v", err)
	}

	if store.PendingCount() != 0 {
		t.Fatalf("PendingCount after verify = %d, want 0", store.PendingCount())
	}
}

// TestVerifyAuthToken_WrongTokenFails covers the most common failure case.
func TestVerifyAuthToken_WrongTokenFails(t *testing.T) {
	store, _ := newTestStore(t, "topsecret", time.Minute)

	identity := []byte("peer-1")

	if _, err := store.IssueChallenge(identity); err != nil {
		t.Fatalf("IssueChallenge: %v", err)
	}

	// 64 hex chars but not the right HMAC.
	wrongToken := strings.Repeat("0", 64)

	if err := store.VerifyAuthToken(identity, wrongToken); !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("VerifyAuthToken should fail with ErrAuthMismatch, got: %v", err)
	}

	if store.PendingCount() != 0 {
		t.Fatalf("nonce must be consumed even on mismatch; PendingCount = %d", store.PendingCount())
	}
}

// TestVerifyAuthToken_NonceReuseFails: a second verify after a successful one
// must fail because the nonce was removed.
func TestVerifyAuthToken_NonceReuseFails(t *testing.T) {
	store, _ := newTestStore(t, "topsecret", time.Minute)

	identity := []byte("peer-1")

	nonceHex, _ := store.IssueChallenge(identity)
	nonce, _ := hex.DecodeString(nonceHex)
	authToken := computeExpectedAuthToken("topsecret", nonce)

	if err := store.VerifyAuthToken(identity, authToken); err != nil {
		t.Fatalf("first verify should pass: %v", err)
	}

	if err := store.VerifyAuthToken(identity, authToken); !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("second verify should fail (nonce single-use), got: %v", err)
	}
}

// TestVerifyAuthToken_MissingNonceFails: verifying for an identity that was
// never issued a challenge is auth_fail.
func TestVerifyAuthToken_MissingNonceFails(t *testing.T) {
	store, _ := newTestStore(t, "topsecret", time.Minute)

	identity := []byte("unknown-peer")

	if err := store.VerifyAuthToken(identity, strings.Repeat("a", 64)); !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("verify with no pending nonce should fail, got: %v", err)
	}
}

// TestVerifyAuthToken_ExpiredNonceFails: a nonce older than ttl is rejected.
func TestVerifyAuthToken_ExpiredNonceFails(t *testing.T) {
	store, _ := newTestStore(t, "topsecret", 10*time.Millisecond)

	identity := []byte("peer-1")

	nonceHex, _ := store.IssueChallenge(identity)
	nonce, _ := hex.DecodeString(nonceHex)
	authToken := computeExpectedAuthToken("topsecret", nonce)

	time.Sleep(50 * time.Millisecond)

	if err := store.VerifyAuthToken(identity, authToken); !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("expired nonce should fail verify, got: %v", err)
	}
}

// TestVerifyAuthToken_SwappedSecretFails verifies the HMAC really depends on
// the secret — a same-nonce same-claim pair must fail under a different
// daemon secret.
func TestVerifyAuthToken_SwappedSecretFails(t *testing.T) {
	storeA, _ := newTestStore(t, "secret-A", time.Minute)
	storeB, _ := newTestStore(t, "secret-B", time.Minute)

	identity := []byte("peer-1")

	// Issue against both stores; both will issue independent nonces.
	nonceAHex, _ := storeA.IssueChallenge(identity)
	if _, err := storeB.IssueChallenge(identity); err != nil {
		t.Fatalf("IssueChallenge B: %v", err)
	}

	nonceA, _ := hex.DecodeString(nonceAHex)

	// Compute token for store A's nonce under store A's secret.
	tokenForA := computeExpectedAuthToken("secret-A", nonceA)

	// Present it to store B — the nonce key matches but B's secret is
	// different and B's stored nonce is different, so this must fail.
	if err := storeB.VerifyAuthToken(identity, tokenForA); !errors.Is(err, ErrAuthMismatch) {
		t.Fatalf("verify with wrong-secret token should fail, got: %v", err)
	}
}

// TestPendingChallengesOverflow ensures the cap is enforced and a WARN log is
// emitted on eviction.
func TestPendingChallengesOverflow(t *testing.T) {
	store, buf := newTestStore(t, "secret", time.Minute)

	// Fill exactly to the cap.
	for i := range maxPendingChallenges { // i := 0; i < maxPendingChallenges; i++
		identity := []byte{byte(i), 'a', 'b'}
		if _, err := store.IssueChallenge(identity); err != nil {
			t.Fatalf("IssueChallenge[%d]: %v", i, err)
		}

		// Pad timestamps so evictOldest has a deterministic oldest.
		// (createdAt is set inside IssueChallenge to time.Now(); a tiny sleep
		// is the cheapest way to break ties.)
		time.Sleep(time.Millisecond)
	}

	if store.PendingCount() != maxPendingChallenges {
		t.Fatalf("PendingCount before overflow = %d, want %d", store.PendingCount(), maxPendingChallenges)
	}

	overflowIdentity := []byte("overflow")
	if _, err := store.IssueChallenge(overflowIdentity); err != nil {
		t.Fatalf("IssueChallenge overflow: %v", err)
	}

	if store.PendingCount() != maxPendingChallenges {
		t.Fatalf("PendingCount after overflow = %d, want %d (eviction should keep it at cap)", store.PendingCount(), maxPendingChallenges)
	}

	output := buf.String()
	if !strings.Contains(output, "pending_challenges_overflow") {
		t.Fatalf("expected pending_challenges_overflow log line, got:\n%s", output)
	}
}

// TestSecretNeverAppearsInLogs is the load-bearing invariant: under no
// circumstances may the daemon's auth_token value end up in any log line.
func TestSecretNeverAppearsInLogs(t *testing.T) {
	// Use a deliberately unusual string so a substring match is reliable.
	secret := "DAEMON-TOKEN-DO-NOT-LEAK-0123456789abcdef"

	store, buf := newTestStore(t, secret, time.Minute)
	store.payloadsEnabled = true // even with payload logging on, the secret stays out

	identity := []byte("peer-1")

	// Successful round trip.
	nonceHex, _ := store.IssueChallenge(identity)
	nonce, _ := hex.DecodeString(nonceHex)
	authToken := computeExpectedAuthToken(secret, nonce)

	if err := store.VerifyAuthToken(identity, authToken); err != nil {
		t.Fatalf("verify should pass: %v", err)
	}

	// Failed round trip with a wrong-token to exercise the ErrAuthMismatch path.
	if _, err := store.IssueChallenge(identity); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	_ = store.VerifyAuthToken(identity, strings.Repeat("0", 64))

	// Overflow path too.
	for i := range maxPendingChallenges + 5 { // i := 0; i < maxPendingChallenges+5; i++
		_, _ = store.IssueChallenge([]byte{byte(i), byte(i >> 8)})
	}

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("daemon secret %q leaked into log output:\n%s", secret, buf.String())
	}
}

// TestNonceGatedByPayloadsEnabled covers the DEBUG payload gate.
func TestNonceGatedByPayloadsEnabled(t *testing.T) {
	tests := []struct {
		name            string
		payloadsEnabled bool
		wantNonceLogged bool
	}{
		{name: "gate off → nonce not logged", payloadsEnabled: false, wantNonceLogged: false},
		{name: "gate on → nonce logged at debug", payloadsEnabled: true, wantNonceLogged: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, buf := newTestStore(t, "secret", time.Minute)
			store.payloadsEnabled = tc.payloadsEnabled

			nonceHex, err := store.IssueChallenge([]byte("peer-1"))
			if err != nil {
				t.Fatalf("IssueChallenge: %v", err)
			}

			loggedNonce := strings.Contains(buf.String(), nonceHex)
			if loggedNonce != tc.wantNonceLogged {
				t.Fatalf("nonce-in-logs = %v, want %v\noutput:\n%s",
					loggedNonce, tc.wantNonceLogged, buf.String())
			}
		})
	}
}

// TestIssueChallenge_RandSourceFailure: when the entropy source returns an
// error, IssueChallenge surfaces it instead of silently producing a zero nonce.
func TestIssueChallenge_RandSourceFailure(t *testing.T) {
	store, _ := newTestStore(t, "secret", time.Minute)

	// Swap in a reader that always returns io.ErrUnexpectedEOF.
	store.randSource = errReader{}

	if _, err := store.IssueChallenge([]byte("peer-1")); err == nil {
		t.Fatalf("IssueChallenge should fail when rand source errors")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected wrapped io.ErrUnexpectedEOF, got: %v", err)
	}
}

// errReader is a stub io.Reader that always errors. Used to test the random
// source failure path.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

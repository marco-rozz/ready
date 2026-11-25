package okf

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Digest is the content digest of a file: the first 16 lowercase hexadecimal
// characters of SHA-256 over its raw bytes. okf_query read prints it and
// okf_write requires it as base_digest.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:])[:16]
}

// VerifyToken is the domain-separated token a /verify preview shows: the
// first 16 lowercase hex characters of SHA-256 over
// "okf-verify-v1\x00" + bundle + "\x00" + concept id + "\x00" + raw bytes.
// It is only ever produced for session-command replies, never by a tool, so
// the model can neither see nor compute it.
func VerifyToken(bundle, id string, raw []byte) string {
	h := sha256.New()
	h.Write([]byte("okf-verify-v1\x00"))
	h.Write([]byte(bundle))
	h.Write([]byte{0})
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(raw)

	return hex.EncodeToString(h.Sum(nil))[:16]
}

// tokenEqual compares two tokens in constant time, case-insensitively on the
// supplied one.
func tokenEqual(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(got))) == 1
}

// Locks is the process-lifetime per-bundle lock registry. It is owned by the
// `ready serve` shell, not by a Runtime generation, so Subagent dispatches on
// an old generation and writers on different addresses exclude each other.
type Locks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// NewLocks returns an empty registry.
func NewLocks() *Locks { return &Locks{m: map[string]*sync.Mutex{}} }

// Lock acquires the lock of the bundle directory dir (keyed by its resolved
// identity) and returns the release function.
func (l *Locks) Lock(dir string) func() {
	key := dir
	if abs, err := filepath.Abs(dir); err == nil {
		key = abs
	}
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	} else if _, statErr := os.Stat(key); statErr != nil {
		key = filepath.Clean(key)
	}
	l.mu.Lock()
	mu := l.m[key]
	if mu == nil {
		mu = &sync.Mutex{}
		l.m[key] = mu
	}
	l.mu.Unlock()
	mu.Lock()

	return mu.Unlock
}

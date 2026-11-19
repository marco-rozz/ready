package errdigest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

var (
	ErrGuardRepeated    = errors.New("repeated permanent tool failure")
	ErrGuardCapacity    = errors.New("tool failure guard capacity exhausted")
	ErrGuardEditRetries = errors.New("config edit correction limit reached")
)

// TurnGuard holds bounded invocation fingerprints for one user turn.
// It never retains argument values. Callers must install a fresh guard for
// each turn and share its context across all attempts of that turn.
type TurnGuard struct {
	mu             sync.Mutex
	entries        map[[32]byte]*guardEntry
	limit          int
	editRecovering bool
	editAttempts   int
	editSerial     chan struct{}
	editExhausted  bool
}

type guardEntry struct {
	done      chan struct{}
	permanent bool
}

type turnGuardKey struct{}

// WithTurnGuard attaches a fresh guard to a turn context.
func WithTurnGuard(ctx context.Context, limit int) context.Context {
	if limit < 1 {
		limit = 1
	}
	g := &TurnGuard{entries: make(map[[32]byte]*guardEntry), limit: limit, editSerial: make(chan struct{}, 1)}
	g.editSerial <- struct{}{}

	return context.WithValue(ctx, turnGuardKey{}, g)
}

func guardFromContext(ctx context.Context) *TurnGuard {
	guard, _ := ctx.Value(turnGuardKey{}).(*TurnGuard)

	return guard
}

// RecoverInvocation clears one specific failed invocation after a tool has
// established a relevant state change. Callers must not use generic success
// as evidence; no arguments are retained by the guard.
func RecoverInvocation(ctx context.Context, name, arguments string) {
	g := guardFromContext(ctx)
	if g == nil {
		return
	}
	key := fingerprint(name, arguments)
	g.mu.Lock()
	defer g.mu.Unlock()
	if entry := g.entries[key]; entry != nil && entry.permanent {
		delete(g.entries, key)
	}
}

// BeginConfigCorrection counts each subsequent update after an exact-match
// failure, including updates with different arguments.
func BeginConfigCorrection(ctx context.Context) error {
	if g := guardFromContext(ctx); g != nil {
		select {
		case <-g.editSerial:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.editExhausted {
			g.editSerial <- struct{}{}

			return ErrGuardEditRetries
		}
		if g.editRecovering {
			g.editAttempts++
		}
	}

	return nil
}

// EndConfigCorrection records a failed exact match or ends recovery after a
// successful update. It returns a terminal error after two failed corrections.
func EndConfigCorrection(ctx context.Context, exactFailure, success bool) error {
	g := guardFromContext(ctx)
	if g == nil {
		return nil
	}
	defer func() { g.editSerial <- struct{}{} }()
	g.mu.Lock()
	defer g.mu.Unlock()
	if success {
		g.editRecovering = false
		g.editAttempts = 0

		return nil
	}
	if g.editRecovering && g.editAttempts >= 2 {
		g.editExhausted = true

		return ErrGuardEditRetries
	}
	if exactFailure {
		g.editRecovering = true
	}

	return nil
}

// fingerprint uses canonical JSON when possible; invalid JSON is hashed as
// received. A length prefix prevents ambiguity between tool name and input.
func fingerprint(name, arguments string) [32]byte {
	var value any
	dec := json.NewDecoder(bytes.NewBufferString(arguments))
	dec.UseNumber()
	var canonical []byte
	if dec.Decode(&value) == nil {
		var extra any
		if dec.Decode(&extra) == io.EOF {
			canonical, _ = json.Marshal(value)
		}
	}
	if canonical == nil {
		canonical = []byte(arguments)
	}
	encoded, _ := json.Marshal([]any{name, string(canonical)})

	return sha256.Sum256(encoded)
}

// begin serializes identical invocations while their outcome is unknown.
// A full registry refuses new invocations instead of evicting failures.
func (g *TurnGuard) begin(ctx context.Context, key [32]byte) (bool, error) {
	for {
		g.mu.Lock()
		entry, exists := g.entries[key]
		if !exists {
			if len(g.entries) >= g.limit {
				g.mu.Unlock()

				return false, ErrGuardCapacity
			}
			g.entries[key] = &guardEntry{done: make(chan struct{})}
			g.mu.Unlock()

			return true, nil
		}
		if entry.permanent {
			g.mu.Unlock()

			return false, ErrGuardRepeated
		}
		done := entry.done
		g.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (g *TurnGuard) finish(key [32]byte, permanent bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entries[key]
	entry.permanent = permanent
	if !permanent {
		delete(g.entries, key)
	}
	close(entry.done)
}

package oauth

import (
	"context"
	"errors"
)

// ErrStoreUnavailable is returned when a Store cannot be constructed or
// cannot reach its backing medium (e.g. the configured directory cannot be
// created).
var ErrStoreUnavailable = errors.New("oauth credential store unavailable")

// Store persists one Credential per MCP server, keyed by its configuration
// map key (an encrypted Secret-store entry, allowing
// per-server revocation by deleting one record).
type Store interface {
	// Load returns the stored Credential for serverKey, or (nil, nil) when
	// no record exists.
	Load(ctx context.Context, serverKey string) (*Credential, error)
	// Save persists cred, replacing any existing record for the same
	// ServerKey. It serializes cross-process on the target's coordination
	// lock and bumps the record's generation.
	Save(ctx context.Context, cred *Credential) error
	// Delete removes the stored Credential for serverKey, recording a
	// tombstone so stale work cannot resurrect the record. It is not an
	// error to delete an absent record.
	Delete(ctx context.Context, serverKey string) error
	// Snapshot returns the target's current generation (record or
	// tombstone, 0 when neither exists) for snapshot/compare-commit flows.
	Snapshot(ctx context.Context, serverKey string) (int64, error)
	// SaveCommitted persists cred only while the target's generation still
	// equals snapshotGen; divergence fails with ErrGenerationConflict.
	SaveCommitted(ctx context.Context, cred *Credential, snapshotGen int64) error
}

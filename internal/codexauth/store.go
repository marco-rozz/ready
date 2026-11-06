package codexauth

import (
	"context"
	"encoding/base64"
	"errors"
)

var ErrStoreUnavailable = errors.New("codex oauth credential store unavailable")

// Store persists one Codex credential per named LLM provider.
type Store interface {
	Load(ctx context.Context, providerKey string) (*Credential, error)
	// Save persists cred, serializing cross-process on the target's
	// coordination lock and bumping the record's generation.
	Save(ctx context.Context, cred *Credential) error
	// Delete removes the stored Credential for providerKey, recording a
	// tombstone so stale work cannot resurrect the record. It is not an
	// error to delete an absent record.
	Delete(ctx context.Context, providerKey string) error
	// Snapshot returns the target's current generation (record or
	// tombstone, 0 when neither exists) for snapshot/compare-commit flows.
	Snapshot(ctx context.Context, providerKey string) (int64, error)
	// SaveCommitted persists cred only while the target's generation still
	// equals snapshotGen; divergence fails with ErrGenerationConflict.
	SaveCommitted(ctx context.Context, cred *Credential, snapshotGen int64) error
}

func filenameFor(providerKey string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(providerKey))

	return "llm-" + encoded + ".json"
}

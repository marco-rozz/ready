package oauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/marco-rozz/ready/internal/config"
)

// ErrStoreUnavailable is returned when a Store cannot be constructed or
// cannot reach its backing medium (e.g. the configured directory cannot be
// created).
var ErrStoreUnavailable = errors.New("oauth credential store unavailable")

// Store persists one Credential per MCP server, keyed by its configuration
// map key (a file-based store, allowing
// per-server revocation by deleting one record).
type Store interface {
	// Load returns the stored Credential for serverKey, or (nil, nil) when
	// no record exists.
	Load(ctx context.Context, serverKey string) (*Credential, error)
	// Save persists cred, replacing any existing record for the same
	// ServerKey.
	Save(ctx context.Context, cred *Credential) error
	// Delete removes the stored Credential for serverKey. It is not an
	// error to delete an absent record.
	Delete(ctx context.Context, serverKey string) error
}

// NewStore builds the Store backend selected by cfg.TokenStore. File is the
// only backend today; config.Validate rejects any other value
// before this is ever called, but the factory still fails explicitly rather
// than silently defaulting, so a future caller that skips validation cannot
// end up writing secrets to an unintended location.
func NewStore(ctx context.Context, cfg config.OAuthConfig) (Store, error) {
	switch cfg.TokenStore {
	case "", "file":
		return NewFileStore(ctx, cfg.Path)
	default:
		return nil, fmt.Errorf("%w: unsupported token_store %q", ErrStoreUnavailable, cfg.TokenStore)
	}
}

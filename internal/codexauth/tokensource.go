package codexauth

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const defaultExpirySkew = 30 * time.Second

// Access holds the current request-scoping secrets. It is deliberately a
// value type and has no String method, preventing accidental log expansion.
type Access struct {
	Token     string
	AccountID string
}

type AccessSource interface {
	Access(ctx context.Context) (Access, error)
	Invalidate(accessToken string)
}

// TokenSource serializes refreshes per process while allowing waiters to
// cancel. Its compare-before-save guard lets a concurrent ready login win.
type TokenSource struct {
	mu          sync.Mutex
	store       Store
	client      *Client
	providerKey string
	loginCmd    string
	cred        *Credential
	skew        time.Duration
	refreshing  chan struct{}
	lastErr     error
}

var _ AccessSource = (*TokenSource)(nil)

// NewTokenSource builds the refreshing source. loginCmd is the exact
// `ready login` remediation for this provider's agent (empty: the short
// `ready login <provider>` form).
func NewTokenSource(store Store, client *Client, providerKey, loginCmd string, cred *Credential) (*TokenSource, error) {
	if loginCmd == "" {
		loginCmd = "ready login " + providerKey
	}
	if store == nil || client == nil {
		return nil, fmt.Errorf("build codex token source: store and client are required")
	}
	if err := cred.Validate(providerKey); err != nil {
		return nil, fmt.Errorf("build codex token source for %q: %w; run `%s`", providerKey, ErrLoginRequired, loginCmd)
	}
	copyCred := *cred

	return &TokenSource{store: store, client: client, providerKey: providerKey, loginCmd: loginCmd, cred: &copyCred, skew: defaultExpirySkew}, nil
}

func (s *TokenSource) Access(ctx context.Context) (Access, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Access{}, fmt.Errorf("get codex access token: %w", err)
		}
		s.mu.Lock()
		// Observe replacement/forget before acting on the cached record
		// (per-user-install-agent-isolation): a newer generation — including
		// a tombstone from a concurrent forget — supersedes the cache.
		s.adoptCurrentLocked(ctx)
		if s.cred != nil && s.cred.AccessToken != "" && (s.cred.Expiry.IsZero() || s.cred.Expiry.After(s.client.now().Add(s.skew))) {
			out := Access{Token: s.cred.AccessToken, AccountID: s.cred.AccountID}
			s.mu.Unlock()

			return out, nil
		}
		if s.refreshing != nil {
			wait := s.refreshing
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return Access{}, fmt.Errorf("wait for codex token refresh: %w", ctx.Err())
			case <-wait:
				continue
			}
		}
		if s.cred == nil || s.cred.RefreshToken == "" {
			s.mu.Unlock()

			return Access{}, fmt.Errorf("%w for provider %q; run `%s`", ErrLoginRequired, s.providerKey, s.loginCmd)
		}
		refreshing := make(chan struct{})
		s.refreshing = refreshing
		snapshot := *s.cred
		s.mu.Unlock()

		refreshed, err := s.client.Refresh(ctx, s.providerKey, snapshot.RefreshToken)
		if err == nil {
			refreshed, err = s.commitRefresh(ctx, &snapshot, refreshed)
		}

		s.mu.Lock()
		if err == nil {
			s.cred = refreshed
		}
		s.lastErr = err
		s.refreshing = nil
		close(refreshing)
		s.mu.Unlock()
		if err != nil {
			return Access{}, fmt.Errorf("refresh codex credential for %q: %w; run `%s`", s.providerKey, err, s.loginCmd)
		}
	}
}

func (s *TokenSource) commitRefresh(ctx context.Context, old, refreshed *Credential) (*Credential, error) {
	// Compare-commit under the target's cross-process coordination (per-user-
	// install-agent-isolation): the cached generation is the snapshot; a
	// concurrent login/forget that changed the record wins and the stale
	// refresh is abandoned instead of overwriting or resurrecting it.
	current, err := s.store.Load(ctx, s.providerKey)
	if err != nil {
		return nil, fmt.Errorf("reload codex credential before refresh save: %w", err)
	}
	if current != nil && (current.RefreshToken != old.RefreshToken || current.Generation > old.Generation || current.UpdatedAt.After(old.UpdatedAt)) {
		if err := current.Validate(s.providerKey); err != nil {
			return nil, err
		}

		return current, nil
	}
	if current == nil {
		// The record was forgotten (tombstone) while we were refreshing.
		return nil, fmt.Errorf("codex credential for %q was deleted during refresh; run `%s`", s.providerKey, s.loginCmd)
	}
	if err := s.store.Save(ctx, refreshed); err != nil {
		return nil, fmt.Errorf("persist refreshed codex credential: %w", err)
	}

	return refreshed, nil
}

// adoptCurrentLocked reloads the store and adopts any newer generation;
// an absent record supersedes the cache when a tombstone or newer deletion
// exists. Best-effort. Caller holds s.mu.
func (s *TokenSource) adoptCurrentLocked(ctx context.Context) {
	snap, snapErr := s.store.Snapshot(ctx, s.providerKey)
	if snapErr != nil || snap == 0 || (s.cred != nil && snap <= s.cred.Generation) {
		return
	}
	cur, err := s.store.Load(ctx, s.providerKey)
	if err != nil {
		return
	}
	s.cred = cur // nil = forgotten/deleted → login required below
}

// Invalidate forces the next request to refresh, but only if the rejected
// token is still current; this avoids invalidating a concurrent fresh login.
func (s *TokenSource) Invalidate(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cred != nil && s.cred.AccessToken == accessToken {
		s.cred.Expiry = time.Time{}
		s.cred.AccessToken = ""
	}
}

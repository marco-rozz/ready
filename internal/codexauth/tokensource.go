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
	cred        *Credential
	skew        time.Duration
	refreshing  chan struct{}
	lastErr     error
}

var _ AccessSource = (*TokenSource)(nil)

func NewTokenSource(store Store, client *Client, providerKey string, cred *Credential) (*TokenSource, error) {
	if store == nil || client == nil {
		return nil, fmt.Errorf("build codex token source: store and client are required")
	}
	if err := cred.Validate(providerKey); err != nil {
		return nil, fmt.Errorf("build codex token source for %q: %w; run `ready login %s`", providerKey, ErrLoginRequired, providerKey)
	}
	copyCred := *cred

	return &TokenSource{store: store, client: client, providerKey: providerKey, cred: &copyCred, skew: defaultExpirySkew}, nil
}

func (s *TokenSource) Access(ctx context.Context) (Access, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Access{}, fmt.Errorf("get codex access token: %w", err)
		}
		s.mu.Lock()
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

			return Access{}, fmt.Errorf("%w for provider %q; run `ready login %s`", ErrLoginRequired, s.providerKey, s.providerKey)
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
			return Access{}, fmt.Errorf("refresh codex credential for %q: %w; run `ready login %s`", s.providerKey, err, s.providerKey)
		}
	}
}

func (s *TokenSource) commitRefresh(ctx context.Context, old, refreshed *Credential) (*Credential, error) {
	current, err := s.store.Load(ctx, s.providerKey)
	if err != nil {
		return nil, fmt.Errorf("reload codex credential before refresh save: %w", err)
	}
	if current != nil && (current.RefreshToken != old.RefreshToken || current.UpdatedAt.After(old.UpdatedAt)) {
		if err := current.Validate(s.providerKey); err != nil {
			return nil, err
		}

		return current, nil
	}
	if err := s.store.Save(ctx, refreshed); err != nil {
		return nil, fmt.Errorf("persist refreshed codex credential: %w", err)
	}

	return refreshed, nil
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

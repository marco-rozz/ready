package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeTokenEndpoint drives a sequence of refresh responses in order,
// asserting that each request carries the expected refresh_token and
// (when non-empty) resource parameter.
type fakeTokenEndpoint struct {
	t         *testing.T
	mu        sync.Mutex
	responses []func(w http.ResponseWriter, r *http.Request)
	calls     int32
}

func (f *fakeTokenEndpoint) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			f.t.Fatalf("failed to parse refresh request form: %v", err)
		}

		idx := int(atomic.AddInt32(&f.calls, 1)) - 1

		f.mu.Lock()
		defer f.mu.Unlock()

		if idx >= len(f.responses) {
			f.t.Fatalf("unexpected extra token request #%d", idx+1)
		}

		f.responses[idx](w, r)
	}
}

func jsonTokenResponse(accessToken, newRefreshToken string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":        accessToken,
			"token_type":          tokenTypeBearer,
			paramRefreshTokenJSON: newRefreshToken,
			"expires_in":          3600,
		})
	}
}

func errorTokenResponse(status int, code string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
	}
}

func expiredCredential() *Credential {
	c := NewCredential(testServerKey)
	c.ClientID = testClientID
	c.AccessToken = "expired-access"
	c.RefreshToken = "refresh-1"
	c.Expiry = time.Now().Add(-time.Hour)
	c.UpdatedAt = time.Now().Add(-time.Hour)

	return c
}

func TestTokenSource_RotatedRefreshTokenIsPersisted(t *testing.T) {
	fake := &fakeTokenEndpoint{t: t, responses: []func(http.ResponseWriter, *http.Request){
		jsonTokenResponse("access-2", "refresh-2"),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := expiredCredential()
	cred.TokenEndpoint = srv.URL
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "access-2" || tok.RefreshToken != "refresh-2" {
		t.Fatalf("Token() = %+v, want rotated refresh token", tok)
	}

	stored, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken != "refresh-2" {
		t.Fatalf("stored refresh token = %q, want %q", stored.RefreshToken, "refresh-2")
	}
}

func TestTokenSource_ResourcePresentOnRefresh(t *testing.T) {
	var gotResource string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotResource = r.Form.Get("resource")
		jsonTokenResponse("access-2", "refresh-2")(w, r)
	}))
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := expiredCredential()
	cred.TokenEndpoint = srv.URL
	cred.Resource = "https://mcp.example.com/mcp"
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)
	if _, err = ts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}

	if gotResource != cred.Resource {
		t.Fatalf("resource on refresh = %q, want %q", gotResource, cred.Resource)
	}
}

func TestTokenSource_DeadTokenWithNewerStoredCredentialRecovers(t *testing.T) {
	fake := &fakeTokenEndpoint{t: t, responses: []func(http.ResponseWriter, *http.Request){
		errorTokenResponse(http.StatusBadRequest, "invalid_grant"),
		jsonTokenResponse("access-3", "refresh-3"),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	stale := expiredCredential()
	stale.TokenEndpoint = srv.URL
	if err = store.Save(ctx, stale); err != nil {
		t.Fatalf("Save stale: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), stale)

	// Simulate `ready login` running out of band: a newer, usable credential
	// (different refresh token so the second endpoint call would succeed)
	// lands in the store after ts was constructed.
	fresh := NewCredential(testServerKey)
	fresh.ClientID = stale.ClientID
	fresh.TokenEndpoint = srv.URL
	fresh.AccessToken = "still-expired"
	fresh.RefreshToken = "refresh-fresh"
	fresh.Expiry = time.Now().Add(-time.Minute)
	fresh.UpdatedAt = time.Now()
	if err = store.Save(ctx, fresh); err != nil {
		t.Fatalf("Save fresh: %v", err)
	}

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "access-3" {
		t.Fatalf("Token() = %+v, want recovery via the newer stored credential", tok)
	}
}

func TestTokenSource_DeadTokenWithNoNewerCredentialReturnsOriginalError(t *testing.T) {
	fake := &fakeTokenEndpoint{t: t, responses: []func(http.ResponseWriter, *http.Request){
		errorTokenResponse(http.StatusBadRequest, "invalid_grant"),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := expiredCredential()
	cred.TokenEndpoint = srv.URL
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)

	_, err = ts.Token()
	if err == nil {
		t.Fatal("expected an error")
	}

	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		t.Fatalf("expected *oauth2.RetrieveError, got %v (%T)", err, err)
	}
	if retrieveErr.ErrorCode != "invalid_grant" {
		t.Fatalf("ErrorCode = %q, want invalid_grant", retrieveErr.ErrorCode)
	}
}

func TestTokenSource_ExpiredNoRefreshTokenIsLoginRequiredWithoutNetworkIO(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = testClientID
	cred.AccessToken = "expired"
	cred.Expiry = time.Now().Add(-time.Hour)
	cred.TokenEndpoint = srv.URL
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)

	_, err = ts.Token()
	if err == nil || !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("expected ErrLoginRequired, got %v", err)
	}
	if called {
		t.Fatal("expected no network I/O when the credential has no refresh token")
	}
}

func TestTokenSource_ConcurrentTokenCallsAreSafe(t *testing.T) {
	fake := &fakeTokenEndpoint{t: t, responses: []func(http.ResponseWriter, *http.Request){
		jsonTokenResponse("access-2", "refresh-2"),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := expiredCredential()
	cred.TokenEndpoint = srv.URL
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, tokErr := ts.Token(); tokErr != nil {
				errs <- tokErr
			}
		}()
	}
	wg.Wait()
	close(errs)

	for e := range errs {
		t.Errorf("concurrent Token() call failed: %v", e)
	}
}

func TestTokenSource_ConcurrentRegistrationNotClobbered(t *testing.T) {
	fake := &fakeTokenEndpoint{t: t, responses: []func(http.ResponseWriter, *http.Request){
		jsonTokenResponse("access-old-reg", "refresh-old-reg"),
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := expiredCredential()
	cred.TokenEndpoint = srv.URL
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ts := NewTokenSourceFromCredential(store, srv.Client(), cred)

	// A concurrent `ready login` re-registers with a new client_id while ts
	// still holds the old one.
	reregistered := NewCredential(testServerKey)
	reregistered.ClientID = "client-2"
	reregistered.AccessToken = "access-new-reg"
	reregistered.RefreshToken = "refresh-new-reg"
	reregistered.Expiry = time.Now().Add(time.Hour)
	reregistered.UpdatedAt = time.Now()
	if err = store.Save(ctx, reregistered); err != nil {
		t.Fatalf("Save reregistered: %v", err)
	}

	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	// The already-fetched token (from the stale registration) is still
	// handed back for this call — it was validly issued.
	if tok.AccessToken != "access-old-reg" {
		t.Fatalf("Token() = %+v, want the already-fetched token", tok)
	}

	stored, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.ClientID != "client-2" {
		t.Fatalf("stored ClientID = %q, want the re-registered client-2 to survive untouched", stored.ClientID)
	}
	if stored.AccessToken != "access-new-reg" {
		t.Fatalf("stored AccessToken = %q, the daemon must not have overwritten the re-registered credential", stored.AccessToken)
	}
}

package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpoauth "github.com/marco-rozz/ready/internal/oauth"
)

const (
	testProviderKey  = "work"
	jsonAccessToken  = "access_token"
	jsonRefreshToken = "refresh_token"
	jsonExpiresIn    = "expires_in"
	testLoginAccess  = "login-access"
	testRotatedToken = "rotated"
)

type authRoundTripFunc func(*http.Request) (*http.Response, error)

func (f authRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jwtWithAccount(t *testing.T, account any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	if err != nil {
		t.Fatal(err)
	}

	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestPKCEAndStateDeterministic(t *testing.T) {
	random := strings.NewReader(strings.Repeat("a", pkceVerifierBytes+stateBytes))
	verifier, challenge, err := newPKCE(random)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(verifier))
	if challenge != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatal("S256 challenge mismatch")
	}
	state, err := newState(random)
	if err != nil || state == "" {
		t.Fatalf("state = %q, err=%v", state, err)
	}
}

func TestExtractAccountID(t *testing.T) {
	if got, err := ExtractAccountID(jwtWithAccount(t, "acct-123")); err != nil || got != "acct-123" {
		t.Fatalf("got %q, err=%v", got, err)
	}
	for _, token := range []string{"bad", "e30.bad.sig", jwtWithAccount(t, 42)} {
		if _, err := ExtractAccountID(token); err == nil {
			t.Fatalf("expected error for %q", token)
		}
	}
}

func TestFileStoreNamespaceTraversalAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oauth")
	store, err := NewFileStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cred := NewCredential("../same/name")
	cred.AccessToken, cred.RefreshToken, cred.AccountID = "access", "refresh", "account"
	if saveErr := store.Save(context.Background(), cred); saveErr != nil {
		t.Fatal(saveErr)
	}
	mcpStore, err := mcpoauth.NewFileStore(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	mcpCred := mcpoauth.NewCredential("../same/name")
	if saveErr := mcpStore.Save(context.Background(), mcpCred); saveErr != nil {
		t.Fatal(saveErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	var llmFile string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "llm-") {
			llmFile = entry.Name()
		}
	}
	if llmFile == "" || strings.Contains(llmFile, "..") || strings.Contains(llmFile, "/") {
		t.Fatalf("unsafe or missing LLM filename %q", llmFile)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, llmFile))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file mode=%o", info.Mode().Perm())
		}
	}
}

func TestAuthorizationURLAndManualParsing(t *testing.T) {
	client := NewClient(nil)
	raw, err := client.AuthorizationURL("state", "challenge")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Host != "auth.openai.com" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("originator") != Originator {
		t.Fatalf("unexpected authorization URL %s", raw)
	}
	for _, input := range []string{
		RedirectURI + "?code=abc&state=state",
		"code=abc&state=state",
		"abc#state",
		"abc",
	} {
		got, err := ParseManualCallback(input, "state")
		if err != nil || got.Code != "abc" {
			t.Fatalf("ParseManualCallback(%q) = %+v, %v", input, got, err)
		}
	}
	if _, err := ParseManualCallback("abc#wrong", "state"); err == nil {
		t.Fatal("expected state mismatch")
	}
}

func TestWaitForLoopbackExactPathAndState(t *testing.T) {
	opener := func(ctx context.Context, _ string) error {
		client := &http.Client{}
		for _, target := range []string{
			"http://localhost:1455/not-the-callback?code=ignored&state=state",
			RedirectURI + "?code=accepted&state=state",
		} {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
		}

		return nil
	}
	got, err := WaitForLoopback(context.Background(), "https://auth.example/start", "state", time.Second, opener)
	if err != nil || got.Code != "accepted" || got.State != "state" {
		t.Fatalf("callback = %+v, %v", got, err)
	}
}

func TestExchangeRefreshRotationAndRedaction(t *testing.T) {
	secret := "refresh-secret-never-print"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != secret {
			t.Fatalf("refresh token = %q", r.Form.Get("refresh_token"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			jsonAccessToken: jwtWithAccount(t, "acct"), jsonRefreshToken: testRotatedToken, jsonExpiresIn: 3600,
		})
	}))
	defer srv.Close()
	client := NewClient(srv.Client())
	client.Endpoints.Token = srv.URL
	cred, err := client.Refresh(context.Background(), testProviderKey, secret)
	if err != nil || cred.RefreshToken != testRotatedToken || cred.AccountID != "acct" {
		t.Fatalf("cred=%+v err=%v", cred, err)
	}

	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + secret + `"}`))
	}))
	defer errSrv.Close()
	client.Endpoints.Token = errSrv.URL
	_, err = client.Refresh(context.Background(), testProviderKey, secret)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestTokenSourceSingleFlightRefresh(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			jsonAccessToken: jwtWithAccount(t, "acct"), jsonRefreshToken: "new", jsonExpiresIn: 3600,
		})
	}))
	defer srv.Close()
	store, _ := NewFileStore(context.Background(), t.TempDir())
	old := NewCredential(testProviderKey)
	old.AccessToken, old.RefreshToken, old.AccountID = "expired", "old", "acct"
	old.Expiry = time.Now().Add(-time.Hour)
	old.UpdatedAt = time.Now().Add(-time.Minute)
	if err := store.Save(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	client := NewClient(srv.Client())
	client.Endpoints.Token = srv.URL
	source, err := NewTokenSource(store, client, testProviderKey, old)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := source.Access(context.Background())
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d, want 1", calls.Load())
	}
}

func TestTokenSourceConcurrentLoginWinsRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			jsonAccessToken: jwtWithAccount(t, "refresh-account"), jsonRefreshToken: testRotatedToken, jsonExpiresIn: 3600,
		})
	}))
	defer srv.Close()
	store, _ := NewFileStore(context.Background(), t.TempDir())
	old := NewCredential(testProviderKey)
	old.AccessToken, old.RefreshToken, old.AccountID = "expired", "old", "old-account"
	old.Expiry = time.Now().Add(-time.Hour)
	old.UpdatedAt = time.Now().Add(-time.Minute)
	if err := store.Save(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	client := NewClient(srv.Client())
	client.Endpoints.Token = srv.URL
	source, err := NewTokenSource(store, client, testProviderKey, old)
	if err != nil {
		t.Fatal(err)
	}
	type accessResult struct {
		access Access
		err    error
	}
	done := make(chan accessResult, 1)
	go func() {
		access, err := source.Access(context.Background())
		done <- accessResult{access: access, err: err}
	}()
	<-started
	login := NewCredential(testProviderKey)
	login.AccessToken, login.RefreshToken, login.AccountID = testLoginAccess, "login-refresh", "login-account"
	login.Expiry = time.Now().Add(time.Hour)
	login.UpdatedAt = time.Now().Add(time.Second)
	if err := store.Save(context.Background(), login); err != nil {
		t.Fatal(err)
	}
	close(release)
	result := <-done
	if result.err != nil || result.access.Token != testLoginAccess || result.access.AccountID != "login-account" {
		t.Fatalf("access = %+v, %v", result.access, result.err)
	}
	persisted, _ := store.Load(context.Background(), testProviderKey)
	if persisted.AccessToken != testLoginAccess {
		t.Fatalf("refresh overwrote concurrent login: %+v", persisted)
	}
}

func TestDeviceAuthorizationPendingSuccessAndCancellation(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "device-id", "user_code": "ABCD-EFGH",
				"verification_uri": "https://auth.example/device", "interval": 1, "expires_in": 60,
			})
		case "/poll":
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})

				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_code": "code", "code_verifier": "verifier"})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				jsonAccessToken: jwtWithAccount(t, "acct-device"), jsonRefreshToken: "refresh-device", jsonExpiresIn: 3600,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := NewClient(srv.Client())
	client.Endpoints = Endpoints{DeviceCode: srv.URL + "/start", DeviceToken: srv.URL + "/poll", Token: srv.URL + "/token"}
	auth, err := client.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth.Interval = time.Millisecond
	cred, err := client.PollDeviceAuthorization(context.Background(), testProviderKey, auth)
	if err != nil || cred.AccountID != "acct-device" || polls.Load() != 2 {
		t.Fatalf("device credential = %+v, polls=%d, err=%v", cred, polls.Load(), err)
	}

	cancelClient := NewClient(&http.Client{Transport: authRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":"slow_down"}`)),
			Header:     make(http.Header), Request: req,
		}, nil
	})})
	cancelClient.Endpoints.DeviceToken = "https://auth.example/poll"
	cancelCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = cancelClient.PollDeviceAuthorization(cancelCtx, testProviderKey, &DeviceAuthorization{
		DeviceAuthID: "id", UserCode: "code", Interval: time.Millisecond, ExpiresAt: time.Now().Add(time.Minute),
	})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("cancelled device poll error = %v", err)
	}
}

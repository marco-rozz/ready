package oauth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

func TestSecretStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := newTestStore(t, t.TempDir())
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = "abc"
	cred.AccessToken = "tok"
	cred.UpdatedAt = time.Now()

	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.ClientID != "abc" || got.Version != credentialVersion {
		t.Fatalf("Load returned %+v", got)
	}
}

func TestSecretStore_LoadAbsent(t *testing.T) {
	ctx := context.Background()
	store, err := newTestStore(t, t.TempDir())
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}

	got, err := store.Load(ctx, "nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for absent record, got %+v", got)
	}
}

func TestSecretStore_Delete(t *testing.T) {
	ctx := context.Background()
	store, err := newTestStore(t, t.TempDir())
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err = store.Delete(ctx, testServerKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestSecretStore_DeleteAbsentIsNotError(t *testing.T) {
	ctx := context.Background()
	store, err := newTestStore(t, t.TempDir())
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}

	if err = store.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete of absent record should not error, got: %v", err)
	}
}

func TestSecretStore_TraversalServerKeyStaysInsideLocks(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "agent")
	store, err := newTestStore(t, dir)
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	if err = store.Save(ctx, &Credential{ServerKey: "../../etc/passwd", ClientID: "c"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(ctx, "../../etc/passwd")
	if err != nil || got == nil || got.ClientID != "c" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "etc")); err == nil {
		t.Fatal("a traversal server key escaped the agent directory")
	}
}

func TestSecretStore_ReadOnlyNeverCreates(t *testing.T) {
	dir := t.TempDir()
	secrets, err := secretstore.New(secretstore.Options{Dir: dir, Key: secretstore.KeySpec{Location: secretstore.KeyAgentDir}, AgentID: testStoreAgentID, ReadOnly: true, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	store := NewSecretStore(secrets, dir)
	if got, err := store.Load(context.Background(), "s"); got != nil || err != nil {
		t.Fatalf("Load on a missing store = %v, %v", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("read-only access created %v", entries)
	}
}

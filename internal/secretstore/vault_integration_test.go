//go:build darwin || windows

package secretstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// TestVaultRoundTrip exercises the real OS vault with a throwaway account.
func TestVaultRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account := "test-" + uuid.NewString()
	t.Cleanup(func() { _ = vaultDelete(context.Background(), account) })

	if _, err := vaultGet(ctx, account); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("get before set = %v, want ErrKeyNotFound", err)
	}
	id, _ := GenerateIdentity()
	b := vaultKey{account: account}
	if err := b.save(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := b.save(ctx, id); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second save = %v, want ErrKeyExists", err)
	}
	got, err := b.load(ctx)
	if err != nil || got.String() != id.String() {
		t.Fatalf("load = %v, %v", got, err)
	}
	if err := b.remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.remove(ctx); err != nil {
		t.Fatalf("removing a missing entry must succeed: %v", err)
	}
}

func TestVaultKeyedStoreRefusesOrphanEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	account := uuid.NewString()
	t.Cleanup(func() { _ = vaultDelete(context.Background(), account) })
	id, _ := GenerateIdentity()
	if err := vaultSet(ctx, account, id.String()); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Dir: t.TempDir(), Key: KeySpec{Location: KeyOSVault, AgentID: account}, AgentID: account, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, setSecret("a", "v")); !errors.Is(err, ErrKeyOrphanVault) {
		t.Fatalf("Update = %v, want ErrKeyOrphanVault", err)
	}
}

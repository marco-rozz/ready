package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func rec(s string) func(uint64) (json.RawMessage, error) {
	return func(uint64) (json.RawMessage, error) { return json.RawMessage(`"` + s + `"`), nil }
}

func TestCredentialGenerationsAndTombstones(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, t.TempDir())

	if _, _, ok, err := s.Credential(ctx, "mcp:a"); ok || err != nil {
		t.Fatalf("absent store: ok=%v err=%v", ok, err)
	}
	if g, err := s.Generation(ctx, "mcp:a"); g != 0 || err != nil {
		t.Fatalf("absent generation = %d, %v", g, err)
	}
	g1, err := s.PutCredential(ctx, "mcp:a", nil, rec("one"))
	if err != nil || g1 != 1 {
		t.Fatalf("first put = %d, %v", g1, err)
	}
	g2, _ := s.PutCredential(ctx, "mcp:a", nil, rec("two"))
	if g2 != 2 {
		t.Fatalf("second put gen = %d", g2)
	}
	raw, gen, ok, _ := s.Credential(ctx, "mcp:a")
	if !ok || gen != 2 || string(raw) != `"two"` {
		t.Fatalf("credential = %s gen %d ok %v", raw, gen, ok)
	}

	if err = s.RemoveCredential(ctx, "mcp:a"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ = s.Credential(ctx, "mcp:a"); ok {
		t.Fatal("record survived removal")
	}
	if g, _ := s.Generation(ctx, "mcp:a"); g != 3 {
		t.Fatalf("tombstone generation = %d, want 3", g)
	}
	// Work snapshotted before the delete must not resurrect the record.
	stale := uint64(2)
	if _, err = s.PutCredential(ctx, "mcp:a", &stale, rec("zombie")); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale commit = %v, want ErrGenerationConflict", err)
	}
	cur := uint64(3)
	if g, err := s.PutCredential(ctx, "mcp:a", &cur, rec("fresh")); err != nil || g != 4 {
		t.Fatalf("fresh commit = %d, %v", g, err)
	}
}

func TestRemoveCredentialOnMissingStoreCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := newStore(t, dir).RemoveCredential(context.Background(), "mcp:x"); err != nil {
		t.Fatal(err)
	}
	if newStore(t, dir).Exists() {
		t.Fatal("removing from a missing store created it")
	}
}

func TestCredentialsAreIsolatedFromSecrets(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, t.TempDir())
	_ = s.Update(ctx, setSecret("a", "v"))
	if _, err := s.PutCredential(ctx, "llm:p", nil, rec("x")); err != nil {
		t.Fatal(err)
	}
	p, _ := s.View(ctx)
	if p.Secrets["a"].Value != "v" || len(p.Credentials) != 1 {
		t.Fatalf("payload = %+v", p)
	}
}

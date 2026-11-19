package errdigest

import (
	"context"
	"errors"
	"testing"
)

func TestTurnGuardSerializesEquivalentCalls(t *testing.T) {
	ctx := WithTurnGuard(context.Background(), 2)
	g := guardFromContext(ctx)
	key := fingerprint("tool", `{"b":2,"a":1}`)
	if key != fingerprint("tool", `{"a":1,"b":2}`) {
		t.Fatal("equivalent arguments must share a fingerprint")
	}
	if ok, err := g.begin(ctx, key); !ok || err != nil {
		t.Fatalf("first call: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := g.begin(ctx, key); done <- err }()
	g.finish(key, true)
	if err := <-done; !errors.Is(err, ErrGuardRepeated) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := g.begin(ctx, fingerprint("other", `{}`)); err != nil {
		t.Fatalf("unrelated call: %v", err)
	}
	if _, err := g.begin(ctx, fingerprint("third", `{}`)); !errors.Is(err, ErrGuardCapacity) {
		t.Fatalf("capacity: %v", err)
	}
}

func TestTurnGuardTransientAllowsRetryAndTurnsAreIsolated(t *testing.T) {
	ctx := WithTurnGuard(context.Background(), 1)
	g := guardFromContext(ctx)
	key := fingerprint("tool", `{}`)
	_, _ = g.begin(ctx, key)
	g.finish(key, false)
	if ok, err := g.begin(ctx, key); !ok || err != nil {
		t.Fatalf("retry: %v", err)
	}
	g.finish(key, true)
	other := guardFromContext(WithTurnGuard(context.Background(), 1))
	if ok, err := other.begin(ctx, key); !ok || err != nil {
		t.Fatalf("new turn: %v", err)
	}
}

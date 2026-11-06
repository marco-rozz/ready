//nolint:goconst // literal fixtures are clearer inline than behind constants
package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

type entryFixture struct {
	e       *SecretEntry
	store   *secretstore.Store
	refs    []secretstore.Reference
	now     time.Time
	adopted []string
	changes int
}

func newEntryFixture(t *testing.T, refs ...secretstore.Reference) *entryFixture {
	t.Helper()
	store, err := secretstore.New(secretstore.Options{
		Dir:     t.TempDir(),
		Key:     secretstore.KeySpec{Location: secretstore.KeyAgentDir},
		AgentID: testAgentID,
		Log:     zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &entryFixture{store: store, refs: refs, now: time.Unix(1_000_000, 0)}
	f.e = NewSecretEntry(SecretEntryOptions{
		Store:       store,
		References:  func(context.Context) ([]secretstore.Reference, error) { return f.refs, nil },
		ZMQName:     func(context.Context) string { return "zmq" },
		AdoptZMQ:    func(v string) { f.adopted = append(f.adopted, v) },
		OnChange:    func(context.Context) { f.changes++ },
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return f.now },
	})

	return f
}

func providerRef(name string) secretstore.Reference {
	return secretstore.Reference{Field: "llm.providers.p.api_key", Name: name}
}

func TestSecretEntryBeginNeedsPendingOrName(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t)
	if _, err := f.e.Begin(ctx, "c1", ""); !errors.Is(err, ErrNoPendingSecrets) {
		t.Fatalf("nothing pending = %v", err)
	}
	f.refs = []secretstore.Reference{providerRef("key")}
	res, err := f.e.Begin(ctx, "c1", "")
	if err != nil || res.OperationID == "" || res.Outcome != OutcomeActive {
		t.Fatalf("begin = %+v, %v", res, err)
	}
	if _, err = f.e.Begin(ctx, "c2", ""); !errors.Is(err, ErrSecretEntryActive) {
		t.Fatalf("second operation = %v, want ErrSecretEntryActive", err)
	}
	if _, err = f.e.Begin(ctx, "c1", "nope"); !errors.Is(err, ErrSecretEntryActive) && !errors.Is(err, ErrUnknownName) {
		t.Fatalf("unknown name = %v", err)
	}
}

func TestSecretEntrySubmitWritesOnlyTheStoreAndNeverEchoes(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t, providerRef("key"))
	op, _ := f.e.Begin(ctx, "c1", "")
	const value = "sk-the-secret-value"
	res, err := f.e.Submit(ctx, "c1", op.OperationID, "key", value)
	if err != nil || res.Outcome != OutcomeCommitted {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	p, _ := f.store.View(ctx)
	if p.Secrets["key"].Value != value {
		t.Fatal("value not stored exactly as entered")
	}
	if f.changes != 1 {
		t.Fatalf("OnChange calls = %d", f.changes)
	}
	list, _ := f.e.List(ctx)
	for _, s := range list.Secrets {
		if s.Name == "key" && s.State != secretstore.StateSet {
			t.Fatalf("state after write = %s", s.State)
		}
	}
	if strings.Contains(strings.Join(SortedNames(secretstore.Report{Secrets: list.Secrets}), ","), value) {
		t.Fatal("a value leaked into list metadata")
	}
}

func TestSecretEntryValueRulesUseTheUnionOfFields(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t,
		providerRef("shared"),
		secretstore.Reference{Field: secretstore.FieldZMQAuthToken, Name: "shared"},
	)
	op, _ := f.e.Begin(ctx, "c1", "")
	res, err := f.e.Submit(ctx, "c1", op.OperationID, "shared", "short")
	if !errors.Is(err, ErrInvalidValue) || res.RuleCode != secretstore.RuleTooShort {
		t.Fatalf("short value on a ZMQ-shared name = %+v, %v", res, err)
	}
	if strings.Contains(err.Error(), "short") && strings.Contains(err.Error(), "=short") {
		t.Fatalf("error echoes the value: %v", err)
	}
	if f.store.Exists() {
		t.Fatal("a refused value created the store")
	}
	if _, err = f.e.Submit(ctx, "c1", op.OperationID, "shared", strings.Repeat("z", 40)); err != nil {
		t.Fatalf("conforming value: %v", err)
	}
}

func TestSecretEntryExpiryOwnershipAndEnd(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t, providerRef("key"))
	op, _ := f.e.Begin(ctx, "c1", "")
	if _, err := f.e.Submit(ctx, "c2", op.OperationID, "key", "v"); !errors.Is(err, ErrSecretEntryExpired) {
		t.Fatalf("foreign owner = %v", err)
	}
	f.now = f.now.Add(2 * time.Minute)
	if _, err := f.e.Submit(ctx, "c1", op.OperationID, "key", "v"); !errors.Is(err, ErrSecretEntryExpired) {
		t.Fatalf("idle operation = %v, want expiry", err)
	}
	op2, err := f.e.Begin(ctx, "c3", "")
	if err != nil {
		t.Fatalf("a new operation after expiry: %v", err)
	}
	f.e.Release("c3")
	if err = f.e.End("c3", op2.OperationID); !errors.Is(err, ErrSecretEntryExpired) {
		t.Fatalf("End after Release = %v", err)
	}
}

func TestSecretEntryRotationAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t, providerRef("key"))
	_ = f.store.Update(ctx, func(p *secretstore.Payload) error {
		p.Secrets["key"] = secretstore.Secret{Value: "old"}
		p.Secrets["orphan"] = secretstore.Secret{Value: "x"}

		return nil
	})
	// Rotation works with nothing pending when a name is given.
	op, err := f.e.Begin(ctx, "c1", "key")
	if err != nil {
		t.Fatalf("rotation begin: %v", err)
	}
	if _, err = f.e.Submit(ctx, "c1", op.OperationID, "key", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Delete(ctx, "key"); !errors.Is(err, ErrNameInUse) {
		t.Fatalf("delete a referenced name = %v", err)
	}
	if _, err = f.e.Delete(ctx, "missing"); !errors.Is(err, ErrUnknownName) {
		t.Fatalf("delete an unknown name = %v", err)
	}
	if res, err := f.e.Delete(ctx, "orphan"); err != nil || res.Outcome != OutcomeDeleted {
		t.Fatalf("delete unused = %+v, %v", res, err)
	}
	p, _ := f.store.View(ctx)
	if _, ok := p.Secrets["orphan"]; ok || p.Secrets["key"].Value != "new" {
		t.Fatalf("payload = %+v", p.Secrets)
	}
}

func TestSecretEntryAdoptsTheZMQSecretOnWrite(t *testing.T) {
	ctx := context.Background()
	f := newEntryFixture(t, secretstore.Reference{Field: secretstore.FieldZMQAuthToken, Name: "zmq"})
	op, _ := f.e.Begin(ctx, "c1", "")
	secret := strings.Repeat("a", 40)
	if _, err := f.e.Submit(ctx, "c1", op.OperationID, "zmq", secret); err != nil {
		t.Fatal(err)
	}
	if len(f.adopted) != 1 || f.adopted[0] != secret {
		t.Fatalf("adopted = %v", f.adopted)
	}
}

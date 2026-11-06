package secretstore

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckValueRules(t *testing.T) {
	zmq := []string{FieldZMQAuthToken}
	if err := CheckValue(zmq, strings.Repeat("a", 31)); RuleCode(err) != RuleTooShort {
		t.Fatalf("31 chars = %v", err)
	}
	if err := CheckValue(zmq, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("32 chars = %v", err)
	}
	if err := CheckValue([]string{"llm.providers.x.api_key"}, "k"); err != nil {
		t.Fatalf("short non-zmq = %v", err)
	}
	if err := CheckValue([]string{"x"}, ""); RuleCode(err) != RuleEmpty {
		t.Fatalf("empty = %v", err)
	}
	// Whitespace is part of the value, never trimmed.
	if err := CheckValue([]string{"x"}, " "); err != nil {
		t.Fatalf("a space is a value: %v", err)
	}
	// The rule applies over the union of referencing fields.
	if err := CheckValue([]string{"a", FieldZMQAuthToken}, "short"); RuleCode(err) != RuleTooShort {
		t.Fatalf("union = %v", err)
	}
}

func TestRuleViolationNeverEchoesValue(t *testing.T) {
	err := CheckValue([]string{FieldZMQAuthToken}, "hunter2")
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error echoes the value: %v", err)
	}
}

func TestNormalizeFileSecret(t *testing.T) {
	cases := map[string]string{
		"v\n":     "v",
		"v\r\n":   "v",
		"v\n\n":   "v\n",
		" v \n":   " v ", //nolint:gocritic // the leading space IS the case under test
		"v":       "v",
		"\n":      "",
		"v\r\r\n": "v\r",
	}
	for in, want := range cases {
		if got := NormalizeFileSecret([]byte(in)); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestReadFileSecret(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadFileSecret(filepath.Join(dir, "missing")); ok || err != nil {
		t.Fatalf("missing = %v, %v", ok, err)
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	if _, ok, _ := ReadFileSecret(empty); ok {
		t.Fatal("empty-after-normalisation must be pending external")
	}
	f := filepath.Join(dir, "f")
	_ = os.WriteFile(f, []byte("val\n"), 0o600)
	if v, ok, _ := ReadFileSecret(f); !ok || v != "val" {
		t.Fatalf("got %q, %v", v, ok)
	}
}

func TestDerive(t *testing.T) {
	p := NewPayload("a")
	p.Secrets["ok"] = Secret{Value: "v"}
	p.Secrets["zmq"] = Secret{Value: "short"}
	p.Secrets["orphan"] = Secret{Value: "v"}
	refs := []Reference{
		{Field: "llm.providers.p.api_key", Name: "ok"},
		{Field: "channels.telegram.token", Name: "absent"},
		{Field: FieldZMQAuthToken, Name: "zmq"},
		{Field: "home_assistant.websocket.token", File: "/no/such/file"},
	}
	r := Derive(p, refs, nil)
	got := map[string]SecretStatus{}
	for _, s := range r.Secrets {
		got[s.Name] = s
	}
	if got["ok"].State != StateSet || got["absent"].State != StatePending ||
		got["orphan"].State != StateUnused {
		t.Fatalf("states = %+v", got)
	}
	if got["zmq"].State != StatePending || got["zmq"].Rule != RuleTooShort {
		t.Fatalf("non-conforming must be pending with a rule: %+v", got["zmq"])
	}
	if len(r.External) != 1 || r.External[0].State != StatePendingExternal {
		t.Fatalf("external = %+v", r.External)
	}
	if n := r.PendingNames(); len(n) != 2 {
		t.Fatalf("PendingNames = %v", n)
	}
	if len(r.PendingFields()) != 3 {
		t.Fatalf("PendingFields = %v", r.PendingFields())
	}
	// No store at all: every referenced name is pending.
	if n := Derive(nil, refs, nil).PendingNames(); len(n) != 3 {
		t.Fatalf("nil store PendingNames = %v", n)
	}
}

func TestWatchFiresOnlyForStoreFile(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	fired := make(chan struct{}, 4)
	if err := Watch(ctx, dir, func() { fired <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o600)
	select {
	case <-fired:
		t.Fatal("fired for an unrelated file")
	case <-time.After(400 * time.Millisecond):
	}
	if err := newStore(t, dir).Update(ctx, setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("did not fire for secrets.age")
	}
}

func TestPermissionWarnings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	_ = newStore(t, dir).Update(context.Background(), setSecret("a", "v"))
	if w := PermissionWarnings(dir); len(w) != 0 {
		t.Fatalf("fresh store warns: %v", w)
	}
	_ = os.Chmod(filepath.Join(dir, KeyFileName), 0o644)
	ext := filepath.Join(dir, "ext")
	_ = os.WriteFile(ext, []byte("x"), 0o640) //nolint:gosec // group-readable on purpose: the case under test
	if w := PermissionWarnings(dir, ext); len(w) != 2 {
		t.Fatalf("warnings = %v", w)
	}
}

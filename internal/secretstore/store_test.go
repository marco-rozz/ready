package secretstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/rs/zerolog"
)

const testAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

func newStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := New(Options{Dir: dir, Key: KeySpec{Location: KeyAgentDir}, AgentID: testAgentID, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func setSecret(name, value string) func(*Payload) error {
	return func(p *Payload) error {
		p.Secrets[name] = Secret{Value: value}

		return nil
	}
}

func TestFirstWriteCreatesStoreAndKeyTogether(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)
	if _, err := s.View(context.Background()); !errors.Is(err, ErrNoStore) {
		t.Fatalf("View before first write = %v, want ErrNoStore", err)
	}
	if err := s.Update(context.Background(), setSecret("a", "  v  ")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{StoreFileName, KeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s missing: %v", f, err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(dir, StoreFileName))
	if bytes.Contains(raw, []byte("  v  ")) {
		t.Fatal("store holds a plaintext value")
	}
	p, err := newStore(t, dir).View(context.Background())
	if err != nil || p.Secrets["a"].Value != "  v  " {
		t.Fatalf("fresh View = %+v, %v", p, err)
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{StoreFileName, KeyFileName} {
			if fi, _ := os.Stat(filepath.Join(dir, f)); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", f, fi.Mode().Perm())
			}
		}
	}
}

func TestMissingKeyNeverCreatesNewKey(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)
	if err := s.Update(context.Background(), setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	fresh := newStore(t, dir)
	if _, err := fresh.View(context.Background()); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("View = %v, want ErrKeyNotFound", err)
	}
	if err := fresh.Update(context.Background(), setSecret("b", "v")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Update = %v, want ErrKeyNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); err == nil {
		t.Fatal("a new key was created over an existing store")
	}
}

func TestOrphanAgentDirKeyIsReused(t *testing.T) {
	dir := t.TempDir()
	id, _ := GenerateIdentity()
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newStore(t, dir).Update(context.Background(), setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, KeyFileName))
	if string(bytes.TrimSpace(got)) != id.String() {
		t.Fatal("orphan key was replaced")
	}
}

func TestAgentMismatchRefused(t *testing.T) {
	dir := t.TempDir()
	if err := newStore(t, dir).Update(context.Background(), setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	other, _ := New(Options{Dir: dir, Key: KeySpec{Location: KeyAgentDir}, AgentID: "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0002", Log: zerolog.Nop()})
	_, err := other.View(context.Background())
	if !errors.Is(err, ErrAgentMismatch) {
		t.Fatalf("err = %v, want ErrAgentMismatch", err)
	}
	if !bytes.Contains([]byte(err.Error()), []byte(testAgentID)) {
		t.Fatalf("mismatch must name both ids: %v", err)
	}
}

func TestPrevFallbackOnCorruptStore(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)
	_ = s.Update(context.Background(), setSecret("a", "one"))
	_ = s.Update(context.Background(), setSecret("a", "two"))
	if err := os.WriteFile(filepath.Join(dir, StoreFileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := newStore(t, dir).View(context.Background())
	if err != nil || p.Secrets["a"].Value != "one" {
		t.Fatalf("View = %+v, %v; want the previous generation", p, err)
	}
}

func TestBothGenerationsCorruptFailsAtBoot(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)
	_ = s.Update(context.Background(), setSecret("a", "one"))
	_ = s.Update(context.Background(), setSecret("a", "two"))
	for _, f := range []string{StoreFileName, StoreFileName + prevSuffix} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("garbage"), 0o600)
	}
	if _, err := newStore(t, dir).View(context.Background()); err == nil {
		t.Fatal("expected a boot refusal")
	}
}

func TestRuntimeKeepsLastGoodView(t *testing.T) {
	dir := t.TempDir()
	s := newStore(t, dir)
	_ = s.Update(context.Background(), setSecret("a", "good"))
	if _, err := s.View(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{StoreFileName, StoreFileName + prevSuffix} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("garbage"), 0o600)
	}
	p, err := s.View(context.Background())
	if err != nil || p.Secrets["a"].Value != "good" {
		t.Fatalf("View = %+v, %v; want the last good view", p, err)
	}
}

func TestConcurrentUpdatesAreSerialised(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s := newStore(t, dir) // separate instances: only the file lock can serialise them
			err := s.Update(context.Background(), setSecret("k"+strconv.Itoa(i), "v"))
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	p, err := newStore(t, dir).View(context.Background())
	if err != nil || len(p.Secrets) != n {
		t.Fatalf("lost updates: %d/%d secrets, err %v", len(p.Secrets), n, err)
	}
}

func TestFileKeyIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ext.key")
	s, err := New(Options{Dir: dir, Key: KeySpec{Location: KeyFile, File: keyPath}, AgentID: testAgentID, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), setSecret("a", "v")); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Update without external key = %v, want ErrKeyNotFound", err)
	}
	id, _ := GenerateIdentity()
	_ = os.WriteFile(keyPath, []byte("# systemd credential\n"+id.String()+"\n"), 0o600)
	if err := s.Update(context.Background(), setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); err == nil {
		t.Fatal("agent_dir key written for a file location")
	}
}

func TestKeySpecValidation(t *testing.T) {
	if err := (KeySpec{Location: KeyFile, File: "relative"}).Validate(); err == nil {
		t.Error("relative key file accepted")
	}
	if err := (KeySpec{Location: "nope"}).Validate(); err == nil {
		t.Error("unknown location accepted")
	}
	if err := (KeySpec{Location: KeyAgentDir, File: "/x"}).Validate(); err == nil {
		t.Error("store_key_file accepted with agent_dir")
	}
	err := (KeySpec{Location: KeyOSVault, AgentID: testAgentID}).Validate()
	if !vaultSupported && !errors.Is(err, ErrVaultUnsupported) {
		t.Errorf("os_vault on an unsupported platform = %v", err)
	}
}

func TestReadOnlyStoreNeverCreates(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(Options{Dir: dir, Key: KeySpec{Location: KeyAgentDir}, AgentID: testAgentID, ReadOnly: true, Log: zerolog.Nop()})
	if err := s.Update(context.Background(), setSecret("a", "v")); err == nil {
		t.Fatal("read-only store accepted a write")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("read-only store created files: %v", entries)
	}
}

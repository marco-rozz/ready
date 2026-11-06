package secretstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWrapUnwrapIdentity(t *testing.T) {
	id, _ := GenerateIdentity()
	blob, err := WrapIdentity(id, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !IsWrapped(blob) {
		t.Fatal("export is not recognised as wrapped")
	}
	if bytes.Contains(blob, []byte(id.String())) {
		t.Fatal("the wrapped export holds the key in plaintext")
	}
	got, err := UnwrapIdentity(blob, "correct horse")
	if err != nil || got.String() != id.String() {
		t.Fatalf("unwrap = %v, %v", got, err)
	}
	if _, err = UnwrapIdentity(blob, "wrong"); err == nil {
		t.Fatal("a wrong passphrase unwrapped the key")
	}
}

func TestWriteNewPrivateFileIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	if err := WriteNewPrivateFile(path, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := WriteNewPrivateFile(path, []byte("w")); err == nil {
		t.Fatal("an existing file was overwritten")
	}
	if got, _ := os.ReadFile(path); string(got) != "v" {
		t.Fatalf("content = %q", got)
	}
}

func TestInstallKeyVerifiesAgainstTheExistingStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newStore(t, dir)
	if err := s.Update(ctx, setSecret("a", "v")); err != nil {
		t.Fatal(err)
	}
	good, _ := s.Identity(ctx)
	other, _ := GenerateIdentity()

	// Lose the key, then try a wrong one and the right one.
	if err := os.Remove(filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	fresh := newStore(t, dir)
	if err := fresh.InstallKey(ctx, other); err == nil {
		t.Fatal("a key that does not decrypt the store was installed")
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); err == nil {
		t.Fatal("a refused key left a file behind")
	}
	if err := fresh.InstallKey(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := fresh.InstallKey(ctx, good); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("second install = %v, want ErrKeyExists", err)
	}
	if p, err := newStore(t, dir).View(ctx); err != nil || p.Secrets["a"].Value != "v" {
		t.Fatalf("store unreadable after import: %v", err)
	}
}

func TestInstallKeyAtAndRemoveExternalFileIsUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newStore(t, dir)
	_ = s.Update(ctx, setSecret("a", "v"))
	id, _ := s.Identity(ctx)

	ext := filepath.Join(t.TempDir(), "ext.key")
	if err := os.WriteFile(ext, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := KeySpec{Location: KeyFile, File: ext}
	if err := InstallKeyAt(ctx, dir, dest, testAgentID, id); err != nil {
		t.Fatalf("install at an existing external file: %v", err)
	}
	if err := RemoveKey(ctx, dir, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ext); err != nil {
		t.Fatal("RemoveKey touched an external file")
	}
}

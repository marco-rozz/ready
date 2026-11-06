package secretstore

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := encrypt(id, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("payload")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := decrypt(id, blob)
	if err != nil || string(got) != "payload" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	blob, err := encrypt(a, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decrypt(b, blob); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
}

func TestParseIdentityRoundTrip(t *testing.T) {
	id, _ := GenerateIdentity()
	got, err := ParseIdentity(id.String() + "\n")
	if err != nil || got.String() != id.String() {
		t.Fatalf("ParseIdentity = %v, %v", got, err)
	}
}

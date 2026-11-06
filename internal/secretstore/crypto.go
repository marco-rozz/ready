// Package secretstore implements the per-agent encrypted Secret store: an
// age-encrypted payload holding operator Secrets and login-managed OAuth
// Credentials, plus the Store key that decrypts it.
package secretstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// ErrDecrypt reports that a store blob could not be decrypted with the
// supplied Store key. It never carries plaintext or key material.
var ErrDecrypt = errors.New("secret store: cannot decrypt with the store key")

// GenerateIdentity creates a fresh X25519 age identity (the Store key).
func GenerateIdentity() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("failed to generate store key: %w", err)
	}

	return id, nil
}

// ParseIdentity parses the textual form of a Store key (AGE-SECRET-KEY-1…).
func ParseIdentity(s string) (*age.X25519Identity, error) {
	ids, err := age.ParseIdentities(strings.NewReader(s))
	if err != nil {
		return nil, fmt.Errorf("failed to parse store key: %w", err)
	}
	if len(ids) != 1 {
		return nil, errors.New("failed to parse store key: expected exactly one identity")
	}
	id, ok := ids[0].(*age.X25519Identity)
	if !ok {
		return nil, errors.New("failed to parse store key: not an X25519 identity")
	}

	return id, nil
}

// encrypt seals plaintext to the identity's single X25519 recipient.
func encrypt(id *age.X25519Identity, plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		return nil, fmt.Errorf("failed to start encryption: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("failed to encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("failed to finish encryption: %w", err)
	}

	return buf.Bytes(), nil
}

// decrypt opens a blob sealed by encrypt.
func decrypt(id *age.X25519Identity, blob []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(blob), id)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, err)
	}

	return out, nil
}

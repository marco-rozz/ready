package oauth

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/marco-rozz/ready/internal/secretstore"
)

const testStoreAgentID = "0190aaaa-bbbb-7ccc-8ddd-eeeeffff0001"

// newTestStore builds a SecretStore over a fresh Secret store under dir.
func newTestStore(t *testing.T, dir string) (*SecretStore, error) {
	t.Helper()

	return openTestStore(dir)
}

// openTestStore is newTestStore without a *testing.T, for re-exec helpers.
func openTestStore(dir string) (*SecretStore, error) {
	secrets, err := secretstore.New(secretstore.Options{
		Dir:     dir,
		Key:     secretstore.KeySpec{Location: secretstore.KeyAgentDir},
		AgentID: testStoreAgentID,
		Log:     zerolog.Nop(),
	})
	if err != nil {
		return nil, err
	}

	return NewSecretStore(secrets, dir), nil
}

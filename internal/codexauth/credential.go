// Package codexauth owns ChatGPT OAuth credentials used by READY's
// openai-codex LLM provider. It deliberately does not reuse the MCP OAuth
// credential schema: the two protocols have different identities and
// lifecycle metadata even though they share the configured storage directory.
package codexauth

import (
	"errors"
	"fmt"
	"time"
)

const credentialVersion = 1

var (
	// ErrLoginRequired means the named provider has no credential that the
	// non-interactive daemon can use or refresh.
	ErrLoginRequired = errors.New("codex oauth: login required")
	// ErrInvalidCredential means a persisted record is structurally corrupt or
	// belongs to another provider instance.
	ErrInvalidCredential = errors.New("codex oauth: invalid credential")
)

// Credential is the versioned on-disk record for one llm.providers entry.
// AccessToken and RefreshToken are secrets and must never be logged.
type Credential struct {
	Version      int       `json:"version"`
	ProviderKey  string    `json:"provider_key"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
	AccountID    string    `json:"chatgpt_account_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NewCredential constructs a record with the current storage version.
func NewCredential(providerKey string) *Credential {
	return &Credential{Version: credentialVersion, ProviderKey: providerKey}
}

// Validate checks the durable record shape and its ownership.
func (c *Credential) Validate(providerKey string) error {
	if c == nil {
		return fmt.Errorf("%w: record is absent", ErrInvalidCredential)
	}
	if c.Version != credentialVersion {
		return fmt.Errorf("%w: unsupported format version %d", ErrInvalidCredential, c.Version)
	}
	if c.ProviderKey == "" || c.ProviderKey != providerKey {
		return fmt.Errorf("%w: provider key mismatch", ErrInvalidCredential)
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return fmt.Errorf("%w: access token or refresh token is required", ErrInvalidCredential)
	}
	if c.AccountID == "" {
		return fmt.Errorf("%w: ChatGPT account ID is required", ErrInvalidCredential)
	}

	return nil
}

// Usable is the single network-free predicate shared by daemon construction
// and ready login --status. skew reserves time for an in-flight request.
func (c *Credential) Usable(providerKey string, now time.Time, skew time.Duration) bool {
	if c.Validate(providerKey) != nil {
		return false
	}
	if c.AccessToken != "" && (c.Expiry.IsZero() || c.Expiry.After(now.Add(skew))) {
		return true
	}

	return c.RefreshToken != ""
}

// PublicAccountID returns a non-secret, stable display form for status output.
func (c *Credential) PublicAccountID() string {
	if c == nil || c.AccountID == "" {
		return "-"
	}
	if len(c.AccountID) <= 8 {
		return c.AccountID
	}

	return c.AccountID[:4] + "…" + c.AccountID[len(c.AccountID)-4:]
}

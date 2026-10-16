package codexauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	pkceVerifierBytes = 48
	stateBytes        = 32
)

// NewPKCE generates an RFC 7636 verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string, err error) {
	return newPKCE(rand.Reader)
}

func newPKCE(random io.Reader) (verifier, challenge string, err error) {
	raw := make([]byte, pkceVerifierBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])

	return verifier, challenge, nil
}

// NewState returns a cryptographically random OAuth state value.
func NewState() (string, error) { return newState(rand.Reader) }

func newState(random io.Reader) (string, error) {
	raw := make([]byte, stateBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ExtractAccountID decodes only the JWT payload and extracts routing
// metadata. The payload is not treated as local authorization evidence.
func ExtractAccountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("decode ChatGPT account ID: access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode ChatGPT account ID: malformed JWT payload")
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode ChatGPT account ID: malformed JWT claims")
	}
	if account, ok := claims["chatgpt_account_id"].(string); ok && account != "" {
		return account, nil
	}
	if nested, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if account, ok := nested["chatgpt_account_id"].(string); ok && account != "" {
			return account, nil
		}
	}

	return "", fmt.Errorf("decode ChatGPT account ID: claim is absent or not a string")
}

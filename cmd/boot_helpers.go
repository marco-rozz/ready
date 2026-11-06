package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// randomHex renders n random bytes as lowercase hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate a random secret: %w", err)
	}

	return hex.EncodeToString(b), nil
}

func nowUTC() time.Time { return time.Now().UTC() }

package config

import (
	"context"
	"os"
	"testing"
)

// TestConfigExampleValidates guards the shipped reference config: every
// documented knob in config.example.yaml must still decode and validate.
func TestConfigExampleValidates(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Skipf("config.example.yaml not present: %v", err)
	}

	if _, verr := ValidateCandidate(context.Background(), data, "config.example.yaml", ProfileOverrideInputs{}, nil); verr != nil {
		t.Fatalf("config.example.yaml invalid: %v", verr)
	}
}

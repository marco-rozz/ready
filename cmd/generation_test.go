package main

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerationStageErrorAddsContextAndPreservesCause(t *testing.T) {
	cause := errors.New("original failure")

	err := generationStageError("prepare boot secrets", cause)
	if err == nil || !strings.Contains(err.Error(), "failed to prepare boot secrets") {
		t.Fatalf("error = %v, want stage context", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want wrapped original cause", err)
	}
}

package toolutil

import (
	"errors"
	"strings"
	"testing"
)

func TestWithRecoveryResp(t *testing.T) {
	t.Run("returns fn result when there is no panic", func(t *testing.T) {
		got, err := WithRecoveryResp(func() (int, error) {
			return 42, nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != 42 {
			t.Fatalf("unexpected result: got %d want %d", got, 42)
		}
	})

	t.Run("passes through returned error", func(t *testing.T) {
		wantErr := errors.New("fn error")
		got, err := WithRecoveryResp(func() (string, error) {
			return "ok", wantErr
		})

		if got != "ok" {
			t.Fatalf("unexpected result: got %q want %q", got, "ok")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected returned error to be preserved, got %v", err)
		}
	})

	t.Run("recovers panic error and returns zero value", func(t *testing.T) {
		wantErr := errors.New("boom")
		got, err := WithRecoveryResp(func() (map[string]int, error) {
			panic(wantErr)
		})

		if got != nil {
			t.Fatalf("expected zero value result, got %#v", got)
		}
		if err == nil {
			t.Fatal("expected an error")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected wrapped panic error to match original, got %v", err)
		}
		if !strings.Contains(err.Error(), "panic recovered:") {
			t.Fatalf("expected wrapped panic prefix, got %v", err)
		}
	})

	t.Run("recovers panic string and returns zero value", func(t *testing.T) {
		got, err := WithRecoveryResp(func() (int, error) {
			panic("kaboom")
		})

		if got != 0 {
			t.Fatalf("expected zero value result, got %d", got)
		}
		if err == nil {
			t.Fatal("expected an error")
		}
		if err.Error() != "panic recovered: kaboom" {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

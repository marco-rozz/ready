package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marco-rozz/ready/internal/daemonlock"
)

func acquireSharedForTest(t *testing.T, dir string) (*daemonlock.Lock, error) {
	t.Helper()

	return daemonlock.AcquireMaintenanceShared(context.Background(), dir)
}

// The hidden maintenance holder: refuses a busy directory, exits on stdin
// EOF, refuses a missing directory. The gate file itself is never unlinked.
func TestMaintenanceHolderBlockedAndReleased(t *testing.T) {
	gateDir := t.TempDir()

	// Acquire the shared gate first: exclusive must refuse.
	shared, err := acquireSharedForTest(t, gateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shared.Release() }()

	err = runMaintenanceHolder(context.Background(), gateDir, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "another ready process") {
		t.Fatalf("holder with a shared holder = %v, want the busy refusal", err)
	}

	_ = shared.Release()

	// Without any other holder it exits when stdin EOFs.
	err = runMaintenanceHolder(context.Background(), gateDir, strings.NewReader(""), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("holder after EOF = %v", err)
	}

	// And refuses a directory that does not exist.
	err = runMaintenanceHolder(context.Background(), filepath.Join(gateDir, "nope"), strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing dir = %v", err)
	}
}

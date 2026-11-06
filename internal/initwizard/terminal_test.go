//go:build !windows

package initwizard

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestTerminalCancellationRestoresState(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(map[bool]string{false: "visible", true: "secret"}[secret], func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = master.Close(); _ = slave.Close() }()
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var output bytes.Buffer
			done := make(chan error, 1)
			go func() {
				_, readErr := readTerminalLine(ctx, slave, &output, secret)
				done <- readErr
			}()
			deadline := time.Now().Add(2 * time.Second)
			for {
				state, stateErr := term.GetState(int(slave.Fd()))
				if stateErr != nil {
					t.Fatal(stateErr)
				}
				if !reflect.DeepEqual(before, state) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("terminal did not enter input mode")
				}
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case readErr := <-done:
				if !errors.Is(readErr, context.Canceled) {
					t.Fatalf("cancelled read: %v", readErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not interrupt terminal read")
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal state was not restored: %v", err)
			}
		})
	}
}

func TestTerminalSecretCtrlCAndSuccess(t *testing.T) {
	for _, answer := range []string{"\x03", "private-canary\r"} {
		master, slave, err := pty.Open()
		if err != nil {
			t.Fatal(err)
		}
		before, err := term.GetState(int(slave.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for ctx.Err() == nil {
				state, stateErr := term.GetState(int(slave.Fd()))
				if stateErr == nil && !reflect.DeepEqual(before, state) {
					_, _ = master.Write([]byte(answer))

					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		value, err := readTerminalLine(ctx, slave, &output, true)
		cancel()
		<-done
		after, stateErr := term.GetState(int(slave.Fd()))
		_ = master.Close()
		_ = slave.Close()
		if stateErr != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("secret input did not restore the terminal")
		}
		if answer == "\x03" {
			if !errors.Is(err, ErrCancelled) {
				t.Fatalf("Ctrl+C: %v", err)
			}
		} else if err != nil || value != "private-canary" || bytes.Contains(output.Bytes(), []byte(value)) {
			t.Fatal("secret was lost or echoed")
		}
	}
}

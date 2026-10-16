package tui

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// pipeFile returns a read end holding data, as a real *os.File so the wrapper
// under test sees the same type Bubble Tea would hand it.
func pipeFile(t *testing.T, data string) *os.File {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	t.Cleanup(func() { _ = r.Close() })

	go func() {
		defer w.Close()

		_, _ = io.WriteString(w, data)
	}()

	return r
}

func readOnce(t *testing.T, r io.Reader) string {
	t.Helper()

	buf := make([]byte, 4096)

	n, err := r.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("Read: %v", err)
	}

	return string(buf[:n])
}

func TestShiftEnterReaderRewritesLoneCRWhenShiftDown(t *testing.T) {
	t.Parallel()

	r := shiftEnterReader{f: pipeFile(t, "\r"), probe: func() bool { return true }}

	if got := readOnce(t, r); got != kittyShiftEnter {
		t.Errorf("lone CR with shift down = %q, want %q", got, kittyShiftEnter)
	}
}

func TestShiftEnterReaderPassesCRThroughWhenShiftUp(t *testing.T) {
	t.Parallel()

	r := shiftEnterReader{f: pipeFile(t, "\r"), probe: func() bool { return false }}

	if got := readOnce(t, r); got != "\r" {
		t.Errorf("lone CR with shift up = %q, want %q", got, "\r")
	}
}

// A CR that arrives inside a larger chunk (paste, fast typing, an escape
// sequence) is never rewritten: only a solitary CR is a keystroke we can
// attribute to the enter key.
func TestShiftEnterReaderIgnoresCRInsideChunk(t *testing.T) {
	t.Parallel()

	const chunk = "ab\r"

	r := shiftEnterReader{f: pipeFile(t, chunk), probe: func() bool { return true }}

	if got := readOnce(t, r); got != chunk {
		t.Errorf("multi-byte chunk = %q, want %q", got, chunk)
	}
}

func TestShiftEnterReaderProbeNotConsultedForOtherBytes(t *testing.T) {
	t.Parallel()

	probed := false
	probe := func() bool {
		probed = true

		return true
	}
	r := shiftEnterReader{f: pipeFile(t, "x"), probe: probe}

	if got := readOnce(t, r); got != "x" {
		t.Errorf("plain byte = %q, want %q", got, "x")
	}

	if probed {
		t.Error("probe was consulted for a byte that is not a lone CR")
	}
}

func TestWrapInputLeavesNonFileReadersAlone(t *testing.T) {
	t.Parallel()

	src := bytes.NewBufferString("\r")

	if got := wrapInput(src, func(string) string { return "" }); got != io.Reader(src) {
		t.Error("wrapInput wrapped a non-*os.File reader")
	}
}

func TestWrapInputRespectsKillSwitchAndSSH(t *testing.T) {
	t.Parallel()

	f := pipeFile(t, "")

	for _, tc := range []struct {
		name    string
		env     map[string]string
		wrapped bool
	}{
		{name: "off", env: map[string]string{shiftEnterEnv: "off"}},
		{name: "false", env: map[string]string{shiftEnterEnv: "FALSE"}},
		{name: "ssh connection", env: map[string]string{"SSH_CONNECTION": "10.0.0.1 22"}},
		{name: "ssh tty", env: map[string]string{"SSH_TTY": "/dev/pts/0"}},
		{
			name:    "ssh overridden by force",
			env:     map[string]string{"SSH_TTY": "/dev/pts/0", shiftEnterEnv: "force"},
			wrapped: probeAvailable(),
		},
		{name: "auto", env: map[string]string{}, wrapped: probeAvailable()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(k string) string { return tc.env[k] }

			_, wrapped := wrapInput(f, getenv).(shiftEnterReader)
			if wrapped != tc.wrapped {
				t.Errorf("wrapped = %v, want %v", wrapped, tc.wrapped)
			}
		})
	}
}

// probeAvailable reports whether this platform/session can answer the modifier
// query at all, so the table above asserts the gating logic rather than the
// availability of a display server or a framework.
func probeAvailable() bool { return newShiftProbe() != nil }

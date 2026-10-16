package tui

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
)

// kittyShiftEnter is the kitty-keyboard-protocol encoding of shift+enter.
// Bubble Tea's decoder maps it to KeyPressMsg{Code: KeyEnter, Mod: ModShift},
// i.e. Keystroke() == "shift+enter", which handleKey already binds to
// "insert a newline".
const kittyShiftEnter = "\x1b[13;2u"

// shiftEnterEnv is the operator kill switch for the fallback below:
// "off" disables it, "on"/"force" enables it unconditionally, anything else
// (including unset) selects the automatic behaviour.
const shiftEnterEnv = "READY_SHIFT_ENTER"

// shiftEnterReader rewrites a solitary CR into [kittyShiftEnter] when the OS
// reports the shift key physically down at that instant.
//
// Terminals that implement neither the kitty keyboard protocol nor xterm's
// modifyOtherKeys — macOS Terminal.app above all, and any multiplexer that
// forwards a bare CR — cannot express shift+enter in band: the byte on the
// wire is identical for enter and shift+enter. Bubble Tea negotiates both
// in-band protocols on its own, so this fallback only ever fires where they
// produced nothing, and it recovers the missing bit out of band from the
// operating system. Same mechanism pi-tui implements with a native addon; here
// the OS call is made without cgo (see the shiftstate_* files).
//
// The full term.File / cancelreader.File contract is implemented on purpose:
// Bubble Tea type-asserts the input reader to put the *real* fd in raw mode
// (tty_unix.go) and to build a kqueue/epoll-backed cancellable reader. A plain
// io.Reader wrapper would silently lose both.
type shiftEnterReader struct {
	f *os.File
	// probe reports whether shift is physically down. Never nil for a
	// constructed reader; injected in tests.
	probe func() bool
}

// Guarantee at compile time that the wrapper keeps satisfying the two
// contracts Bubble Tea and its cancel reader assert against.
var (
	_ term.File         = shiftEnterReader{}
	_ cancelreader.File = shiftEnterReader{}
)

func (s shiftEnterReader) Read(p []byte) (int, error) {
	n, err := s.f.Read(p)
	if n == 1 && p[0] == '\r' && len(p) >= len(kittyShiftEnter) && s.probe() {
		n = copy(p, kittyShiftEnter)
	}

	return n, err //nolint:wrapcheck // transparent pass-through of the underlying file's error
}

func (s shiftEnterReader) Write(p []byte) (int, error) {
	return s.f.Write(p) //nolint:wrapcheck // transparent pass-through
}

func (s shiftEnterReader) Close() error {
	return s.f.Close() //nolint:wrapcheck // transparent pass-through
}

func (s shiftEnterReader) Fd() uintptr  { return s.f.Fd() }
func (s shiftEnterReader) Name() string { return s.f.Name() }

// wrapInput returns r wrapped in a [shiftEnterReader] when the out-of-band
// fallback is both wanted and possible, and r unchanged otherwise: r is not an
// *os.File (tests, pipes), the platform has no modifier probe, or the operator
// disabled it. Returning r untouched is always safe — it only means shift+enter
// stays indistinguishable from enter on deaf terminals, exactly as before.
func wrapInput(r io.Reader, getenv func(string) string) io.Reader {
	f, ok := r.(*os.File)
	if !ok {
		return r
	}

	mode := strings.ToLower(strings.TrimSpace(getenv(shiftEnterEnv)))
	if mode == "off" || mode == "false" || mode == "0" {
		return r
	}

	// The probe reads the state of the machine's own keyboard. Over SSH the
	// keyboard is somewhere else entirely, so the answer would be noise.
	if mode != "on" && mode != "force" &&
		(getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "") {
		return r
	}

	probe := newShiftProbe()
	if probe == nil {
		return r
	}

	return shiftEnterReader{f: f, probe: probe}
}

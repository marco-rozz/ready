//go:build linux && !noshiftprobe

package tui

import (
	"os"

	"github.com/ebitengine/purego"
)

// Linux has no OS-wide "is shift down" query by design: Wayland hides global
// input state, and /dev/input needs the `input` group. What exists is X11's
// XQueryPointer, which also answers for XWayland clients.
//
// This is a residual net only. Every Linux terminal emulator in practical use
// encodes shift+enter in band (kitty keyboard protocol or xterm
// modifyOtherKeys), and Bubble Tea negotiates and decodes both on its own, so
// the fallback normally never fires here.
//
// Build-tag `noshiftprobe` excludes this file (see shiftstate_noprope.go):
// purego's cgo_import_dynamic forces a dynamically linked ELF even under
// CGO_ENABLED=0, whose /lib/ld-linux.so.3 interpreter does not exist on
// armhf systems (e.g. Raspberry Pi OS) — cross-builder switch for static
// builds on such targets.
const (
	libX11    = "libX11.so.6"
	shiftMask = 1 << 0
)

// newShiftProbe opens the X display once and returns a closure querying the
// pointer/modifier state, or nil when X11 is unavailable (no DISPLAY, pure
// Wayland, library missing).
func newShiftProbe() (probe func() bool) {
	if os.Getenv("DISPLAY") == "" {
		return nil
	}

	lib, err := purego.Dlopen(libX11, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil
	}

	var (
		openDisplay  func(name *byte) uintptr
		defaultRoot  func(dpy uintptr) uint64
		queryPointer func(dpy uintptr, w uint64, root, child *uint64, rx, ry, wx, wy *int32, mask *uint32) int32
	)

	// RegisterLibFunc panics on a missing symbol; degrade to "no probe".
	defer func() {
		if r := recover(); r != nil {
			probe = nil
		}
	}()

	purego.RegisterLibFunc(&openDisplay, lib, "XOpenDisplay")
	purego.RegisterLibFunc(&defaultRoot, lib, "XDefaultRootWindow")
	purego.RegisterLibFunc(&queryPointer, lib, "XQueryPointer")

	dpy := openDisplay(nil)
	if dpy == 0 {
		return nil
	}

	root := defaultRoot(dpy)

	return func() bool {
		var (
			rootRet, childRet    uint64
			rootX, rootY, wX, wY int32
			mask                 uint32
		)

		if queryPointer(dpy, root, &rootRet, &childRet, &rootX, &rootY, &wX, &wY, &mask) == 0 {
			return false
		}

		return mask&shiftMask != 0
	}
}

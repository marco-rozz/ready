//go:build darwin && !noshiftprobe

package tui

import "github.com/ebitengine/purego"

// macOS: CGEventSourceFlagsState reports the current modifier state of the
// session and CGEventSourceKeyState the state of a single virtual keycode.
// Both are state queries, not event taps, so no Accessibility or
// Input-Monitoring grant is involved. purego dlopen's the framework at
// runtime, so the binary keeps building with CGO_ENABLED=0 and needs no C
// toolchain.
const (
	coreGraphicsPath = "/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics"

	stateCombinedSession = 0

	// kCGEventFlagMaskShift.
	maskShift = 1 << 17

	vkLeftShift  = 56
	vkRightShift = 60
)

// newShiftProbe binds the CoreGraphics entry points once and returns a closure
// querying them, or nil when the framework or the symbols are unavailable.
func newShiftProbe() (probe func() bool) {
	lib, err := purego.Dlopen(coreGraphicsPath, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil
	}

	var (
		flagsState func(stateID int32) uint64
		keyState   func(stateID int32, keycode uint16) bool
	)

	// RegisterLibFunc panics on a missing symbol; degrade to "no probe"
	// instead of taking the process down over a keybinding nicety.
	defer func() {
		if r := recover(); r != nil {
			probe = nil
		}
	}()

	purego.RegisterLibFunc(&flagsState, lib, "CGEventSourceFlagsState")
	purego.RegisterLibFunc(&keyState, lib, "CGEventSourceKeyState")

	return func() bool {
		if flagsState(stateCombinedSession)&maskShift != 0 {
			return true
		}

		return keyState(stateCombinedSession, vkLeftShift) ||
			keyState(stateCombinedSession, vkRightShift)
	}
}

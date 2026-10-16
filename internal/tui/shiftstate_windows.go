//go:build windows

package tui

import "golang.org/x/sys/windows"

// Windows: GetAsyncKeyState reports the physical key state at call time,
// independently of the thread's message queue, which is what a console
// application needs. NewLazySystemDLL resolves user32.dll from System32 only.
// Plain syscall, no cgo.
const (
	vkShift     = 0x10
	keyDownMask = 0x8000
)

// newShiftProbe resolves user32!GetAsyncKeyState once and returns a closure
// querying it, or nil when the proc cannot be resolved.
func newShiftProbe() func() bool {
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("GetAsyncKeyState")
	if err := proc.Find(); err != nil {
		return nil
	}

	return func() bool {
		r, _, _ := proc.Call(uintptr(vkShift))

		return uint16(r)&keyDownMask != 0
	}
}

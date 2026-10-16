//go:build !darwin && !linux && !windows

package tui

// newShiftProbe has no implementation on this platform: there is no OS-level
// modifier query, so the fallback stays off and only the terminal's own
// in-band encoding (kitty protocol / modifyOtherKeys) is used.
func newShiftProbe() func() bool { return nil }

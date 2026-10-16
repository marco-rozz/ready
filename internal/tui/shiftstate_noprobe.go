//go:build (linux || darwin) && noshiftprobe

package tui

// noshiftprobe build-tag stub: replaces the purego-based SHIFT+RETURN probes
// (shiftstate_linux.go / shiftstate_darwin.go). purego's cgo_import_dynamic
// forces a dynamically linked ELF even under CGO_ENABLED=0, whose
// /lib/ld-linux.so.3 interpreter does not exist on armhf systems (e.g.
// Raspberry Pi OS). With this tag the binary links statically.
//
// The fallback stays off: only the terminal's own in-band shift+enter encoding
// (kitty protocol / modifyOtherKeys) is used.
func newShiftProbe() func() bool { return nil }

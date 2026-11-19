//go:build windows

package main

import "github.com/rs/zerolog"

// wireGoroutineDumpSignal is a no-op on Windows: there is no SIGUSR1, so the
// goroutine-dump diagnostic is unavailable there (the Unix build installs it).
func wireGoroutineDumpSignal(_ zerolog.Logger) {}

//go:build !windows

package main

import (
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/rs/zerolog"
)

// wireGoroutineDumpSignal installs a permanent operator diagnostic: SIGUSR1
// dumps every goroutine's stack (plus GC/build metadata) into the log file,
// where it can be analysed after the fact. Non-destructive by design — the
// daemon keeps running; use it whenever the process looks wedged and a
// Ctrl+\ (SIGQUIT, dump + exit) is not acceptable. Never blocks shutdown.
func wireGoroutineDumpSignal(logger zerolog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)

	go func() {
		for range ch {
			buf := make([]byte, 4<<20)
			n := runtime.Stack(buf, true)
			logger.Info().
				Bytes("stack", buf[:n]).
				Str("go_version", runtime.Version()).
				Msg("goroutine dump requested (SIGUSR1)")
			runtime.GC()
		}
	}()
}

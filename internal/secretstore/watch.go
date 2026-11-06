package secretstore

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchDebounce coalesces the burst of events one atomic replace produces.
const watchDebounce = 100 * time.Millisecond

// Watch calls onChange after secrets.age changes (create, write, rename,
// remove) until ctx is done. Events for any other file of the agent directory
// are ignored. The directory is watched rather than the file because every
// write replaces the file.
func Watch(ctx context.Context, dir string, onChange func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create store watcher: %w", err)
	}
	if err = w.Add(dir); err != nil {
		_ = w.Close()

		return fmt.Errorf("failed to watch %s: %w", dir, err)
	}
	target := filepath.Join(dir, StoreFileName)
	go func() {
		defer func() { _ = w.Close() }()
		var timer *time.Timer
		var fire <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}

				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Clean(ev.Name) != target {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(watchDebounce)
				} else {
					timer.Reset(watchDebounce)
				}
				fire = timer.C
			case _, ok := <-w.Errors:
				// A dropped event only delays recomputation to the next change.
				if !ok {
					return
				}
			case <-fire:
				fire = nil
				onChange()
			}
		}
	}()

	return nil
}

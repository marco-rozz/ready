package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RetentionPolicy bounds the ended Sessions kept on disk.
type RetentionPolicy struct {
	// Days keeps ended Sessions whose name's day falls within the last Days
	// calendar days, today included (0 = delete every ended Session).
	Days int
	// MaxTotalSize caps the total bytes of ended Sessions across addresses;
	// the oldest are deleted first (0 = no cap).
	MaxTotalSize int64
}

// Retain runs one retention pass over every address directory: an ended
// Session (a file that is not its address's current one) is deleted when its
// name's day falls outside the window, then further ended files are deleted
// oldest-first, across addresses, while their total size exceeds the cap.
// Only scheme-matching files are ever touched and a current Session is never
// deleted. Returns the deleted paths.
func (s *Store) Retain(ctx context.Context, policy RetentionPolicy, now time.Time) ([]string, error) {
	dirs, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to list sessions root: %w", err)
	}

	var ended []sessionFile

	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("retention interrupted: %w", err)
		}

		if !d.IsDir() {
			continue
		}

		files, err := listFiles(filepath.Join(s.root, d.Name()))
		if err != nil {
			s.logger.Warn().Err(err).Str("dir", d.Name()).Msg("retention: failed to list address directory")

			continue
		}

		if len(files) == 0 {
			continue
		}

		// The newest file is the current Session unless it is closed.
		last := len(files) - 1
		if closed, cerr := isClosed(files[last].path); cerr == nil && closed {
			last++
		}

		ended = append(ended, files[:last]...)
	}

	y, m, d := now.Date()
	cutoff := time.Date(y, m, d-(policy.Days-1), 0, 0, 0, 0, time.Local)

	var deleted []string
	var kept []sessionFile

	for _, f := range ended {
		if policy.Days == 0 || f.day.Before(cutoff) {
			if s.remove(f.path) {
				deleted = append(deleted, f.path)
			}

			continue
		}

		kept = append(kept, f)
	}

	if policy.MaxTotalSize > 0 {
		sort.Slice(kept, func(i, j int) bool { return kept[i].id < kept[j].id })

		var total int64
		for _, f := range kept {
			total += f.size
		}

		for _, f := range kept {
			if total <= policy.MaxTotalSize {
				break
			}

			if s.remove(f.path) {
				deleted = append(deleted, f.path)
				total -= f.size
			}
		}
	}

	return deleted, nil
}

// remove deletes one ended Session file, logging a failure.
func (s *Store) remove(path string) bool {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn().Err(err).Str("file", path).Msg("retention: failed to delete ended session")

		return false
	}

	s.logger.Info().Str("file", path).Msg("retention: ended session deleted")

	return true
}

// RunRetention starts the background retention worker: one pass right away
// (boot), then one per trigger (after each Session end), coalesced so
// passes never overlap. It stops when ctx is cancelled.
func (s *Store) RunRetention(ctx context.Context, policy RetentionPolicy) (trigger func()) {
	kick := make(chan struct{}, 1)
	kick <- struct{}{}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
				if _, err := s.Retain(ctx, policy, time.Now()); err != nil {
					s.logger.Warn().Err(err).Msg("session retention pass failed")
				}
			}
		}
	}()

	return func() {
		select {
		case kick <- struct{}{}:
		default: // a pass is already queued
		}
	}
}

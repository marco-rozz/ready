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
// Session (a folder that is not its address's current one) is deleted when
// its name's day falls outside the window, then further ended trees are
// deleted oldest-first, across addresses, while their total size exceeds the
// cap. A whole tree is removed (the folder incl. its subagents/ subtree);
// only scheme-matching folders are ever touched and a current Session is
// never deleted. Returns the deleted paths.
func (s *Store) Retain(ctx context.Context, policy RetentionPolicy, now time.Time) ([]string, error) {
	addressDirs, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to list sessions root: %w", err)
	}

	var ended []sessionDir

	for _, d := range addressDirs {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("retention interrupted: %w", err)
		}

		if !d.IsDir() {
			continue
		}

		folders, err := listSessionDirs(filepath.Join(s.root, d.Name()))
		if err != nil {
			s.logger.Warn().Err(err).Str("dir", d.Name()).Msg("retention: failed to list address directory")

			continue
		}

		if len(folders) == 0 {
			continue
		}

		// The newest folder is the current Session unless it is closed.
		last := len(folders) - 1
		if closed, cerr := isClosed(folders[last].file); cerr == nil && closed {
			last++
		}

		ended = append(ended, folders[:last]...)
	}

	y, m, d := now.Date()
	cutoff := time.Date(y, m, d-(policy.Days-1), 0, 0, 0, 0, time.Local)

	var deleted []string
	var kept []sessionDir

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

// remove deletes one ended Session tree, logging a failure.
func (s *Store) remove(path string) bool {
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.Warn().Err(err).Str("folder", path).Msg("retention: failed to delete ended session tree")

		return false
	}

	s.logger.Info().Str("folder", path).Msg("retention: ended session tree deleted")

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

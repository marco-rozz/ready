package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/rs/zerolog"
)

// errReloadClosed is returned once the daemon is shutting down.
var errReloadClosed = errors.New("runtime reload refused: the daemon is shutting down")

// errDrainTimeout is returned when in-flight turns do not finish within the
// drain grace; the previous generation then stays active.
var errDrainTimeout = errors.New("in-flight turns did not finish in time")

// msgReloadFailed is the single fixed warning an originating non-TUI channel
// receives when an automatic Runtime reload fails.
const msgReloadFailed = "The configuration was saved, but the runtime reload failed; the previous configuration stays active."

// reloadCoordinator serializes Runtime reloads outside the dispatcher's
// per-address workers. A reload builds the next generation first (the old one
// keeps serving), then drains in-flight items — the sessionRuntime's
// generation lock is the turn-admission gate: it is held for reading by every
// item and for writing by the swap — installs the new generation, and only
// then releases the old one.
type reloadCoordinator struct {
	rt *sessionRuntime
	// build constructs the next generation. It owns its context lifetime.
	build func(context.Context) (*runtimeGeneration, error)
	// afterSwap runs after the swap, outside the generation lock.
	afterSwap func(g *runtimeGeneration, fromTUI bool)
	grace     time.Duration
	logger    zerolog.Logger

	mu      sync.Mutex
	current *runtimeGeneration
	closed  bool

	autoPending atomic.Bool
	wg          sync.WaitGroup
}

func (c *reloadCoordinator) currentGen() *runtimeGeneration {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.current
}

// Reload rebuilds the runtime in-process. Any failure leaves the previous
// generation active and is returned to the caller.
func (c *reloadCoordinator) Reload(ctx context.Context, fromTUI bool) (*runtimeGeneration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, errReloadClosed
	}

	next, err := c.build(ctx)
	if err != nil {
		c.logger.Error().Err(err).Msg("runtime reload failed; the previous generation stays active")

		return nil, fmt.Errorf("runtime reload failed; the previous configuration stays active: %w", err)
	}

	if err = c.rt.swapGeneration(ctx, next, c.grace); err != nil {
		next.shutdown(ctx, c.logger)
		c.logger.Error().Err(err).Msg("runtime reload aborted; the previous generation stays active")

		return nil, fmt.Errorf("runtime reload aborted; the previous configuration stays active: %w", err)
	}

	old := c.current
	c.current = next

	if c.afterSwap != nil {
		c.afterSwap(next, fromTUI)
	}
	old.shutdown(ctx, c.logger)
	c.logger.Info().Bool("from_tui", fromTUI).Msg("runtime reload complete")

	return next, nil
}

// RequestAutoReload schedules an asynchronous reload for a save committed on a
// non-TUI channel. It never blocks (the caller is a dispatcher worker that has
// already released its generation lock); concurrent requests coalesce.
func (c *reloadCoordinator) RequestAutoReload(ctx context.Context, origin channel.Message) {
	if ctx.Err() != nil || !c.autoPending.CompareAndSwap(false, true) {
		return
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.autoPending.Store(false)

		if _, err := c.Reload(ctx, false); err != nil && !errors.Is(err, errReloadClosed) {
			if origin.ChannelName == telegram.ChannelName && ctx.Err() == nil {
				c.rt.sendFixed(ctx, origin, msgReloadFailed)
			}
		}
	}()
}

// Close stops further reloads, waits for scheduled ones, and returns the
// generation to shut down.
func (c *reloadCoordinator) Close() *runtimeGeneration {
	c.mu.Lock()
	c.closed = true
	cur := c.current
	c.mu.Unlock()
	c.wg.Wait()

	return cur
}

// swapGeneration drains in-flight items and installs next. It returns
// errDrainTimeout (leaving the previous generation in place) when the drain
// does not complete within grace.
func (rt *sessionRuntime) swapGeneration(ctx context.Context, next *runtimeGeneration, grace time.Duration) error {
	const (
		pending int32 = iota
		acquired
		abandoned
	)

	var state atomic.Int32
	locked := make(chan struct{})

	go func() {
		rt.genMu.Lock()
		if !state.CompareAndSwap(pending, acquired) {
			rt.genMu.Unlock() // the caller gave up: never keep the gate closed

			return
		}
		close(locked)
	}()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-locked:
	case <-timer.C:
		if state.CompareAndSwap(pending, abandoned) {
			return errDrainTimeout
		}
	case <-ctx.Done():
		if state.CompareAndSwap(pending, abandoned) {
			return fmt.Errorf("runtime reload cancelled while draining turns: %w", ctx.Err())
		}
	}

	defer rt.genMu.Unlock()
	rt.applyGeneration(next)

	return nil
}

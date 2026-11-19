package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marco-rozz/ready/internal/channel"
	"github.com/marco-rozz/ready/internal/channel/telegram"
	"github.com/marco-rozz/ready/internal/config"
	"github.com/rs/zerolog"
)

func TestGenerationStageErrorAddsContextAndPreservesCause(t *testing.T) {
	cause := errors.New("original failure")

	err := generationStageError("prepare boot secrets", cause)
	if err == nil || !strings.Contains(err.Error(), "failed to prepare boot secrets") {
		t.Fatalf("error = %v, want stage context", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want wrapped original cause", err)
	}
}

// swapGuardChannel implements channel.Channel with an observable Close.
type swapGuardChannel struct {
	name   string
	closed atomic.Bool
}

func (c *swapGuardChannel) Name() string { return c.name }

func (c *swapGuardChannel) Receive(ctx context.Context) (channel.Message, error) {
	<-ctx.Done()

	return channel.Message{}, ctx.Err()
}

func (c *swapGuardChannel) Send(context.Context, channel.Message) error { return nil }

func (c *swapGuardChannel) Close() error {
	c.closed.Store(true)

	return nil
}

// A runtime reload landing while the daemon is shutting down (SIGINT racing
// the config-save reload) must never (re)start channels — the previous
// incident left a phantom Telegram poller alive behind the shutdown path.
func TestSwapChannelsSkippedWhenDaemonShuttingDown(t *testing.T) {
	stub := &swapGuardChannel{name: telegram.ChannelName}
	mgr := channel.NewManager(zerolog.Nop(), stub)
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop() })

	shellCtx, cancelShell := context.WithCancel(context.Background())
	cancelShell()
	shell := &serveShell{ctx: shellCtx, mgr: mgr, logger: zerolog.Nop()}

	gen := testGeneration()
	gen.bootCfg = &config.Config{} // telegram disabled: only the stop path would run
	shell.swapChannels(gen)

	if stub.closed.Load() {
		t.Fatal("swapChannels must not touch channels while the daemon is shutting down")
	}
}

// Without shutdown in progress the swap still stops the previous Telegram
// channel (regression guard for the guard itself).
func TestSwapChannelsStopsPreviousTelegramWhenRunning(t *testing.T) {
	stub := &swapGuardChannel{name: telegram.ChannelName}
	mgr := channel.NewManager(zerolog.Nop(), stub)
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Stop() })

	shell := &serveShell{ctx: context.Background(), mgr: mgr, logger: zerolog.Nop()}

	gen := testGeneration()
	gen.bootCfg = &config.Config{} // telegram disabled: StopChannel, no rebuild
	shell.swapChannels(gen)

	if !stub.closed.Load() {
		t.Fatal("swapChannels must stop the previous Telegram channel")
	}
}

// --- subagent-runtime task 3.4: generation leases ---

func TestGenerationLeases_DeferredRetirement(t *testing.T) {
	var retired atomic.Int32
	g1 := &runtimeGeneration{cancel: func() {}}
	g2 := &runtimeGeneration{cancel: func() {}}
	l := newGenerationLeases(g1, zerolog.Nop())
	l.retire = func(context.Context, *runtimeGeneration) { retired.Add(1) }

	// A child holds a lease on g1; the swap must not wait for it.
	release := func() func() {
		_, rel := l.Acquire(context.Background())

		return rel
	}()
	l.Swap(context.Background(), g2)

	if l.PendingRetired() != 1 {
		t.Fatalf("pending retired = %d", l.PendingRetired())
	}
	if retired.Load() != 0 {
		t.Fatal("leased generation retired during swap")
	}

	// Release: retirement runs.
	release()
	if retired.Load() != 1 {
		t.Fatalf("retired = %d after release", retired.Load())
	}
	if l.PendingRetired() != 0 {
		t.Fatal("deferred generation not cleared")
	}
}

func TestGenerationLeases_ImmediateRetirementWhenUnleased(t *testing.T) {
	var retired atomic.Int32
	g1 := &runtimeGeneration{cancel: func() {}}
	g2 := &runtimeGeneration{cancel: func() {}}
	l := newGenerationLeases(g1, zerolog.Nop())
	l.retire = func(context.Context, *runtimeGeneration) { retired.Add(1) }

	l.Swap(context.Background(), g2)
	if retired.Load() != 1 {
		t.Fatalf("unleased previous not retired immediately: %d", retired.Load())
	}
}

func TestGenerationLeases_ShutdownAllRetiresEverything(t *testing.T) {
	var retired atomic.Int32
	g1 := &runtimeGeneration{cancel: func() {}}
	g2 := &runtimeGeneration{cancel: func() {}}
	g3 := &runtimeGeneration{cancel: func() {}}
	l := newGenerationLeases(g1, zerolog.Nop())
	l.retire = func(context.Context, *runtimeGeneration) { retired.Add(1) }

	_, rel := l.Acquire(context.Background())
	l.Swap(context.Background(), g2)
	_, rel2 := l.Acquire(context.Background())
	l.Swap(context.Background(), g3)

	l.ShutdownAll(context.Background())
	rel()
	rel2() // releases after ShutdownAll must not double-retire

	if got := retired.Load(); got != 3 {
		t.Fatalf("retired = %d, want 3 (g1, g2, g3)", got)
	}
}

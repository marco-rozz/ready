package tui

import (
	"context"
	"io"
	"os"
	"os/signal"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/marco-rozz/ready/internal/privacy"
	"github.com/marco-rozz/ready/internal/protocol"
)

// homeDir returns the user's home directory, wrapping os.UserHomeDir. A
// plain function (not a global var) keeps gochecknoglobals happy; the echo
// redaction test calls it to build a matching sanitizer and does not need to
// override it.
func homeDir() (string, error) {
	return os.UserHomeDir()
}

// RunOptions configures Run.
type RunOptions struct {
	ConnectionID string
	// SessionID is the conversation Session ID from the authenticated
	// handshake (session-lifecycle), shown by the info block and replaced on
	// every session_changed event.
	SessionID    string
	ReplyTimeout time.Duration
	Sanitizer    *privacy.Sanitizer
	Stdin        io.Reader
	Stdout       io.Writer
	// ModelRef / ModelState seed the status line's model region from the
	// authenticated handshake response (llm-status-visibility).
	ModelRef   string
	ModelState protocol.ModelState
	// Usage seeds the status line's usage segment from the authenticated
	// handshake response (tui-usage-observability); nil when fresh.
	Usage *protocol.UsageInfo
	// LivenessTimeout is the daemon's heartbeat window from the handshake
	// (zmq-session-liveness); the TUI pings every third of it. Zero (old
	// daemon, metadata absent) keeps the built-in default interval.
	LivenessTimeout time.Duration
	// LogoMode selects the viewport's leading presentation (banner-in-
	// transcript): LogoGlyph renders the glyph banner inside the viewport
	// above the info block; LogoNone (off mode) leads with the info block
	// alone. The PNG banner itself never enters the TUI — cmd/chat.go prints
	// it on the main screen before the alt-screen program starts; the same
	// glyph art IS rendered inside the viewport on every non-off run.
	LogoMode LogoMode
	// DaemonAddress / ClientVersion are static info-block facts
	// (chat-tui-logo-modes task 4.1), alongside ModelRef and SessionID above.
	DaemonAddress string
	ClientVersion string
}

// Run drives the interactive TUI to completion and returns the final model
// and the program error. The dispatcher maps the model's ExitReason (and the
// program error, e.g. tea.ErrInterrupted for an out-of-band SIGINT) to the
// process exit code.
//
// A residual signal handler closes the dealer on an out-of-band os.Interrupt
// so the in-flight recv command (blocked on dealer.Recv, which does not
// honour context cancellation) unblocks and Program.Run can tear down.
// Bubble Tea's own signal handler converts that SIGINT into tea.ErrInterrupted.
func Run(ctx context.Context, d dealer, opts RunOptions) (*Model, error) {
	m := NewModel(d, Options{
		ConnectionID:    opts.ConnectionID,
		SessionID:       opts.SessionID,
		ReplyTimeout:    opts.ReplyTimeout,
		Sanitizer:       opts.Sanitizer,
		ModelRef:        opts.ModelRef,
		ModelState:      opts.ModelState,
		Usage:           opts.Usage,
		LivenessTimeout: opts.LivenessTimeout,
		LogoMode:        opts.LogoMode,
		DaemonAddress:   opts.DaemonAddress,
		ClientVersion:   opts.ClientVersion,
	})

	prog := tea.NewProgram(
		m,
		tea.WithInput(wrapInput(opts.Stdin, os.Getenv)),
		tea.WithOutput(opts.Stdout),
		tea.WithContext(ctx),
	)

	var closeOnce sync.Once
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	go func() {
		select {
		case <-sigCh:
			closeOnce.Do(func() { _ = d.Close() })
		case <-ctx.Done():
		}
	}()

	finalModel, err := prog.Run()
	closeOnce.Do(func() { _ = d.Close() }) // ensure the recv unblocks on any exit

	if finalModel != nil {
		if mm, ok := finalModel.(*Model); ok {
			m = mm
		}
	}

	return m, err
}
